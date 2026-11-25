package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fattyatom/clusterdeck/internal/gitops"
)

type fakeGit struct {
	opened  []gitops.Request
	landed  bool
	openErr error
	result  *gitops.PullRequest
}

func (f *fakeGit) OpenRollback(_ context.Context, req gitops.Request) (*gitops.PullRequest, error) {
	f.opened = append(f.opened, req)
	if f.openErr != nil {
		return nil, f.openErr
	}
	if f.result != nil {
		return f.result, nil
	}
	return &gitops.PullRequest{Number: 9, URL: "https://github.com/o/r/pull/9"}, nil
}

func (f *fakeGit) RolledBackSince(context.Context, gitops.Source, string, string) (bool, error) {
	return f.landed, nil
}

func gitApp(source map[string]any, sources []any) map[string]any {
	obj := argoAppObj("shop", map[string]any{"selfHeal": true})
	spec := obj["spec"].(map[string]any)
	if source != nil {
		spec["source"] = source
	}
	if sources != nil {
		spec["sources"] = sources
	}
	return obj
}

var homelabSource = map[string]any{"repoURL": "https://github.com/o/r.git", "path": "apps/shop", "targetRevision": "main"}

func TestRollbackModes(t *testing.T) {
	cases := []struct {
		name    string
		app     map[string]any
		git     bool
		mode    string
		allowed bool
		reason  string
	}{
		{"self-heal without git", gitApp(homelabSource, nil), false, ModeNone, false, "Configure a GitHub token"},
		{"self-heal with git", gitApp(homelabSource, nil), true, ModePullRequest, true, "opens a pull request"},
		{"helm chart", gitApp(map[string]any{"repoURL": "https://charts.example.com", "chart": "x", "targetRevision": "1.0"}, nil), true, ModeNone, false, "Helm repository"},
		{"multi-source", gitApp(nil, []any{homelabSource, homelabSource}), true, ModeNone, false, "several sources"},
		{"gitlab", gitApp(map[string]any{"repoURL": "https://gitlab.com/o/r.git", "path": "apps/shop"}, nil), true, ModeNone, false, "not a github.com repository"},
		{"no self-heal stays in the cluster", argoAppObj("shop", map[string]any{"prune": true}), true, ModeCluster, true, "OutOfSync"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newClient(t, argoDyn(t, tc.app), rolloutDeployment(2, nil), rs(1, "web:1", time.Hour), rs(2, "web:2", time.Minute))
			if tc.git {
				c.Git = &fakeGit{}
			}
			r, err := c.Rollouts(context.Background(), "shop", "shop")
			if err != nil {
				t.Fatal(err)
			}
			p := r[0].Rollback
			if p.Mode != tc.mode || p.Allowed != tc.allowed || !strings.Contains(p.Reason, tc.reason) {
				t.Fatalf("policy = %+v", p)
			}
			if tc.mode == ModePullRequest && r[0].Manager.PreviousRevision != "1111111" {
				t.Fatalf("previous revision = %q", r[0].Manager.PreviousRevision)
			}
		})
	}
}

func TestRollbackPR(t *testing.T) {
	ctx := context.Background()
	git := &fakeGit{}
	c, _ := newClient(t, argoDyn(t, gitApp(homelabSource, nil)), rolloutDeployment(2, nil), rs(1, "web:1", time.Hour), rs(2, "web:2", time.Minute))

	if _, err := c.RollbackPR(ctx, "argocd", "shop", "", "me"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("without git: %v", err)
	}
	c.Git = git

	for _, rev := range []string{"abcdef1234", "deadbeef"} {
		if _, err := c.RollbackPR(ctx, "argocd", "shop", rev, "me"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("revision %s: %v", rev, err)
		}
	}
	msg, err := c.RollbackPR(ctx, "argocd", "shop", "", "me")
	if err != nil || !strings.Contains(msg, "#9") {
		t.Fatalf("msg = %q err = %v", msg, err)
	}
	req := git.opened[len(git.opened)-1]
	if req.FromRevision != "abcdef1234" || req.ToRevision != "1111111" || req.User != "me" || req.Auto || req.Source.Path != "apps/shop" {
		t.Fatalf("request = %+v", req)
	}

	if _, err := c.Rollback(ctx, "shop", "web", 0, "me"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a cluster rollback must be refused in pull-request mode: %v", err)
	}

	git.result = &gitops.PullRequest{Number: 3, URL: "u3", Existing: true}
	if msg, _ := c.RollbackPR(ctx, "argocd", "shop", "1111111", "me"); !strings.Contains(msg, "already open") {
		t.Fatalf("msg = %q", msg)
	}
	git.result, git.openErr = nil, gitops.ErrNoChange
	if _, err := c.RollbackPR(ctx, "argocd", "shop", "1111111", "me"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no change: %v", err)
	}
}

func TestAutoRollbackByPR(t *testing.T) {
	ctx := context.Background()
	opted := map[string]string{AnnAutoRollback: "true"}
	setup := func(git *fakeGit) *Client {
		c, _ := newClient(t, argoDyn(t, gitApp(homelabSource, nil)), rolloutDeployment(2, opted), rs(1, "web:1", time.Hour), rs(2, "web:2", 10*time.Minute), crashingPod(2))
		c.Git = git
		return c
	}

	git := &fakeGit{}
	res, err := setup(git).AutoRollback(ctx, nil)
	if err != nil || len(res) != 1 || res[0].PullRequest != "https://github.com/o/r/pull/9" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	req := git.opened[0]
	if !req.Auto || req.ToRevision != "1111111" || !strings.Contains(req.Reason, "CrashLoopBackOff") {
		t.Fatalf("request = %+v", req)
	}

	for name, g := range map[string]*fakeGit{
		"a rollback already landed": {landed: true},
		"pull request already open": {result: &gitops.PullRequest{Existing: true}},
		"branch already exists":     {openErr: gitops.ErrExists},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := setup(g).AutoRollback(ctx, nil)
			if err != nil || len(res) != 0 {
				t.Fatalf("res = %+v err = %v", res, err)
			}
		})
	}
	if g := (&fakeGit{landed: true}); len(func() []gitops.Request { setup(g).AutoRollback(ctx, nil); return g.opened }()) != 0 {
		t.Fatal("no pull request may be opened after a rollback landed")
	}
}
