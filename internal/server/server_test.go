package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fattyatom/clusterdeck/internal/inflight"
	"github.com/fattyatom/clusterdeck/internal/kube"
	"github.com/fattyatom/clusterdeck/internal/metrics"
)

type fakeCluster struct {
	ranCronJob string
	restore    struct {
		ns, name, confirm, user string
		params                  map[string]string
	}
	restarted  string
	rolledBack string
	rollbackPR string
	logQuery   kube.LogQuery
}

func (f *fakeCluster) Overview(context.Context) (*kube.Overview, error) {
	return &kube.Overview{Namespaces: 3}, nil
}
func (f *fakeCluster) Apps(context.Context) ([]kube.App, error) { return []kube.App{{Name: "a"}}, nil }
func (f *fakeCluster) ScheduledJobs(context.Context) ([]kube.ScheduledJob, error) {
	return []kube.ScheduledJob{}, nil
}
func (f *fakeCluster) RestoreTemplates(context.Context) ([]kube.RestoreTemplate, error) {
	return []kube.RestoreTemplate{}, nil
}
func (f *fakeCluster) PodLogs(_ context.Context, ns, pod string, q kube.LogQuery) (*kube.Logs, error) {
	f.logQuery = q
	return &kube.Logs{Lines: []kube.LogLine{{Level: "info", Text: ns + "/" + pod + "/" + q.Container}}}, nil
}
func (f *fakeCluster) RunCronJob(_ context.Context, ns, name, user string) (string, error) {
	f.ranCronJob = ns + "/" + name + " by " + user
	return name + "-manual-", nil
}
func (f *fakeCluster) RunRestore(_ context.Context, ns, name string, params map[string]string, confirm, user string) (string, error) {
	f.restore.ns, f.restore.name, f.restore.params, f.restore.confirm, f.restore.user = ns, name, params, confirm, user
	if confirm != ns+"/"+name {
		return "", kube.ErrInvalid
	}
	return "restore-", nil
}
func (f *fakeCluster) RestartWorkload(_ context.Context, ns, kind, name string) error {
	f.restarted = ns + "/" + kind + "/" + name
	return nil
}

func (f *fakeCluster) Rollouts(_ context.Context, ns, app string) ([]kube.Rollout, error) {
	return []kube.Rollout{{Namespace: ns, Name: app + "-web", Phase: "complete"}}, nil
}
func (f *fakeCluster) Rollback(_ context.Context, ns, name string, rev int64, user string) (string, error) {
	f.rolledBack = fmt.Sprintf("%s/%s@%d by %s", ns, name, rev, user)
	return "ok", nil
}

func (f *fakeCluster) RollbackPR(_ context.Context, ns, app, rev, user string) (string, error) {
	f.rollbackPR = fmt.Sprintf("%s/%s@%s by %s", ns, app, rev, user)
	return "opened pull request #1: https://example.com/1", nil
}

type fakeMetrics struct{ window time.Duration }

func (f *fakeMetrics) Name() string { return "fake" }
func (f *fakeMetrics) Query(_ context.Context, panel string, start, end time.Time, _ time.Duration) ([]metrics.Series, error) {
	if panel != "node_cpu" {
		return nil, metrics.ErrUnknownPanel
	}
	f.window = end.Sub(start)
	return []metrics.Series{{Label: "n1", Points: []metrics.Point{{T: 1, V: 2}}}}, nil
}

func newServer(opts Options) (*Server, *fakeCluster, *fakeMetrics, *bytes.Buffer) {
	logs := &bytes.Buffer{}
	opts.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	opts.Static = fstest.MapFS{"index.html": {Data: []byte("<html>")}}
	fc, fm := &fakeCluster{}, &fakeMetrics{}
	return New(fc, fm, opts), fc, fm, logs
}

func do(s http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

var authed = map[string]string{"X-Forwarded-Email": "me@example.com", "X-Requested-With": "clusterdeck"}

func TestAuthHeader(t *testing.T) {
	s, _, _, _ := newServer(Options{AuthHeader: "X-Forwarded-Email", AllowedUsers: []string{"Me@Example.com"}})
	if rec := do(s, "GET", "/api/overview", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no identity: %d", rec.Code)
	}
	if rec := do(s, "GET", "/", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("static files must need an identity too: %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/overview", "", map[string]string{"X-Forwarded-Email": "other@example.com"}); rec.Code != http.StatusForbidden {
		t.Fatalf("not on the allowlist: %d", rec.Code)
	}
	rec := do(s, "GET", "/api/overview", "", authed)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"namespaces":3`) {
		t.Fatalf("allowed user: %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "GET", "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz must not need auth: %d", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing CSP header")
	}
}

func TestActionsDisabledByDefault(t *testing.T) {
	s, fc, _, _ := newServer(Options{AuthHeader: "X-Forwarded-Email"})
	rec := do(s, "POST", "/api/cronjobs/shop/db-backup/run", "", authed)
	if rec.Code != http.StatusForbidden || fc.ranCronJob != "" {
		t.Fatalf("actions off must refuse: %d %q", rec.Code, fc.ranCronJob)
	}
	var cfg map[string]any
	json.Unmarshal(do(s, "GET", "/api/config", "", authed).Body.Bytes(), &cfg)
	if cfg["actionsEnabled"] != false || cfg["user"] != "me@example.com" || cfg["metricsProvider"] != "fake" {
		t.Fatalf("config = %v", cfg)
	}
}

func TestActionsNeverRunAnonymously(t *testing.T) {
	s, fc, _, _ := newServer(Options{ActionsEnabled: true})
	rec := do(s, "POST", "/api/cronjobs/shop/db-backup/run", "", map[string]string{"X-Requested-With": "clusterdeck"})
	if rec.Code != http.StatusForbidden || fc.ranCronJob != "" {
		t.Fatalf("anonymous action ran: %d", rec.Code)
	}
}

func TestActionNeedsCSRFHeader(t *testing.T) {
	s, fc, _, _ := newServer(Options{AuthHeader: "X-Forwarded-Email", ActionsEnabled: true})
	rec := do(s, "POST", "/api/cronjobs/shop/db-backup/run", "", map[string]string{"X-Forwarded-Email": "me@example.com"})
	if rec.Code != http.StatusForbidden || fc.ranCronJob != "" {
		t.Fatalf("missing X-Requested-With must refuse: %d", rec.Code)
	}
}

func TestActionsRunAndAreAudited(t *testing.T) {
	s, fc, _, logs := newServer(Options{AuthHeader: "X-Forwarded-Email", ActionsEnabled: true})

	rec := do(s, "POST", "/api/cronjobs/shop/db-backup/run", "", authed)
	if rec.Code != http.StatusOK || fc.ranCronJob != "shop/db-backup by me@example.com" {
		t.Fatalf("run: %d %q", rec.Code, fc.ranCronJob)
	}
	if !strings.Contains(logs.String(), `"user":"me@example.com"`) || !strings.Contains(logs.String(), `"msg":"action"`) {
		t.Fatalf("action not audited: %s", logs)
	}

	rec = do(s, "POST", "/api/restores/shop/restore-db/run", `{"confirm":"shop/restore-db","params":{"DUMP":"x.dump"}}`, authed)
	if rec.Code != http.StatusOK || fc.restore.params["DUMP"] != "x.dump" || fc.restore.user != "me@example.com" {
		t.Fatalf("restore: %d %+v", rec.Code, fc.restore)
	}
	rec = do(s, "POST", "/api/restores/shop/restore-db/run", `{"confirm":"nope"}`, authed)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad confirm should be 400, got %d", rec.Code)
	}
	if !strings.Contains(logs.String(), "action refused or failed") {
		t.Fatal("refusals must be audited too")
	}
	rec = do(s, "POST", "/api/restores/shop/restore-db/run", `not json`, authed)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json should be 400, got %d", rec.Code)
	}

	rec = do(s, "POST", "/api/workloads/shop/deployment/web/restart", "", authed)
	if rec.Code != http.StatusOK || fc.restarted != "shop/deployment/web" {
		t.Fatalf("restart: %d %q", rec.Code, fc.restarted)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s, _, fm, _ := newServer(Options{})
	rec := do(s, "GET", "/api/metrics/node_cpu?range=24h", "", nil)
	if rec.Code != http.StatusOK || fm.window != 24*time.Hour || !strings.Contains(rec.Body.String(), `"label":"n1"`) {
		t.Fatalf("metrics: %d %s window=%v", rec.Code, rec.Body, fm.window)
	}
	if rec := do(s, "GET", "/api/metrics/node_cpu?range=3y", "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad range: %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/metrics/zzz", "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown panel: %d", rec.Code)
	}
}

func TestMetricsNotConfigured(t *testing.T) {
	s := New(&fakeCluster{}, nil, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if rec := do(s, "GET", "/api/metrics/node_cpu", "", nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestLogsEndpoint(t *testing.T) {
	s, fc, _, _ := newServer(Options{})
	rec := do(s, "GET", "/api/pods/shop/web-1/logs?container=c&tail=7&since=2026-11-26T10:00:00Z&until=2026-11-26T11:00:00%2B01:00&level=Warn,%20error&q=%20timeout%20&previous=true", "", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("logs: %d %q", rec.Code, rec.Body)
	}
	var body kube.Logs
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Lines) != 1 || body.Lines[0].Text != "shop/web-1/c" {
		t.Fatalf("body: %v %q", err, rec.Body)
	}
	q := fc.logQuery
	since := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	if q.Container != "c" || q.Tail != 7 || !q.Since.Equal(since) || !q.Until.Equal(since) || q.Contains != "timeout" || !q.Previous ||
		strings.Join(q.Levels, ",") != "warn,error" {
		t.Fatalf("query: %+v", q)
	}

	do(s, "GET", "/api/pods/shop/web-1/logs", "", nil)
	if q := fc.logQuery; q.Tail != 500 || !q.Since.IsZero() || q.Levels != nil || q.Previous {
		t.Fatalf("defaults: %+v", q)
	}

	for _, bad := range []string{"tail=999999", "tail=0", "since=yesterday", "until=2026-11-26", "level=loud", "q=" + strings.Repeat("x", 201)} {
		if rec := do(s, "GET", "/api/pods/shop/web-1/logs?"+bad, "", nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
}

func TestRollouts(t *testing.T) {
	s, fc, _, _ := newServer(Options{AuthHeader: "X-Forwarded-Email", ActionsEnabled: true})
	rec := do(s, "GET", "/api/rollouts/shop/shop", "", authed)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"shop-web"`) {
		t.Fatalf("rollouts: %d %s", rec.Code, rec.Body)
	}
	rec = do(s, "POST", "/api/deployments/shop/web/rollback", `{"revision":4}`, authed)
	if rec.Code != http.StatusOK || fc.rolledBack != "shop/web@4 by me@example.com" {
		t.Fatalf("rollback: %d %q", rec.Code, fc.rolledBack)
	}
	if rec := do(s, "POST", "/api/deployments/shop/web/rollback", `{"revision":-1}`, authed); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative revision: %d", rec.Code)
	}
	ro, fc2, _, _ := newServer(Options{AuthHeader: "X-Forwarded-Email"})
	if rec := do(ro, "POST", "/api/deployments/shop/web/rollback", `{}`, authed); rec.Code != http.StatusForbidden || fc2.rolledBack != "" {
		t.Fatalf("read-only rollback: %d", rec.Code)
	}
}

func TestRollbackPR(t *testing.T) {
	s, fc, _, logs := newServer(Options{AuthHeader: "X-Forwarded-Email", ActionsEnabled: true, GitRollback: true})
	rec := do(s, "POST", "/api/argo/argocd/shop/rollback-pr", `{"revision":"abc1234"}`, authed)
	if rec.Code != http.StatusOK || fc.rollbackPR != "argocd/shop@abc1234 by me@example.com" || !strings.Contains(rec.Body.String(), "pull request #1") {
		t.Fatalf("rollback pr: %d %s %q", rec.Code, rec.Body, fc.rollbackPR)
	}
	if !strings.Contains(logs.String(), "rollback-pr") {
		t.Fatal("rollback PRs must be audited")
	}
	var cfg map[string]any
	json.Unmarshal(do(s, "GET", "/api/config", "", authed).Body.Bytes(), &cfg)
	if cfg["gitRollback"] != true {
		t.Fatalf("config = %v", cfg)
	}
	ro, fc2, _, _ := newServer(Options{AuthHeader: "X-Forwarded-Email", GitRollback: true})
	if rec := do(ro, "POST", "/api/argo/argocd/shop/rollback-pr", `{}`, authed); rec.Code != http.StatusForbidden || fc2.rollbackPR != "" {
		t.Fatalf("read-only: %d", rec.Code)
	}
}

type fakeLogin struct{}

func (fakeLogin) User(r *http.Request) string {
	if c, err := r.Cookie("s"); err == nil {
		return c.Value
	}
	return ""
}
func (fakeLogin) LoginURL(next string) string { return "/auth/login?next=" + next }
func (fakeLogin) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("login page")) })
}

func withSession(user string) map[string]string {
	return map[string]string{"Cookie": "s=" + user, "X-Requested-With": "clusterdeck"}
}

func TestLogin(t *testing.T) {
	s, fc, _, logs := newServer(Options{Login: fakeLogin{}, ActionsEnabled: true, AllowedUsers: []string{"me@example.com"}})

	rec := do(s, "GET", "/api/overview", "", nil)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"login":"/auth/login?next=/"`) {
		t.Fatalf("api without a session: %d %s", rec.Code, rec.Body)
	}
	rec = do(s, "GET", "/index.html?x=1", "", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login?next=/index.html?x=1" {
		t.Fatalf("page without a session: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(s, "GET", "/auth/login", "", nil); rec.Code != http.StatusOK || rec.Body.String() != "login page" {
		t.Fatalf("login routes must be reachable without a session: %d", rec.Code)
	}
	if rec := do(s, "GET", "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/overview", "", map[string]string{"X-Forwarded-Email": "me@example.com"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an identity header must be ignored when login is on: %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/overview", "", withSession("other@example.com")); rec.Code != http.StatusForbidden {
		t.Fatalf("not on the allowlist: %d", rec.Code)
	}

	var cfg map[string]any
	json.Unmarshal(do(s, "GET", "/api/config", "", withSession("me@example.com")).Body.Bytes(), &cfg)
	if cfg["user"] != "me@example.com" || cfg["actionsEnabled"] != true || cfg["logout"] != "/auth/logout" {
		t.Fatalf("config = %v", cfg)
	}
	rec = do(s, "POST", "/api/cronjobs/shop/db-backup/run", "", withSession("me@example.com"))
	if rec.Code != http.StatusOK || fc.ranCronJob != "shop/db-backup by me@example.com" || !strings.Contains(logs.String(), `"user":"me@example.com"`) {
		t.Fatalf("action as a signed-in user: %d %q", rec.Code, fc.ranCronJob)
	}
}

func TestNoLogoutWithoutLogin(t *testing.T) {
	s, _, _, _ := newServer(Options{AuthHeader: "X-Forwarded-Email"})
	var cfg map[string]any
	json.Unmarshal(do(s, "GET", "/api/config", "", authed).Body.Bytes(), &cfg)
	if cfg["logout"] != "" {
		t.Fatalf("config = %v", cfg)
	}
	if rec := do(s, "GET", "/auth/login", "", authed); rec.Code == http.StatusOK && rec.Body.String() == "login page" {
		t.Fatal("login routes exist without login")
	}
}

type fakeInFlight struct{ calls int }

func (f *fakeInFlight) Report(context.Context) (*inflight.Report, error) {
	f.calls++
	return &inflight.Report{Rows: []inflight.Row{{App: "sure", Env: "production", Kind: inflight.KindPromotion, PR: 7, WaitingOn: inflight.WaitHealth}}, Notices: []string{inflight.MissingStatuses}}, nil
}

func TestInFlightEndpoint(t *testing.T) {
	fi := &fakeInFlight{}
	s, _, _, _ := newServer(Options{InFlight: fi})
	rec := do(s, "GET", "/api/inflight", "", nil)
	if rec.Code != http.StatusOK || fi.calls != 1 {
		t.Fatalf("inflight: %d %s", rec.Code, rec.Body)
	}
	var rep inflight.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil || len(rep.Rows) != 1 || rep.Rows[0].PR != 7 || rep.Notices[0] != inflight.MissingStatuses {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	if rec := do(s, "GET", "/api/config", "", nil); !strings.Contains(rec.Body.String(), `"inflight":true`) {
		t.Fatalf("config = %s", rec.Body)
	}
}

func TestInFlightNotConfigured(t *testing.T) {
	s, _, _, _ := newServer(Options{})
	if rec := do(s, "GET", "/api/inflight", "", nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestInFlightNeedsLogin(t *testing.T) {
	s, _, _, _ := newServer(Options{InFlight: &fakeInFlight{}, AuthHeader: "X-User"})
	if rec := do(s, "GET", "/api/inflight", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous got %d", rec.Code)
	}
}
