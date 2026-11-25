package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/fattyatom/clusterdeck/internal/kube"
	"github.com/fattyatom/clusterdeck/internal/metrics"
)

type Cluster interface {
	Overview(ctx context.Context) (*kube.Overview, error)
	Apps(ctx context.Context) ([]kube.App, error)
	ScheduledJobs(ctx context.Context) ([]kube.ScheduledJob, error)
	RestoreTemplates(ctx context.Context) ([]kube.RestoreTemplate, error)
	PodLogs(ctx context.Context, ns, pod, container string, tail int64) (string, error)
	RunCronJob(ctx context.Context, ns, name, user string) (string, error)
	RunRestore(ctx context.Context, ns, name string, params map[string]string, confirm, user string) (string, error)
	RestartWorkload(ctx context.Context, ns, kind, name string) error
	Rollouts(ctx context.Context, ns, app string) ([]kube.Rollout, error)
	Rollback(ctx context.Context, ns, name string, toRevision int64, user string) (string, error)
	RollbackPR(ctx context.Context, appNS, app, toRevision, user string) (string, error)
}

type Login interface {
	User(r *http.Request) string
	LoginURL(next string) string
	Routes(mux *http.ServeMux)
}

type Options struct {
	ClusterName    string
	AuthHeader     string
	Login          Login
	AllowedUsers   []string
	ActionsEnabled bool
	AutoRollback   bool
	GitRollback    bool
	Static         fs.FS
	Logger         *slog.Logger
	Now            func() time.Time
}

type Server struct {
	cluster Cluster
	metrics metrics.Provider
	opts    Options
	allowed map[string]bool
	mux     *http.ServeMux
}

const (
	anonymous     = "anonymous"
	actionHeader  = "X-Requested-With"
	actionValue   = "clusterdeck"
	maxBodyBytes  = 64 << 10
	defaultWindow = time.Hour
)

var windows = map[string]time.Duration{
	"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
}

func New(cluster Cluster, provider metrics.Provider, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if provider == nil {
		provider = metrics.None{}
	}
	s := &Server{cluster: cluster, metrics: provider, opts: opts, allowed: map[string]bool{}, mux: http.NewServeMux()}
	for _, u := range opts.AllowedUsers {
		s.allowed[strings.ToLower(u)] = true
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	if opts.Login != nil {
		opts.Login.Routes(s.mux)
	}
	s.mux.Handle("GET /api/config", s.read(s.handleConfig))
	s.mux.Handle("GET /api/overview", s.read(s.handleOverview))
	s.mux.Handle("GET /api/apps", s.read(s.handleApps))
	s.mux.Handle("GET /api/backups", s.read(s.handleBackups))
	s.mux.Handle("GET /api/restores", s.read(s.handleRestores))
	s.mux.Handle("GET /api/metrics/{panel}", s.read(s.handleMetrics))
	s.mux.Handle("GET /api/pods/{ns}/{pod}/logs", s.read(s.handleLogs))
	s.mux.Handle("GET /api/rollouts/{ns}/{app}", s.read(s.handleRollouts))
	s.mux.Handle("POST /api/deployments/{ns}/{name}/rollback", s.action(s.handleRollback))
	s.mux.Handle("POST /api/argo/{ns}/{app}/rollback-pr", s.action(s.handleRollbackPR))
	s.mux.Handle("POST /api/cronjobs/{ns}/{name}/run", s.action(s.handleRunCronJob))
	s.mux.Handle("POST /api/restores/{ns}/{name}/run", s.action(s.handleRunRestore))
	s.mux.Handle("POST /api/workloads/{ns}/{kind}/{name}/restart", s.action(s.handleRestart))
	if opts.Static != nil {
		s.mux.Handle("GET /", s.authenticated(http.FileServerFS(opts.Static)))
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
	s.mux.ServeHTTP(w, r)
}

type userKey struct{}

func userFrom(r *http.Request) string {
	if u, ok := r.Context().Value(userKey{}).(string); ok {
		return u
	}
	return anonymous
}

func (s *Server) identify(r *http.Request) (string, int) {
	var user string
	switch {
	case s.opts.Login != nil:
		user = s.opts.Login.User(r)
	case s.opts.AuthHeader != "":
		user = strings.TrimSpace(r.Header.Get(s.opts.AuthHeader))
	default:
		return anonymous, 0
	}
	if user == "" {
		return "", http.StatusUnauthorized
	}
	if len(s.allowed) > 0 && !s.allowed[strings.ToLower(user)] {
		return "", http.StatusForbidden
	}
	return user, 0
}

func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, status := s.identify(r)
		if status == http.StatusUnauthorized && s.opts.Login != nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, status, map[string]string{"error": "sign in required", "login": s.opts.Login.LoginURL("/")})
			} else {
				http.Redirect(w, r, s.opts.Login.LoginURL(r.URL.RequestURI()), http.StatusFound)
			}
			return
		}
		if status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	})
}

func (s *Server) read(h func(http.ResponseWriter, *http.Request) error) http.Handler {
	return s.authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.fail(w, r, err)
		}
	}))
}

func (s *Server) action(h func(http.ResponseWriter, *http.Request) (string, error)) http.Handler {
	return s.authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := userFrom(r)
		if !s.opts.ActionsEnabled || user == anonymous {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "actions are disabled"})
			return
		}
		if r.Header.Get(actionHeader) != actionValue {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing " + actionHeader + " header"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		result, err := h(w, r)
		attrs := []any{"user", user, "action", r.Pattern, "path", r.URL.Path}
		if err != nil {
			s.opts.Logger.Warn("action refused or failed", append(attrs, "error", err.Error())...)
			s.fail(w, r, err)
			return
		}
		s.opts.Logger.Info("action", append(attrs, "result", result)...)
		writeJSON(w, http.StatusOK, map[string]string{"result": result})
	}))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, kube.ErrInvalid), errors.Is(err, metrics.ErrUnknownPanel):
		status = http.StatusBadRequest
	case errors.Is(err, kube.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, metrics.ErrNotConfigured):
		status = http.StatusNotImplemented
	case apierrors.IsNotFound(err):
		status = http.StatusNotFound
	case apierrors.IsForbidden(err):
		status = http.StatusForbidden
	}
	if status == http.StatusInternalServerError {
		s.opts.Logger.Error("request failed", "path", r.URL.Path, "error", err.Error())
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]any{
		"clusterName":     s.opts.ClusterName,
		"user":            userFrom(r),
		"actionsEnabled":  s.opts.ActionsEnabled && userFrom(r) != anonymous,
		"metricsProvider": s.metrics.Name(),
		"autoRollback":    s.opts.AutoRollback,
		"gitRollback":     s.opts.GitRollback,
		"panels":          metrics.Panels,
		"logout":          s.logoutPath(),
	})
	return nil
}

func (s *Server) logoutPath() string {
	if s.opts.Login == nil {
		return ""
	}
	return "/auth/logout"
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) error {
	ov, err := s.cluster.Overview(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ov)
	return nil
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) error {
	apps, err := s.cluster.Apps(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, apps)
	return nil
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) error {
	jobs, err := s.cluster.ScheduledJobs(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, jobs)
	return nil
}

func (s *Server) handleRestores(w http.ResponseWriter, r *http.Request) error {
	templates, err := s.cluster.RestoreTemplates(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, templates)
	return nil
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) error {
	window := defaultWindow
	if q := r.URL.Query().Get("range"); q != "" {
		d, ok := windows[q]
		if !ok {
			return errors.Join(kube.ErrInvalid, errors.New("range must be one of 1h, 6h, 24h, 7d"))
		}
		window = d
	}
	end := s.opts.Now()
	series, err := s.metrics.Query(r.Context(), r.PathValue("panel"), end.Add(-window), end, metrics.StepFor(window))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, series)
	return nil
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) error {
	tail := int64(200)
	if q := r.URL.Query().Get("tail"); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n < 1 || n > 5000 {
			return errors.Join(kube.ErrInvalid, errors.New("tail must be 1-5000"))
		}
		tail = n
	}
	logs, err := s.cluster.PodLogs(r.Context(), r.PathValue("ns"), r.PathValue("pod"), r.URL.Query().Get("container"), tail)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, err = w.Write([]byte(logs))
	return err
}

func (s *Server) handleRollouts(w http.ResponseWriter, r *http.Request) error {
	rollouts, err := s.cluster.Rollouts(r.Context(), r.PathValue("ns"), r.PathValue("app"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rollouts)
	return nil
}

func (s *Server) handleRollback(_ http.ResponseWriter, r *http.Request) (string, error) {
	var body struct {
		Revision int64 `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", errors.Join(kube.ErrInvalid, err)
	}
	if body.Revision < 0 {
		return "", errors.Join(kube.ErrInvalid, errors.New("revision must be 0 (previous) or a revision number"))
	}
	return s.cluster.Rollback(r.Context(), r.PathValue("ns"), r.PathValue("name"), body.Revision, userFrom(r))
}

func (s *Server) handleRollbackPR(_ http.ResponseWriter, r *http.Request) (string, error) {
	var body struct {
		Revision string `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", errors.Join(kube.ErrInvalid, err)
	}
	return s.cluster.RollbackPR(r.Context(), r.PathValue("ns"), r.PathValue("app"), body.Revision, userFrom(r))
}

func (s *Server) handleRunCronJob(_ http.ResponseWriter, r *http.Request) (string, error) {
	return s.cluster.RunCronJob(r.Context(), r.PathValue("ns"), r.PathValue("name"), userFrom(r))
}

func (s *Server) handleRunRestore(_ http.ResponseWriter, r *http.Request) (string, error) {
	var body struct {
		Confirm string            `json:"confirm"`
		Params  map[string]string `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", errors.Join(kube.ErrInvalid, err)
	}
	return s.cluster.RunRestore(r.Context(), r.PathValue("ns"), r.PathValue("name"), body.Params, body.Confirm, userFrom(r))
}

func (s *Server) handleRestart(_ http.ResponseWriter, r *http.Request) (string, error) {
	ns, kind, name := r.PathValue("ns"), r.PathValue("kind"), r.PathValue("name")
	if err := s.cluster.RestartWorkload(r.Context(), ns, kind, name); err != nil {
		return "", err
	}
	return "restarted " + kind + "/" + name, nil
}
