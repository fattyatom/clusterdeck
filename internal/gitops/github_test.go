package gitops

import (
	"context"
	"encoding/json"
	"errors"
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
			"commits": []any{
				map[string]any{"sha": "head2222bbbb", "commit": map[string]any{"message": "Bump shop to 2.1.0\n\nbody"}},
			},
			"files": []any{map[string]any{"filename": "apps/shop/kustomization.yaml"}, map[string]any{"filename": "base/deploy.yaml"}},
		},
		posted: map[string][]map[string]any{},
	}
	return f
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
	if got := BranchFor("Sure Testing", "abcdef123456"); got != "clusterdeck/rollback-sure-testing-abcdef1" {
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
	if !strings.HasPrefix(msg, "Roll back shop to old1111\n") || !strings.Contains(msg, CommitTrailer) || !strings.Contains(msg, "me@example.com") {
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
	f.open = []map[string]any{{"number": 3, "html_url": "https://github.com/o/r/pull/3", "head": map[string]any{"ref": "clusterdeck/rollback-shop-head222-to-zzz"}}}
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
	for _, msg := range []string{"Roll back shop to old1111 (#7)", "Merge pull request #7\n\n" + CommitTrailer} {
		f.compare = map[string]any{"commits": []any{map[string]any{"sha": "x", "commit": map[string]any{"message": msg}}}}
		if landed, _ := g.RolledBackSince(ctx, src, "shop", "old1111aaaa"); !landed {
			t.Fatalf("%q should count as a landed rollback", msg)
		}
	}
	f.compare = map[string]any{"commits": []any{map[string]any{"sha": "x", "commit": map[string]any{"message": "Roll back shopfront to abc"}}}}
	if landed, _ := g.RolledBackSince(ctx, src, "shop", "old1111aaaa"); landed {
		t.Fatal("another app's rollback must not count")
	}
}
