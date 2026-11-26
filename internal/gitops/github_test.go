package gitops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func sp(s string) *string { return &s }

type fakeGitHub struct {
	mu        sync.Mutex
	trees     map[string][]treeEntry
	recursive map[string][]treeEntry
	commits   map[string]string
	refs      map[string]string
	open      []map[string]any
	compare   map[string]any
	contents  map[string]any
	dates     map[string]string
	byPath    map[string][]any
	queried   []string
	posted    map[string][]map[string]any
	auth      string
}

func newFake() *fakeGitHub {
	f := &fakeGitHub{
		commits: map[string]string{"old1111aaaa": "rootOld", "head2222bbbb": "rootHead"},
		refs:    map[string]string{"main": "head2222bbbb"},
		trees: map[string][]treeEntry{
			"rootOld":  {{Path: "apps", Type: "tree", Mode: "040000", SHA: sp("appsOld")}, {Path: "base", Type: "tree", Mode: "040000", SHA: sp("base1")}},
			"rootHead": {{Path: "apps", Type: "tree", Mode: "040000", SHA: sp("appsHead")}, {Path: "base", Type: "tree", Mode: "040000", SHA: sp("base2")}},
			"appsOld":  {{Path: "shop", Type: "tree", Mode: "040000", SHA: sp("shopOld")}},
			"appsHead": {{Path: "shop", Type: "tree", Mode: "040000", SHA: sp("shopHead")}},
		},
		recursive: map[string][]treeEntry{
			"shopOld": {
				{Path: "kustomization.yaml", Type: "blob", Mode: "100644", SHA: sp("kust-v1")},
				{Path: "config.yaml", Type: "blob", Mode: "100644", SHA: sp("config-v1")},
				{Path: "same.yaml", Type: "blob", Mode: "100644", SHA: sp("same")},
				{Path: "sub", Type: "tree", Mode: "040000", SHA: sp("subtree")},
			},
			"shopHead": {
				{Path: "kustomization.yaml", Type: "blob", Mode: "100644", SHA: sp("kust-v2")},
				{Path: "new.yaml", Type: "blob", Mode: "100644", SHA: sp("new-v1")},
				{Path: "same.yaml", Type: "blob", Mode: "100644", SHA: sp("same")},
			},
		},
		compare: map[string]any{
			"total_commits": 1,
			"commits": []any{
				ghCommit("head2222bbbb", "Bump shop to 2.1.0\n\nbody", "2026-01-02T00:00:00Z"),
			},
			"files": []any{map[string]any{"filename": "apps/shop/kustomization.yaml"}, map[string]any{"filename": "base/deploy.yaml"}},
		},
		contents: map[string]any{
			"apps/shop":                    dirListing("kustomization.yaml", "config.yaml"),
			"apps/shop/kustomization.yaml": kfile("resources:\n- config.yaml\n- ../../base\n"),
			"base":                         dirListing("deploy.yaml"),
		},
		dates: map[string]string{"old1111aaaa": "2026-01-01T00:00:00Z"},
		byPath: map[string][]any{
			"apps/shop":        {ghCommit("head2222bbbb", "Bump shop to 2.1.0\n\nbody", "2026-01-02T00:00:00Z")},
			"base/deploy.yaml": {ghCommit("head2222bbbb", "Bump shop to 2.1.0\n\nbody", "2026-01-02T00:00:00Z")},
		},
		posted: map[string][]map[string]any{},
	}
	return f
}

func ghCommit(sha, message, date string) map[string]any {
	return map[string]any{"sha": sha, "commit": map[string]any{"message": message, "committer": map[string]any{"date": date}}}
}

func dirListing(names ...string) []any {
	var out []any
	for _, n := range names {
		typ := "file"
		if !strings.Contains(n, ".") && n != "Kustomization" {
			typ = "dir"
		}
		out = append(out, map[string]any{"name": n, "type": typ})
	}
	return out
}

func kfile(body string) map[string]any {
	return map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(body))}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	p := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
	write := func(v any) { json.NewEncoder(w).Encode(v) }
	if r.Method == http.MethodPost {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		f.posted[p] = append(f.posted[p], body)
		switch p {
		case "/git/trees":
			write(map[string]any{"sha": "newtree"})
		case "/git/commits":
			write(map[string]any{"sha": "newcommit"})
		case "/git/refs":
			ref := strings.TrimPrefix(body["ref"].(string), "refs/heads/")
			if _, ok := f.refs[ref]; ok {
				w.WriteHeader(http.StatusUnprocessableEntity)
				write(map[string]any{"message": "Reference already exists"})
				return
			}
			f.refs[ref] = body["sha"].(string)
			write(map[string]any{})
		case "/pulls":
			write(map[string]any{"number": 7, "html_url": "https://github.com/o/r/pull/7", "head": map[string]any{"ref": body["head"]}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	switch {
	case p == "":
		write(map[string]any{"default_branch": "main"})
	case strings.HasPrefix(p, "/git/ref/heads/"):
		sha, ok := f.refs[strings.TrimPrefix(p, "/git/ref/heads/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		write(map[string]any{"object": map[string]any{"sha": sha}})
	case strings.HasPrefix(p, "/git/commits/"):
		tree, ok := f.commits[strings.TrimPrefix(p, "/git/commits/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		write(map[string]any{"tree": map[string]any{"sha": tree}})
	case strings.HasPrefix(p, "/git/trees/"):
		sha := strings.TrimPrefix(p, "/git/trees/")
		if r.URL.Query().Get("recursive") == "1" {
			write(map[string]any{"sha": sha, "tree": f.recursive[sha]})
			return
		}
		write(map[string]any{"sha": sha, "tree": f.trees[sha]})
	case strings.HasPrefix(p, "/compare/"):
		write(f.compare)
	case strings.HasPrefix(p, "/contents/"):
		c, ok := f.contents[strings.TrimPrefix(p, "/contents/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		write(c)
	case strings.HasPrefix(p, "/commits/"):
		sha := strings.TrimPrefix(p, "/commits/")
		write(ghCommit(sha, "target", f.dates[sha]))
	case p == "/commits":
		q := r.URL.Query()
		f.queried = append(f.queried, q.Get("path"))
		if q.Get("sha") != "main" || q.Get("since") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		list := f.byPath[q.Get("path")]
		if list == nil {
			list = []any{}
		}
		write(list)
	case p == "/pulls":
		write(f.open)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func client(t *testing.T, f *fakeGitHub) *GitHub {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &GitHub{API: srv.URL, Token: "tok"}
}

func request() Request {
	return Request{
		App:          "shop",
		Source:       Source{RepoURL: "https://github.com/o/r.git", Path: "apps/shop/", TargetRevision: "HEAD"},
		FromRevision: "head2222bbbb",
		ToRevision:   "old1111aaaa",
		User:         "me@example.com",
	}
}

func TestParseRepo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/fattyatom/homelab.git": "fattyatom/homelab",
		"https://github.com/fattyatom/homelab":     "fattyatom/homelab",
		"git@github.com:fattyatom/homelab.git":     "fattyatom/homelab",
		"ssh://git@github.com/o/r.js.git":          "o/r.js",
	} {
		o, r, ok := ParseRepo(in)
		if !ok || o+"/"+r != want {
			t.Errorf("%s: got %s/%s %v", in, o, r, ok)
		}
	}
	for _, in := range []string{"https://gitlab.com/o/r.git", "https://github.com/o", "https://charts.example.com"} {
		if _, _, ok := ParseRepo(in); ok {
			t.Errorf("%s should not parse", in)
		}
	}
	if err := Supported(Source{RepoURL: "https://github.com/o/r", Path: "../x"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("path traversal: %v", err)
	}
	if got := BranchFor("Sure Testing", "abcdef123456"); got != "clusterdeck/rollback-sure-testing-abcdef1-to-" {
		t.Fatalf("branch = %s", got)
	}
}

func TestOpenRollback(t *testing.T) {
	f := newFake()
	g := client(t, f)
	pr, err := g.OpenRollback(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 || pr.Branch != "clusterdeck/rollback-shop-head222-to-old1111" || pr.Existing {
		t.Fatalf("pr = %+v", pr)
	}
	if f.auth != "Bearer tok" {
		t.Fatalf("auth = %q", f.auth)
	}

	tree := f.posted["/git/trees"][0]
	if tree["base_tree"] != "rootHead" {
		t.Fatalf("base tree = %v", tree["base_tree"])
	}
	got := map[string]any{}
	for _, e := range tree["tree"].([]any) {
		m := e.(map[string]any)
		got[m["path"].(string)] = m["sha"]
		if m["type"] != "blob" {
			t.Fatalf("only blob entries are written: %v", m)
		}
	}
	want := map[string]any{
		"apps/shop/config.yaml":        "config-v1",
		"apps/shop/kustomization.yaml": "kust-v1",
		"apps/shop/new.yaml":           nil,
	}
	if len(got) != len(want) {
		t.Fatalf("tree entries = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("entry %s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}

	commit := f.posted["/git/commits"][0]
	msg := commit["message"].(string)
	if !strings.HasPrefix(msg, "Roll back shop to old1111\n") || !strings.Contains(msg, "\nClusterdeck-Rollback: shop\n") || !strings.Contains(msg, "me@example.com") {
		t.Fatalf("message = %q", msg)
	}
	if parents := commit["parents"].([]any); len(parents) != 1 || parents[0] != "head2222bbbb" {
		t.Fatalf("parents = %v", parents)
	}
	if f.refs["clusterdeck/rollback-shop-head222-to-old1111"] != "newcommit" {
		t.Fatalf("refs = %v", f.refs)
	}

	pull := f.posted["/pulls"][0]
	body := pull["body"].(string)
	if pull["base"] != "main" || pull["title"] != "Roll back shop to old1111" {
		t.Fatalf("pull = %v", pull)
	}
	for _, s := range []string{"`apps/shop/new.yaml` deleted", "`apps/shop/kustomization.yaml` restored", "Bump shop to 2.1.0", "`base/deploy.yaml`", "Not reverted"} {
		if !strings.Contains(body, s) {
			t.Errorf("body lacks %q:\n%s", s, body)
		}
	}
	if strings.Contains(body, "Bump shop to 2.1.0\n\nbody") {
		t.Error("only commit titles belong in the body")
	}

	_, err = g.OpenRollback(context.Background(), request())
	if !errors.Is(err, ErrExists) {
		t.Fatalf("a second identical rollback must not open a second branch: %v", err)
	}
}

func TestOpenRollbackReturnsOpenPull(t *testing.T) {
	f := newFake()
	f.open = []map[string]any{
		{"number": 2, "html_url": "https://github.com/o/r/pull/2", "head": map[string]any{"ref": "clusterdeck/rollback-shop-head222x-to-zzz"}},
		{"number": 3, "html_url": "https://github.com/o/r/pull/3", "head": map[string]any{"ref": "clusterdeck/rollback-shop-head222-to-zzz"}},
	}
	pr, err := client(t, f).OpenRollback(context.Background(), request())
	if err != nil || !pr.Existing || pr.Number != 3 {
		t.Fatalf("pr = %+v err = %v", pr, err)
	}
	if len(f.posted) != 0 {
		t.Fatalf("nothing may be written when a pull request is already open: %v", f.posted)
	}
}

func TestOpenRollbackRefusals(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.recursive["shopOld"] = f.recursive["shopHead"]
	f.trees["appsOld"] = []treeEntry{{Path: "shop", Type: "tree", Mode: "040000", SHA: sp("shopHead")}}
	if _, err := client(t, f).OpenRollback(ctx, request()); !errors.Is(err, ErrNoChange) {
		t.Fatalf("identical directory: %v", err)
	}

	f = newFake()
	f.trees["appsOld"] = nil
	if _, err := client(t, f).OpenRollback(ctx, request()); !errors.Is(err, ErrNoChange) {
		t.Fatalf("directory missing at target: %v", err)
	}

	req := request()
	req.Source.TargetRevision = "v1.2.0"
	if _, err := client(t, newFake()).OpenRollback(ctx, req); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("tag target: %v", err)
	}

	req = request()
	req.ToRevision = req.FromRevision
	if _, err := client(t, newFake()).OpenRollback(ctx, req); !errors.Is(err, ErrNoChange) {
		t.Fatalf("same revision: %v", err)
	}
}

func TestRolledBackSince(t *testing.T) {
	ctx := context.Background()
	src := request().Source
	f := newFake()
	g := client(t, f)
	if landed, err := g.RolledBackSince(ctx, src, "shop", "old1111aaaa"); err != nil || landed {
		t.Fatalf("landed = %v err = %v", landed, err)
	}
	for _, msg := range []string{"Roll back shop to old1111 (#7)", "Merge pull request #7\n\n" + TrailerFor("shop")} {
		f.compare = map[string]any{"commits": []any{map[string]any{"sha": "x", "commit": map[string]any{"message": msg}}}}
		if landed, _ := g.RolledBackSince(ctx, src, "shop", "old1111aaaa"); !landed {
			t.Fatalf("%q should count as a landed rollback", msg)
		}
	}
	for _, msg := range []string{
		"Roll back shopfront to abc",
		"Roll back sure-testing to abc1234\n\nRestores apps/sure.\n\nClusterdeck-Rollback: sure-testing\n",
		"Roll back shop-testing to abc1234\n\nClusterdeck-Rollback: shop-testing\n",
	} {
		f.compare = map[string]any{"commits": []any{map[string]any{"sha": "x", "commit": map[string]any{"message": msg}}}}
		if landed, _ := g.RolledBackSince(ctx, src, "shop", "old1111aaaa"); landed {
			t.Fatalf("another app's rollback must not count: %q", msg)
		}
	}
}

func TestOpenRollbackDeletesBranchWhenPullFails(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
		switch {
		case r.Method == http.MethodPost && p == "/pulls":
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"Validation Failed"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/git/refs/heads/"):
			f.mu.Lock()
			delete(f.refs, strings.TrimPrefix(p, "/git/refs/heads/"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			f.ServeHTTP(w, r)
		}
	}))
	defer srv.Close()
	g := &GitHub{API: srv.URL, Token: "tok"}
	if _, err := g.OpenRollback(context.Background(), request()); err == nil {
		t.Fatal("want the pull request error")
	}
	if _, ok := f.refs["clusterdeck/rollback-shop-head222-to-old1111"]; ok {
		t.Fatal("the branch must be deleted when the pull request cannot be opened, or it blocks every retry")
	}
	if _, err := g.OpenRollback(context.Background(), request()); errors.Is(err, ErrExists) {
		t.Fatal("a retry must not be blocked by a leftover branch")
	}
}

func files(names ...string) []any {
	var out []any
	for _, n := range names {
		out = append(out, map[string]any{"filename": n})
	}
	return out
}

func rollbackBody(t *testing.T, f *fakeGitHub) string {
	t.Helper()
	if _, err := client(t, f).OpenRollback(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	return f.posted["/pulls"][0]["body"].(string)
}

func section(body, heading string) string {
	_, rest, ok := strings.Cut(body, heading)
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "\n### "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func TestRollbackBodyFollowsKustomization(t *testing.T) {
	f := newFake()
	f.contents = map[string]any{
		"apps/shop": dirListing("kustomization.yaml", "config.yaml"),
		"apps/shop/kustomization.yaml": kfile(`resources:
- config.yaml
- ../../base
- ../../lib/ingress
- https://github.com/x/y//z?ref=v1
components:
- ../../components/tls
patches:
- path: ../../patches/limits.yaml
  target: {kind: Deployment}
- patch: |-
    - op: replace
configMapGenerator:
- name: cfg
  files:
  - app.conf=../../conf/app.conf
`),
		"base":                              dirListing("deploy.yaml", "service.yaml"),
		"lib/ingress":                       dirListing("Kustomization", "ingress.yaml", "README.md"),
		"lib/ingress/Kustomization":         kfile("resources:\n- ingress.yaml\n- ../common.yaml\n"),
		"components/tls":                    dirListing("kustomization.yaml", "cert.yaml"),
		"components/tls/kustomization.yaml": kfile("apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nresources:\n- cert.yaml\n"),
	}
	f.compare = map[string]any{
		"total_commits": 5,
		"commits": []any{
			ghCommit("c1docs", "Docs: rewrite README", "2026-01-02T00:00:00Z"),
			ghCommit("c2shop", "Bump shop to 2.1.0", "2026-01-03T00:00:00Z"),
			ghCommit("c3base", "Base: raise replicas", "2026-01-04T00:00:00Z"),
			ghCommit("c4other", "Bump other", "2026-01-05T00:00:00Z"),
			ghCommit("c5tls", "TLS: rotate issuer", "2026-01-06T00:00:00Z"),
		},
		"files": files(
			"apps/shop/kustomization.yaml", "base/deploy.yaml", "lib/common.yaml", "lib/ingress/ingress.yaml",
			"lib/ingress/README.md", "components/tls/cert.yaml", "patches/limits.yaml", "conf/app.conf",
			"docs/readme.md", "apps/other/x.yaml", ".github/workflows/ci.yaml", "lib/other.yaml",
		),
	}
	f.byPath = map[string][]any{
		"apps/shop":                {ghCommit("c2shop", "Bump shop to 2.1.0", "2026-01-03T00:00:00Z"), ghCommit("old1111aaaa", "target", "2026-01-01T00:00:00Z")},
		"base/deploy.yaml":         {ghCommit("c3base", "Base: raise replicas", "2026-01-04T00:00:00Z"), ghCommit("elsewhere", "On another branch", "2026-01-04T00:00:00Z")},
		"components/tls/cert.yaml": {ghCommit("c5tls", "TLS: rotate issuer", "2026-01-06T00:00:00Z")},
	}
	body := rollbackBody(t, f)

	notReverted := section(body, "### Not reverted: changed since, outside the application's path, used by its kustomization")
	for _, want := range []string{"base/deploy.yaml", "lib/common.yaml", "lib/ingress/ingress.yaml", "components/tls/cert.yaml", "patches/limits.yaml", "conf/app.conf"} {
		if !strings.Contains(notReverted, "`"+want+"`") {
			t.Errorf("not-reverted lacks %s:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"lib/ingress/README.md", "docs/readme.md", "apps/other/x.yaml", ".github/workflows/ci.yaml", "lib/other.yaml", "apps/shop/kustomization.yaml"} {
		if strings.Contains(notReverted, "`"+unwanted+"`") {
			t.Errorf("not-reverted lists %s, which the application cannot render from:\n%s", unwanted, body)
		}
	}

	commits := section(body, "### Commits on main since then touching `apps/shop` or a file listed under *Not reverted*")
	for _, want := range []string{"`c2shop` Bump shop to 2.1.0", "`c3base` Base: raise replicas", "`c5tls` TLS: rotate issuer"} {
		if !strings.Contains(commits, want) {
			t.Errorf("commits lack %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"Docs: rewrite README", "Bump other", "On another branch", "`old1111`"} {
		if strings.Contains(commits, unwanted) {
			t.Errorf("commits list %q:\n%s", unwanted, body)
		}
	}
	if strings.Index(commits, "c2shop") > strings.Index(commits, "c3base") {
		t.Errorf("commits are oldest first:\n%s", commits)
	}
	if f.queried[0] != "apps/shop" || len(f.queried) != 7 {
		t.Errorf("commit queries = %v, want the path then each changed file it renders from", f.queried)
	}
}

func TestRollbackBodyFallsBackToParentFolder(t *testing.T) {
	f := newFake()
	f.contents = map[string]any{"apps/shop": dirListing("deploy.yaml")}
	changed := []string{"apps/shop/deploy.yaml", "docs/readme.md", "base/deploy.yaml"}
	for i := 0; i < maxListed+5; i++ {
		changed = append(changed, fmt.Sprintf("apps/app%02d/values.yaml", i))
	}
	f.compare = map[string]any{
		"total_commits": 400,
		"commits":       []any{ghCommit("c1", "x", "2026-01-02T00:00:00Z")},
		"files":         files(changed...),
	}
	f.byPath = map[string][]any{
		"apps/shop": {ghCommit("same-second", "Committed with the target", "2026-01-01T00:00:00Z")},
		"apps":      {ghCommit("c9", "Bump app03", "2026-01-09T00:00:00Z")},
	}
	body := rollbackBody(t, f)

	notReverted := section(body, "### Not reverted: changed since, in `apps/` outside the application's path")
	if notReverted == "" || !strings.Contains(notReverted, "could not resolve the application's kustomization") {
		t.Fatalf("want the parent-folder fallback heading and reason:\n%s", body)
	}
	if !strings.Contains(notReverted, "`apps/app00/values.yaml`") || !strings.Contains(notReverted, "- … and 5 more\n") {
		t.Errorf("want the first %d sibling files and a truncation line:\n%s", maxListed, body)
	}
	for _, unwanted := range []string{"docs/readme.md", "base/deploy.yaml", "apps/shop/deploy.yaml"} {
		if strings.Contains(notReverted, unwanted) {
			t.Errorf("not-reverted lists %s:\n%s", unwanted, body)
		}
	}
	commits := section(body, "### Commits on main since then touching `apps/shop` or `apps/`")
	if !strings.Contains(commits, "`c9` Bump app03") || strings.Contains(commits, "Committed with the target") {
		t.Errorf("with the comparison truncated, commits are bounded by the target's date:\n%s", body)
	}
	if len(f.queried) != 2 || f.queried[1] != "apps" {
		t.Errorf("commit queries = %v", f.queried)
	}
}
