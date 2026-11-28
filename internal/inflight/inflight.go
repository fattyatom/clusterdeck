package inflight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fattyatom/clusterdeck/internal/gitops"
	"github.com/fattyatom/clusterdeck/internal/kube"
)

var ErrNotConfigured = errors.New("the In flight view needs INFLIGHT_REPO and GitHub credentials")

const (
	LabelPromotion       = "pin-promotion"
	LabelRollback        = "pin-rollback"
	LabelFolderPromotion = "folder-promotion"
	LabelNeedsHuman      = "needs-human"
	LabelWatchOK         = "watch-ok"
	LabelRolledBack      = "rolled-back"

	HealthContext    = "pin-promoter/testing-health"
	CICheck          = "ci-ok"
	CIFallbackCheck  = "ci-ok-fallback"
	WatchGrace       = 15 * time.Minute
	DefaultCacheTTL  = 60 * time.Second
	maxBumpCommits   = 10
	maxBumpCacheSize = 2000

	MissingStatuses = "missing permission: Commit statuses read"
	MissingActions  = "missing permission: Actions read"
)

const (
	KindPromotion = "promotion"
	KindRollback  = "rollback"
	KindDeploy    = "deploy"
	KindSync      = "sync"

	WaitSoak   = "soak"
	WaitHealth = "health"
	WaitCI     = "CI"
	WaitHuman  = "human"
	WaitMerge  = "merge"
	WaitWatch  = "watch"
	WaitSync   = "sync"
)

var metaRE = regexp.MustCompile(`(?s)<!--\s*pin-promoter:(\{.*?\})\s*-->`)

type Argo struct {
	Application string `json:"application"`
	Sync        string `json:"sync"`
	Health      string `json:"health"`
	Revision    string `json:"revision"`
}

type Row struct {
	App             string     `json:"app"`
	Env             string     `json:"env"`
	Kind            string     `json:"kind"`
	PR              int        `json:"pr,omitempty"`
	URL             string     `json:"url,omitempty"`
	Title           string     `json:"title,omitempty"`
	From            string     `json:"from"`
	To              string     `json:"to"`
	Health          string     `json:"health"`
	HealthDetail    string     `json:"health_detail,omitempty"`
	CI              string     `json:"ci"`
	CIDetail        string     `json:"ci_detail,omitempty"`
	SoakRemainingS  *int64     `json:"soak_remaining_s"`
	WatchRemainingS *int64     `json:"watch_remaining_s"`
	Argo            *Argo      `json:"argo"`
	WaitingOn       string     `json:"waiting_on"`
	Since           *time.Time `json:"since,omitempty"`
	Notes           []string   `json:"notes,omitempty"`
}

type Report struct {
	Rows         []Row     `json:"rows"`
	Notices      []string  `json:"notices"`
	GeneratedAt  time.Time `json:"generated_at"`
	SoakSeconds  int64     `json:"soak_seconds"`
	WatchSeconds int64     `json:"watch_seconds"`
}

type GitHub interface {
	Get(ctx context.Context, path string, out any) error
}

type ArgoLister interface {
	ArgoApplications(ctx context.Context) ([]kube.ArgoApplication, error)
}

type DeploySource interface {
	Deploys(ctx context.Context) ([]Row, error)
}

type Service struct {
	GitHub  GitHub
	Argo    ArgoLister
	Deploys DeploySource
	Owner   string
	Repo    string
	Branch  string
	Soak    time.Duration
	Watch   time.Duration
	TTL     time.Duration
	Now     func() time.Time

	mu       sync.Mutex
	cached   *Report
	cachedAt time.Time
	bumps    map[string]*time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) Report(ctx context.Context) (*Report, error) {
	if s == nil || s.GitHub == nil || s.Owner == "" || s.Repo == "" {
		return nil, ErrNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ttl := s.TTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	now := s.now()
	if s.cached != nil && now.Sub(s.cachedAt) < ttl {
		return s.cached, nil
	}
	r := s.build(ctx, now)
	s.cached, s.cachedAt = r, now
	return r, nil
}

type notices struct {
	seen map[string]bool
	list []string
}

func (n *notices) add(msg string) {
	if n.seen == nil {
		n.seen = map[string]bool{}
	}
	if !n.seen[msg] {
		n.seen[msg] = true
		n.list = append(n.list, msg)
	}
}

func (s *Service) build(ctx context.Context, now time.Time) *Report {
	rep := &Report{Rows: []Row{}, GeneratedAt: now, SoakSeconds: int64(s.Soak / time.Second), WatchSeconds: int64(s.Watch / time.Second)}
	var n notices

	argoByName := map[string]kube.ArgoApplication{}
	var apps []kube.ArgoApplication
	if s.Argo != nil {
		var err error
		apps, err = s.Argo.ArgoApplications(ctx)
		if err != nil {
			n.add("Argo CD Applications could not be read: " + err.Error())
		}
		for _, a := range apps {
			argoByName[a.Name] = a
		}
	}

	rep.Rows = append(rep.Rows, s.openRows(ctx, now, argoByName, &n)...)
	rep.Rows = append(rep.Rows, s.mergedRows(ctx, now, argoByName, &n)...)
	rep.Rows = append(rep.Rows, argoRows(apps)...)

	if s.Deploys == nil {
		n.add("Dokploy deploys are not shown: no deploy source is configured.")
	} else if rows, err := s.Deploys.Deploys(ctx); err != nil {
		n.add("Dokploy deploys could not be read: " + err.Error())
	} else {
		rep.Rows = append(rep.Rows, rows...)
	}

	sortRows(rep.Rows)
	rep.Notices = n.list
	if rep.Notices == nil {
		rep.Notices = []string{}
	}
	return rep
}

func sortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].Since, rows[j].Since
		switch {
		case a == nil && b == nil:
			return rows[i].PR > rows[j].PR
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return a.After(*b)
	})
}

type label struct {
	Name string `json:"name"`
}

type pull struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	HTMLURL        string     `json:"html_url"`
	Body           string     `json:"body"`
	CreatedAt      *time.Time `json:"created_at"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Labels         []label    `json:"labels"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p pull) labels() map[string]bool {
	out := map[string]bool{}
	for _, l := range p.Labels {
		out[l.Name] = true
	}
	return out
}

type Meta struct {
	Repo         string `json:"repo"`
	Slug         string `json:"slug"`
	NewTag       string `json:"new_tag"`
	OldTag       string `json:"old_tag"`
	Path         string `json:"path"`
	Source       string `json:"source"`
	Cls          string `json:"cls"`
	Rollback     int    `json:"rollback"`
	Mode         string `json:"mode"`
	App          string `json:"app"`
	SourceCommit string `json:"source_commit"`
}

func ParseMeta(body string) *Meta {
	m := metaRE.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	var meta Meta
	if err := json.Unmarshal([]byte(m[1]), &meta); err != nil {
		return nil
	}
	return &meta
}

func AppEnv(p string) (app, env string) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i := len(parts) - 1; i > 0; i-- {
		if parts[i] == "production" || parts[i] == "testing" {
			return parts[i-1], parts[i]
		}
	}
	return "", ""
}

func isKustomization(p string) bool {
	b := path.Base(p)
	return b == "kustomization.yaml" || b == "kustomization.yml" || b == "Kustomization"
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func secondsPtr(d time.Duration) *int64 {
	v := int64(d / time.Second)
	if v < 0 {
		v = 0
	}
	return &v
}

func (s *Service) repoPath() string { return "/repos/" + s.Owner + "/" + s.Repo }

func (s *Service) branch() string {
	if s.Branch == "" {
		return "main"
	}
	return s.Branch
}

func (s *Service) rowFor(p pull, meta *Meta, labels map[string]bool) Row {
	r := Row{PR: p.Number, URL: p.HTMLURL, Title: p.Title, Kind: KindPromotion}
	if labels[LabelRollback] || (meta != nil && meta.Rollback != 0) {
		r.Kind = KindRollback
	}
	if meta != nil {
		if meta.Mode == "folder" {
			r.App, r.Env = meta.App, "production"
			r.To = shortSHA(meta.SourceCommit)
		} else {
			r.App, r.Env = AppEnv(meta.Path)
			r.From, r.To = meta.OldTag, meta.NewTag
		}
		if r.App == "" {
			r.App = meta.Slug
		}
	}
	if r.App == "" {
		r.App = p.Title
	}
	if r.Env == "" {
		r.Env = "production"
	}
	return r
}

func attachArgo(r *Row, argoByName map[string]kube.ArgoApplication) *kube.ArgoApplication {
	name := r.App
	if r.Env != "production" {
		name = r.App + "-" + r.Env
	}
	a, ok := argoByName[name]
	if !ok {
		return nil
	}
	r.Argo = &Argo{Application: a.Name, Sync: a.Sync, Health: a.Health, Revision: a.Revision}
	return &a
}

func (s *Service) openRows(ctx context.Context, now time.Time, argoByName map[string]kube.ArgoApplication, n *notices) []Row {
	q := url.Values{"state": {"open"}, "base": {s.branch()}, "per_page": {"100"}}
	var pulls []pull
	if err := s.GitHub.Get(ctx, s.repoPath()+"/pulls?"+q.Encode(), &pulls); err != nil {
		n.add("open pull requests could not be read: " + err.Error())
		return nil
	}
	var rows []Row
	for _, p := range pulls {
		labels := p.labels()
		if !labels[LabelPromotion] && !labels[LabelRollback] && !labels[LabelFolderPromotion] {
			continue
		}
		meta := ParseMeta(p.Body)
		r := s.rowFor(p, meta, labels)
		r.Since = p.CreatedAt
		attachArgo(&r, argoByName)

		r.Health, r.HealthDetail, r.CI, r.CIDetail = "unknown", "", "unknown", ""
		if p.Head.SHA != "" {
			statuses, err := s.statuses(ctx, p.Head.SHA)
			if err != nil {
				r.Notes = append(r.Notes, s.permissionNote(err, MissingStatuses, "commit statuses", n))
			} else {
				r.Health, r.HealthDetail = healthOf(statuses)
			}
			jobs, jerr := s.jobs(ctx, p.Head.SHA)
			if jerr != nil {
				r.Notes = append(r.Notes, s.permissionNote(jerr, MissingActions, "Actions runs", n))
			}
			if err == nil && jerr == nil {
				r.CI, r.CIDetail = CIState(statuses, jobs)
			}
		}

		if r.Kind == KindPromotion && meta != nil {
			if at, err := s.bumpTime(ctx, meta); err != nil {
				r.Notes = append(r.Notes, "soak unknown: "+err.Error())
			} else if at != nil {
				r.SoakRemainingS = secondsPtr(s.Soak - now.Sub(*at))
			} else {
				r.Notes = append(r.Notes, "soak unknown: the bump was not found in the testing file's recent history")
			}
		}
		r.WaitingOn = waitingOn(r, labels)
		rows = append(rows, r)
	}
	return rows
}

func (s *Service) permissionNote(err error, missing, what string, n *notices) string {
	if gitops.IsForbidden(err) {
		n.add(missing + ": grant it to the GitHub App and accept the new permissions on the installation.")
		return missing
	}
	msg := what + " could not be read: " + err.Error()
	n.add(msg)
	return msg
}

func waitingOn(r Row, labels map[string]bool) string {
	switch {
	case r.SoakRemainingS != nil && *r.SoakRemainingS > 0:
		return WaitSoak
	case r.Health != "success":
		return WaitHealth
	case r.CI != "success":
		return WaitCI
	case labels[LabelNeedsHuman]:
		return WaitHuman
	}
	return WaitMerge
}

func (s *Service) mergedRows(ctx context.Context, now time.Time, argoByName map[string]kube.ArgoApplication, n *notices) []Row {
	q := url.Values{"state": {"closed"}, "base": {s.branch()}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {"50"}}
	var pulls []pull
	if err := s.GitHub.Get(ctx, s.repoPath()+"/pulls?"+q.Encode(), &pulls); err != nil {
		n.add("merged pull requests could not be read: " + err.Error())
		return nil
	}
	var rows []Row
	for _, p := range pulls {
		if p.MergedAt == nil {
			continue
		}
		age := now.Sub(*p.MergedAt)
		if age > s.Watch+WatchGrace {
			continue
		}
		labels := p.labels()
		meta := ParseMeta(p.Body)
		if !labels[LabelPromotion] && !labels[LabelRollback] && !labels[LabelFolderPromotion] {
			continue
		}
		r := s.rowFor(p, meta, labels)
		r.Since = p.MergedAt
		r.Health, r.CI = "merged", "merged"
		app := attachArgo(&r, argoByName)
		if r.Kind == KindRollback {
			if app == nil || (app.Sync == "Synced" && app.Health == "Healthy") {
				continue
			}
			r.WaitingOn = WaitSync
			rows = append(rows, r)
			continue
		}
		if labels[LabelWatchOK] || labels[LabelRolledBack] {
			continue
		}
		if meta == nil || meta.OldTag == "" {
			continue
		}
		r.WatchRemainingS = secondsPtr(s.Watch - age)
		r.WaitingOn = WaitWatch
		rows = append(rows, r)
	}
	return rows
}

func argoRows(apps []kube.ArgoApplication) []Row {
	var rows []Row
	for _, a := range apps {
		wait := ""
		switch {
		case a.OperationPhase == "Running" || (a.Sync != "" && a.Sync != "Synced"):
			wait = WaitSync
		case a.Health != "" && a.Health != "Healthy":
			wait = WaitHealth
		}
		if wait == "" {
			continue
		}
		env, app := "production", a.Name
		if base, ok := strings.CutSuffix(a.Name, "-testing"); ok {
			env, app = "testing", base
		}
		rows = append(rows, Row{
			App: app, Env: env, Kind: KindSync, To: shortSHA(a.Revision),
			Health: a.Health, CI: "",
			Argo:      &Argo{Application: a.Name, Sync: a.Sync, Health: a.Health, Revision: a.Revision},
			WaitingOn: wait, Since: a.Changed,
		})
	}
	return rows
}

type status struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
}

type Job struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func (s *Service) statuses(ctx context.Context, sha string) ([]status, error) {
	var combined struct {
		Statuses []status `json:"statuses"`
	}
	if err := s.GitHub.Get(ctx, s.repoPath()+"/commits/"+url.PathEscape(sha)+"/status?per_page=100", &combined); err != nil {
		return nil, err
	}
	return combined.Statuses, nil
}

func (s *Service) jobs(ctx context.Context, sha string) ([]Job, error) {
	var runs struct {
		WorkflowRuns []struct {
			ID         int64 `json:"id"`
			WorkflowID int64 `json:"workflow_id"`
		} `json:"workflow_runs"`
	}
	q := url.Values{"head_sha": {sha}, "per_page": {"100"}}
	if err := s.GitHub.Get(ctx, s.repoPath()+"/actions/runs?"+q.Encode(), &runs); err != nil {
		return nil, err
	}
	latest := map[int64]int64{}
	for _, run := range runs.WorkflowRuns {
		if run.ID > latest[run.WorkflowID] {
			latest[run.WorkflowID] = run.ID
		}
	}
	ids := make([]int64, 0, len(latest))
	for _, id := range latest {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []Job
	for _, id := range ids {
		var jobs struct {
			Jobs []Job `json:"jobs"`
		}
		if err := s.GitHub.Get(ctx, fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", s.repoPath(), id), &jobs); err != nil {
			return nil, err
		}
		out = append(out, jobs.Jobs...)
	}
	return out, nil
}

func healthOf(statuses []status) (string, string) {
	for _, st := range statuses {
		if st.Context == HealthContext {
			switch st.State {
			case "success":
				return "success", st.Description
			case "pending":
				return "pending", st.Description
			default:
				return "failure", st.Description
			}
		}
	}
	return "pending", "no " + HealthContext + " status yet"
}

func CIState(statuses []status, jobs []Job) (string, string) {
	rank := map[string]int{"ok": 0, "pending": 1, "fail": 2}
	results := map[string]string{}
	succeeded := map[string]bool{}
	record := func(name, res string, success bool) {
		if prev, ok := results[name]; !ok || rank[res] > rank[prev] {
			results[name] = res
		}
		if prev, ok := succeeded[name]; ok {
			succeeded[name] = prev && success
		} else {
			succeeded[name] = success
		}
	}
	for _, st := range statuses {
		if st.Context == HealthContext {
			continue
		}
		switch st.State {
		case "success":
			record(st.Context, "ok", true)
		case "pending":
			record(st.Context, "pending", false)
		default:
			record(st.Context, "fail", false)
		}
	}
	for _, j := range jobs {
		if j.Status != "completed" {
			record(j.Name, "pending", false)
			continue
		}
		switch j.Conclusion {
		case "success":
			record(j.Name, "ok", true)
		case "neutral", "skipped":
			record(j.Name, "ok", false)
		case "cancelled":
			continue
		default:
			record(j.Name, "fail", false)
		}
	}
	if results[CICheck] == "fail" && succeeded[CIFallbackCheck] {
		results[CICheck] = "ok"
	}
	var failed, waiting []string
	for name, res := range results {
		switch res {
		case "fail":
			failed = append(failed, name)
		case "pending":
			waiting = append(waiting, name)
		}
	}
	sort.Strings(failed)
	sort.Strings(waiting)
	if len(failed) > 0 {
		return "failure", "failed: " + strings.Join(failed, ", ")
	}
	if len(waiting) > 0 {
		return "pending", "waiting on: " + strings.Join(waiting, ", ")
	}
	if !succeeded[CICheck] && !succeeded[CIFallbackCheck] {
		return "pending", CICheck + " has not reported"
	}
	return "success", "all checks green"
}

type commitInfo struct {
	SHA    string `json:"sha"`
	Commit struct {
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
	Files []struct {
		Filename string `json:"filename"`
		Patch    string `json:"patch"`
	} `json:"files"`
}

func (s *Service) bumpTime(ctx context.Context, meta *Meta) (*time.Time, error) {
	if meta.Mode == "folder" {
		if meta.SourceCommit == "" {
			return nil, errors.New("the footer has no source_commit")
		}
		var c commitInfo
		if err := s.GitHub.Get(ctx, s.repoPath()+"/commits/"+url.PathEscape(meta.SourceCommit), &c); err != nil {
			return nil, err
		}
		t := c.Commit.Committer.Date
		return &t, nil
	}
	src := meta.Source
	if src == "" {
		src = meta.Path
	}
	if src == "" || meta.NewTag == "" {
		return nil, errors.New("the footer has no path or new_tag")
	}
	pattern := meta.NewTag
	if !isKustomization(src) {
		pattern = meta.Repo + ":" + meta.NewTag
	}
	q := url.Values{"sha": {s.branch()}, "path": {src}, "per_page": {fmt.Sprint(maxBumpCommits)}}
	var commits []commitInfo
	if err := s.GitHub.Get(ctx, s.repoPath()+"/commits?"+q.Encode(), &commits); err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		return nil, nil
	}
	key := commits[0].SHA + "|" + src + "|" + pattern
	if at, ok := s.bumps[key]; ok {
		return at, nil
	}
	var found *time.Time
	for _, c := range commits {
		var full commitInfo
		if err := s.GitHub.Get(ctx, s.repoPath()+"/commits/"+url.PathEscape(c.SHA), &full); err != nil {
			return nil, err
		}
		if touches(full, src, pattern) {
			t := full.Commit.Committer.Date
			found = &t
			break
		}
	}
	if s.bumps == nil || len(s.bumps) >= maxBumpCacheSize {
		s.bumps = map[string]*time.Time{}
	}
	s.bumps[key] = found
	return found, nil
}

func touches(c commitInfo, file, pattern string) bool {
	for _, f := range c.Files {
		if f.Filename != file {
			continue
		}
		for _, line := range strings.Split(f.Patch, "\n") {
			if (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) &&
				!strings.HasPrefix(line, "+++") && !strings.HasPrefix(line, "---") &&
				strings.Contains(line, pattern) {
				return true
			}
		}
	}
	return false
}
