package inflight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fattyatom/clusterdeck/internal/gitops"
	"github.com/fattyatom/clusterdeck/internal/kube"
)

var now = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

type fakeAPI struct {
	mu        sync.Mutex
	responses map[string]any
	forbidden []string
	calls     []string
}

func (f *fakeAPI) Get(_ context.Context, p string, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)
	for _, prefix := range f.forbidden {
		if strings.HasPrefix(p, prefix) {
			return forbidden()
		}
	}
	for key, v := range f.responses {
		if p == key || strings.HasPrefix(p, key+"?") {
			raw, _ := json.Marshal(v)
			return json.Unmarshal(raw, out)
		}
	}
	return fmt.Errorf("github: HTTP 404: %s", p)
}

func forbidden() error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	g := &gitops.GitHub{API: srv.URL, Token: "t"}
	return g.Get(context.Background(), "/repos/o/r/commits/x/status", nil)
}

type fakeArgo struct {
	apps []kube.ArgoApplication
	err  error
}

func (f fakeArgo) ArgoApplications(context.Context) ([]kube.ArgoApplication, error) {
	return f.apps, f.err
}

const repo = "/repos/fattyatom/homelab"

func footer(meta map[string]any) string {
	raw, _ := json.Marshal(meta)
	return "Promote.\n\n<!-- pin-promoter:" + string(raw) + " -->"
}

func ts(t time.Time) string { return t.Format(time.RFC3339) }

func labels(names ...string) []map[string]string {
	out := []map[string]string{}
	for _, n := range names {
		out = append(out, map[string]string{"name": n})
	}
	return out
}

const kustPath = "infrastructure/argocd/manifests/webfinger/production/kustomization.yaml"
const kustSource = "infrastructure/argocd/manifests/webfinger/testing/kustomization.yaml"

func baseAPI() *fakeAPI {
	open := []map[string]any{
		{
			"number": 101, "title": "promote(pin): webfinger 1.2.3 → 1.2.4", "html_url": "https://github.com/fattyatom/homelab/pull/101",
			"created_at": ts(now.Add(-30 * time.Minute)), "head": map[string]any{"sha": "head101"},
			"labels": labels(LabelPromotion),
			"body": footer(map[string]any{"repo": "ghcr.io/fattyatom/webfinger", "slug": "webfinger", "new_tag": "1.2.4", "old_tag": "1.2.3",
				"path": kustPath, "source": kustSource, "cls": "patch"}),
		},
		{
			"number": 102, "title": "unrelated", "created_at": ts(now), "labels": labels("dependencies"), "head": map[string]any{"sha": "x"},
		},
	}
	closed := []map[string]any{
		{
			"number": 90, "title": "promote(pin): sure", "merged_at": ts(now.Add(-20 * time.Minute)),
			"labels": labels(LabelPromotion),
			"body": footer(map[string]any{"repo": "ghcr.io/we-promise/sure", "new_tag": "0.6.1", "old_tag": "0.6.0",
				"path": "infrastructure/argocd/manifests/sure/production/kustomization.yaml"}),
		},
		{
			"number": 89, "title": "settled", "merged_at": ts(now.Add(-10 * time.Minute)),
			"labels": labels(LabelPromotion, LabelWatchOK),
			"body":   footer(map[string]any{"repo": "r", "new_tag": "2", "old_tag": "1", "path": "a/x/production/k.yaml"}),
		},
		{
			"number": 80, "title": "too old", "merged_at": ts(now.Add(-3 * time.Hour)),
			"labels": labels(LabelPromotion),
			"body":   footer(map[string]any{"repo": "r", "new_tag": "2", "old_tag": "1", "path": "a/y/production/k.yaml"}),
		},
		{"number": 79, "title": "closed unmerged", "labels": labels(LabelPromotion)},
	}
	return &fakeAPI{responses: map[string]any{
		repo + "/commits/head101/status": map[string]any{"statuses": []map[string]any{
			{"context": HealthContext, "state": "success", "description": "validated green"},
		}},
		repo + "/actions/runs": map[string]any{"workflow_runs": []map[string]any{
			{"id": 1, "workflow_id": 7}, {"id": 2, "workflow_id": 7},
		}},
		repo + "/actions/runs/2/jobs": map[string]any{"jobs": []map[string]any{
			{"name": CICheck, "status": "completed", "conclusion": "success"},
		}},
		repo + "/commits": []map[string]any{{"sha": "c2"}, {"sha": "c1"}},
		repo + "/commits/c2": map[string]any{"sha": "c2", "commit": map[string]any{"committer": map[string]any{"date": ts(now.Add(-50 * time.Minute))}},
			"files": []map[string]any{{"filename": kustSource, "patch": "@@\n-    newTag: 1.2.3\n+    newTag: 1.2.5"}}},
		repo + "/commits/c1": map[string]any{"sha": "c1", "commit": map[string]any{"committer": map[string]any{"date": ts(now.Add(-2 * time.Hour))}},
			"files": []map[string]any{{"filename": kustSource, "patch": "@@\n-    newTag: 1.2.4\n+    newTag: 1.2.4"}}},
		"open":   open,
		"closed": closed,
	}}
}

type routedAPI struct{ *fakeAPI }

func (r routedAPI) Get(ctx context.Context, p string, out any) error {
	if strings.HasPrefix(p, repo+"/pulls?") {
		key := "closed"
		if strings.Contains(p, "state=open") {
			key = "open"
		}
		r.mu.Lock()
		r.calls = append(r.calls, p)
		for _, prefix := range r.forbidden {
			if strings.HasPrefix(p, prefix) {
				r.mu.Unlock()
				return forbidden()
			}
		}
		raw, _ := json.Marshal(r.responses[key])
		r.mu.Unlock()
		return json.Unmarshal(raw, out)
	}
	return r.fakeAPI.Get(ctx, p, out)
}

func service(api *fakeAPI, argo ArgoLister) *Service {
	return &Service{
		GitHub: routedAPI{api}, Argo: argo, Owner: "fattyatom", Repo: "homelab",
		Soak: time.Hour, Watch: time.Hour, Now: func() time.Time { return now },
	}
}

func byPR(rows []Row) map[int]Row {
	out := map[int]Row{}
	for _, r := range rows {
		if r.PR != 0 {
			out[r.PR] = r
		}
	}
	return out
}

func TestReportPromotionRow(t *testing.T) {
	argo := fakeArgo{apps: []kube.ArgoApplication{
		{Name: "webfinger", Sync: "Synced", Health: "Healthy", Revision: "abc1234567"},
	}}
	rep, err := service(baseAPI(), argo).Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows := byPR(rep.Rows)
	r, ok := rows[101]
	if !ok {
		t.Fatalf("rows = %+v", rep.Rows)
	}
	if _, ok := rows[102]; ok {
		t.Fatal("an unlabelled pull request must not be listed")
	}
	if r.App != "webfinger" || r.Env != "production" || r.Kind != KindPromotion || r.From != "1.2.3" || r.To != "1.2.4" {
		t.Fatalf("row = %+v", r)
	}
	if r.Health != "success" || r.CI != "success" {
		t.Fatalf("health/ci = %s/%s (%s)", r.Health, r.CI, r.CIDetail)
	}
	if r.SoakRemainingS == nil || *r.SoakRemainingS != 0 {
		t.Fatalf("soak = %v: the bump 2h ago is past a 1h soak", r.SoakRemainingS)
	}
	if r.Argo == nil || r.Argo.Application != "webfinger" || r.Argo.Sync != "Synced" {
		t.Fatalf("argo = %+v", r.Argo)
	}
	if r.WaitingOn != WaitMerge {
		t.Fatalf("waiting on %q", r.WaitingOn)
	}
	if len(r.Notes) != 0 {
		t.Fatalf("notes = %v", r.Notes)
	}
}

func TestReportWatchRows(t *testing.T) {
	rep, _ := service(baseAPI(), nil).Report(context.Background())
	rows := byPR(rep.Rows)
	w, ok := rows[90]
	if !ok {
		t.Fatalf("rows = %+v", rep.Rows)
	}
	if w.App != "sure" || w.WaitingOn != WaitWatch || w.WatchRemainingS == nil || *w.WatchRemainingS != 40*60 {
		t.Fatalf("watch row = %+v", w)
	}
	for _, n := range []int{89, 80, 79} {
		if _, ok := rows[n]; ok {
			t.Fatalf("#%d must not be in flight", n)
		}
	}
}

func TestReportNewestFirst(t *testing.T) {
	changed := now.Add(-time.Minute)
	argo := fakeArgo{apps: []kube.ArgoApplication{
		{Name: "sure-testing", Sync: "OutOfSync", Health: "Healthy", Revision: "fedcba9876", Changed: &changed},
		{Name: "lan-routes", Sync: "Synced", Health: "Healthy"},
		{Name: "nextexplorer", Sync: "Synced", Health: "Degraded"},
	}}
	rep, _ := service(baseAPI(), argo).Report(context.Background())
	if len(rep.Rows) != 4 {
		t.Fatalf("rows = %+v", rep.Rows)
	}
	first := rep.Rows[0]
	if first.Kind != KindSync || first.App != "sure" || first.Env != "testing" || first.WaitingOn != WaitSync {
		t.Fatalf("first = %+v", first)
	}
	if rep.Rows[1].PR != 90 || rep.Rows[2].PR != 101 {
		t.Fatalf("order = %d, %d", rep.Rows[1].PR, rep.Rows[2].PR)
	}
	last := rep.Rows[3]
	if last.App != "nextexplorer" || last.Env != "production" || last.WaitingOn != WaitHealth || last.Since != nil {
		t.Fatalf("last = %+v", last)
	}
}

func TestMissingPermissionsKeepTheRow(t *testing.T) {
	api := baseAPI()
	api.forbidden = []string{repo + "/commits/head101/status", repo + "/actions/"}
	rep, err := service(api, nil).Report(context.Background())
	if err != nil {
		t.Fatalf("a missing permission must not fail the endpoint: %v", err)
	}
	r, ok := byPR(rep.Rows)[101]
	if !ok {
		t.Fatal("the row must stay when statuses cannot be read")
	}
	if r.Health != "unknown" || r.CI != "unknown" || r.WaitingOn != WaitHealth {
		t.Fatalf("row = %+v", r)
	}
	if strings.Join(r.Notes, "|") != MissingStatuses+"|"+MissingActions {
		t.Fatalf("notes = %v", r.Notes)
	}
	joined := strings.Join(rep.Notices, "\n")
	if !strings.Contains(joined, MissingStatuses) || !strings.Contains(joined, MissingActions) {
		t.Fatalf("notices = %v", rep.Notices)
	}
}

func TestOnlyActionsMissingStillShowsHealth(t *testing.T) {
	api := baseAPI()
	api.forbidden = []string{repo + "/actions/"}
	rep, _ := service(api, nil).Report(context.Background())
	r := byPR(rep.Rows)[101]
	if r.Health != "success" || r.CI != "unknown" || r.WaitingOn != WaitCI {
		t.Fatalf("row = %+v", r)
	}
	if len(r.Notes) != 1 || r.Notes[0] != MissingActions {
		t.Fatalf("notes = %v", r.Notes)
	}
}

func TestPullsForbiddenStillShowsArgo(t *testing.T) {
	api := baseAPI()
	api.forbidden = []string{repo + "/pulls"}
	argo := fakeArgo{apps: []kube.ArgoApplication{{Name: "webfinger-testing", Sync: "OutOfSync"}}}
	rep, err := service(api, argo).Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 1 || rep.Rows[0].Kind != KindSync {
		t.Fatalf("rows = %+v", rep.Rows)
	}
	if !strings.Contains(strings.Join(rep.Notices, "\n"), "open pull requests could not be read") {
		t.Fatalf("notices = %v", rep.Notices)
	}
}

func TestDokployNotice(t *testing.T) {
	rep, _ := service(baseAPI(), nil).Report(context.Background())
	if !strings.Contains(strings.Join(rep.Notices, "\n"), "Dokploy deploys are not shown") {
		t.Fatalf("notices = %v", rep.Notices)
	}
}

type fakeDeploys struct{ err error }

func (f fakeDeploys) Deploys(context.Context) ([]Row, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []Row{{App: "pocket-id", Env: "testing", Kind: KindDeploy, WaitingOn: WaitHealth}}, nil
}

func TestDeploySource(t *testing.T) {
	s := service(baseAPI(), nil)
	s.Deploys = fakeDeploys{}
	rep, _ := s.Report(context.Background())
	found := false
	for _, r := range rep.Rows {
		found = found || r.Kind == KindDeploy
	}
	if !found || strings.Contains(strings.Join(rep.Notices, "\n"), "Dokploy") {
		t.Fatalf("rows = %+v notices = %v", rep.Rows, rep.Notices)
	}
	s = service(baseAPI(), nil)
	s.Deploys = fakeDeploys{err: errors.New("boom")}
	rep, _ = s.Report(context.Background())
	if !strings.Contains(strings.Join(rep.Notices, "\n"), "Dokploy deploys could not be read: boom") {
		t.Fatalf("notices = %v", rep.Notices)
	}
}

func TestReportIsCached(t *testing.T) {
	api := baseAPI()
	clock := now
	s := service(api, nil)
	s.Now = func() time.Time { return clock }
	s.Report(context.Background())
	n := len(api.calls)
	clock = clock.Add(30 * time.Second)
	s.Report(context.Background())
	if len(api.calls) != n {
		t.Fatalf("a second call within the TTL hit GitHub: %d → %d", n, len(api.calls))
	}
	clock = clock.Add(31 * time.Second)
	s.Report(context.Background())
	if len(api.calls) == n {
		t.Fatal("the cache never expires")
	}
}

func TestBumpLookupIsMemoised(t *testing.T) {
	api := baseAPI()
	clock := now
	s := service(api, nil)
	s.Now = func() time.Time { return clock }
	s.Report(context.Background())
	clock = clock.Add(2 * time.Minute)
	before := len(api.calls)
	s.Report(context.Background())
	for _, c := range api.calls[before:] {
		if c == repo+"/commits/c1" || c == repo+"/commits/c2" {
			t.Fatalf("commit %s re-read although the path's history did not change", c)
		}
	}
}

func TestSoakRemaining(t *testing.T) {
	api := baseAPI()
	api.responses[repo+"/commits/c2"] = map[string]any{"sha": "c2",
		"commit": map[string]any{"committer": map[string]any{"date": ts(now.Add(-20 * time.Minute))}},
		"files":  []map[string]any{{"filename": kustSource, "patch": "-    newTag: 1.2.3\n+    newTag: 1.2.4"}}}
	rep, _ := service(api, nil).Report(context.Background())
	r := byPR(rep.Rows)[101]
	if r.SoakRemainingS == nil || *r.SoakRemainingS != 40*60 || r.WaitingOn != WaitSoak {
		t.Fatalf("row = %+v", r)
	}
}

func TestNotConfigured(t *testing.T) {
	var s *Service
	if _, err := s.Report(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
	if _, err := (&Service{Owner: "o", Repo: "r"}).Report(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseMeta(t *testing.T) {
	m := ParseMeta("text\n<!-- pin-promoter:{\"new_tag\": \"1.2\", \"path\": \"a/b/production/c.yaml\", \"repo\": \"r\", \"rollback\": 12} -->")
	if m == nil || m.NewTag != "1.2" || m.Rollback != 12 {
		t.Fatalf("meta = %+v", m)
	}
	if ParseMeta("no footer") != nil || ParseMeta("<!-- pin-promoter:{bad} -->") != nil {
		t.Fatal("a missing or broken footer must parse to nil")
	}
}

func TestAppEnv(t *testing.T) {
	for p, want := range map[string][2]string{
		"infrastructure/argocd/manifests/sure/production/kustomization.yaml": {"sure", "production"},
		"dokploy/cloud/pocket-id/testing/docker-compose.yml":                 {"pocket-id", "testing"},
		"README.md": {"", ""},
	} {
		app, env := AppEnv(p)
		if app != want[0] || env != want[1] {
			t.Errorf("%s → %s/%s", p, app, env)
		}
	}
}

func TestRowForRollbackAndFolder(t *testing.T) {
	s := service(baseAPI(), nil)
	p := pull{Number: 5, Title: "rollback(pin): x"}
	r := s.rowFor(p, &Meta{Path: "a/x/production/k.yaml", NewTag: "1", Rollback: 4}, map[string]bool{LabelPromotion: true})
	if r.Kind != KindRollback || r.App != "x" || r.To != "1" {
		t.Fatalf("rollback row = %+v", r)
	}
	r = s.rowFor(p, &Meta{Mode: "folder", App: "sure", SourceCommit: "0123456789abcdef"}, map[string]bool{LabelFolderPromotion: true})
	if r.Kind != KindPromotion || r.App != "sure" || r.Env != "production" || r.To != "0123456" {
		t.Fatalf("folder row = %+v", r)
	}
}

func TestMergedRollbackListedUntilArgoSettles(t *testing.T) {
	api := baseAPI()
	api.responses["closed"] = []map[string]any{{
		"number": 95, "title": "rollback(pin)", "merged_at": ts(now.Add(-2 * time.Minute)),
		"labels": labels(LabelPromotion, LabelRollback),
		"body":   footer(map[string]any{"repo": "r", "new_tag": "1", "path": "a/webfinger/production/k.yaml", "rollback": 90}),
	}}
	syncing := fakeArgo{apps: []kube.ArgoApplication{{Name: "webfinger", Sync: "OutOfSync", Health: "Healthy"}}}
	rep, _ := service(api, syncing).Report(context.Background())
	if r, ok := byPR(rep.Rows)[95]; !ok || r.Kind != KindRollback || r.WaitingOn != WaitSync {
		t.Fatalf("rows = %+v", rep.Rows)
	}
	settled := fakeArgo{apps: []kube.ArgoApplication{{Name: "webfinger", Sync: "Synced", Health: "Healthy"}}}
	rep, _ = service(api, settled).Report(context.Background())
	if _, ok := byPR(rep.Rows)[95]; ok {
		t.Fatal("a settled rollback is no longer in flight")
	}
}

func TestCIState(t *testing.T) {
	ok := Job{Name: CICheck, Status: "completed", Conclusion: "success"}
	cases := []struct {
		name     string
		statuses []status
		jobs     []Job
		want     string
	}{
		{"green", nil, []Job{ok}, "success"},
		{"nothing reported", nil, nil, "pending"},
		{"own health context ignored", []status{{Context: HealthContext, State: "failure"}}, []Job{ok}, "success"},
		{"other status failed", []status{{Context: "lint", State: "failure"}}, []Job{ok}, "failure"},
		{"running", nil, []Job{ok, {Name: "test", Status: "in_progress"}}, "pending"},
		{"fallback covers ci-ok", nil, []Job{{Name: CICheck, Status: "completed", Conclusion: "failure"}, {Name: CIFallbackCheck, Status: "completed", Conclusion: "success"}}, "success"},
		{"skipped is not reported", nil, []Job{{Name: CICheck, Status: "completed", Conclusion: "skipped"}}, "pending"},
	}
	for _, c := range cases {
		if got, detail := CIState(c.statuses, c.jobs); got != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got, detail, c.want)
		}
	}
}

func TestHealthOf(t *testing.T) {
	if h, _ := healthOf(nil); h != "pending" {
		t.Fatalf("no status = %s", h)
	}
	if h, d := healthOf([]status{{Context: HealthContext, State: "error", Description: "SigNoz down"}}); h != "failure" || d != "SigNoz down" {
		t.Fatalf("error = %s %s", h, d)
	}
}

func TestWaitingOn(t *testing.T) {
	soak := int64(10)
	cases := []struct {
		r      Row
		labels map[string]bool
		want   string
	}{
		{Row{SoakRemainingS: &soak, Health: "success", CI: "success"}, nil, WaitSoak},
		{Row{Health: "pending", CI: "success"}, nil, WaitHealth},
		{Row{Health: "success", CI: "failure"}, nil, WaitCI},
		{Row{Health: "success", CI: "success"}, map[string]bool{LabelNeedsHuman: true}, WaitHuman},
		{Row{Health: "success", CI: "success"}, nil, WaitMerge},
	}
	for i, c := range cases {
		if got := waitingOn(c.r, c.labels); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
}
