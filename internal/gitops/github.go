package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	ErrUnsupported = errors.New("unsupported source")
	ErrNoChange    = errors.New("nothing to roll back")
	ErrExists      = errors.New("rollback pull request already exists")
)

const (
	BranchPrefix = "clusterdeck/rollback-"
	TrailerKey   = "Clusterdeck-Rollback: "
	maxListed    = 30

	maxPathQueries  = 10
	maxCompareFiles = 300
)

var githubRepo = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

type Source struct {
	RepoURL        string `json:"repoURL"`
	Path           string `json:"path"`
	TargetRevision string `json:"targetRevision"`
}

type Request struct {
	App          string
	Source       Source
	FromRevision string
	ToRevision   string
	User         string
	Reason       string
	Auto         bool
}

type PullRequest struct {
	Number   int    `json:"number"`
	URL      string `json:"url"`
	Branch   string `json:"branch"`
	Existing bool   `json:"existing"`
}

type GitHub struct {
	API    string
	Token  string
	Auth   TokenSource
	Client *http.Client
}

var repoPath = regexp.MustCompile(`^/repos/([^/]+)/([^/?]+)`)

func (g *GitHub) token(ctx context.Context, p string) (string, error) {
	if g.Auth == nil {
		return g.Token, nil
	}
	m := repoPath.FindStringSubmatch(p)
	if m == nil {
		return "", fmt.Errorf("github: %q is not a repository path", p)
	}
	return g.Auth.Token(ctx, m[1], m[2])
}

func ParseRepo(repoURL string) (owner, repo string, ok bool) {
	m := githubRepo.FindStringSubmatch(strings.TrimSpace(repoURL))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func Supported(src Source) error {
	if _, _, ok := ParseRepo(src.RepoURL); !ok {
		return fmt.Errorf("%w: %s is not a github.com repository", ErrUnsupported, src.RepoURL)
	}
	if strings.Trim(src.Path, "/") == "" || strings.Contains(src.Path, "..") {
		return fmt.Errorf("%w: the application has no path inside the repository", ErrUnsupported)
	}
	return nil
}

func short(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

func slug(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(s, "-")
	return strings.Trim(s, "-.")
}

func BranchFor(app, from string) string {
	return BranchPrefix + slug(app) + "-" + short(from) + "-to-"
}

func TrailerFor(app string) string {
	return TrailerKey + app
}

func hasTrailer(message, app string) bool {
	want := TrailerFor(app)
	for _, line := range strings.Split(message, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body) }

func (g *GitHub) do(ctx context.Context, method, p string, body, out any) error {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(raw)
	}
	api := strings.TrimRight(g.API, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	token, err := g.token(ctx, p)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, api+p, r)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &apiError{Status: resp.StatusCode, Body: msg}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func isStatus(err error, status int) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == status
}

type treeEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"`
}

type tree struct {
	SHA       string      `json:"sha"`
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

func (g *GitHub) commitTree(ctx context.Context, repo, commit string) (string, error) {
	var c struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.do(ctx, http.MethodGet, repo+"/git/commits/"+url.PathEscape(commit), nil, &c); err != nil {
		return "", err
	}
	return c.Tree.SHA, nil
}

func (g *GitHub) subtree(ctx context.Context, repo, rootTree, dir string) (string, error) {
	sha := rootTree
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		var t tree
		if err := g.do(ctx, http.MethodGet, repo+"/git/trees/"+sha, nil, &t); err != nil {
			return "", err
		}
		next := ""
		for _, e := range t.Tree {
			if e.Path == part && e.Type == "tree" && e.SHA != nil {
				next = *e.SHA
			}
		}
		if next == "" {
			return "", nil
		}
		sha = next
	}
	return sha, nil
}

func (g *GitHub) blobs(ctx context.Context, repo, treeSHA string) (map[string]treeEntry, error) {
	out := map[string]treeEntry{}
	if treeSHA == "" {
		return out, nil
	}
	var t tree
	if err := g.do(ctx, http.MethodGet, repo+"/git/trees/"+treeSHA+"?recursive=1", nil, &t); err != nil {
		return nil, err
	}
	if t.Truncated {
		return nil, fmt.Errorf("%w: the application's directory is too large for one tree listing", ErrUnsupported)
	}
	for _, e := range t.Tree {
		if e.Type == "blob" {
			out[e.Path] = e
		}
	}
	return out, nil
}

func diffEntries(dir string, old, cur map[string]treeEntry) []treeEntry {
	var out []treeEntry
	for p, o := range old {
		c, ok := cur[p]
		if ok && c.SHA != nil && o.SHA != nil && *c.SHA == *o.SHA && c.Mode == o.Mode {
			continue
		}
		sha := *o.SHA
		out = append(out, treeEntry{Path: path.Join(dir, p), Mode: o.Mode, Type: "blob", SHA: &sha})
	}
	for p, c := range cur {
		if _, ok := old[p]; !ok {
			out = append(out, treeEntry{Path: path.Join(dir, p), Mode: c.Mode, Type: "blob", SHA: nil})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

type pull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

func (g *GitHub) findOpen(ctx context.Context, repo, prefix string) (*PullRequest, error) {
	var pulls []pull
	if err := g.do(ctx, http.MethodGet, repo+"/pulls?state=open&per_page=100", nil, &pulls); err != nil {
		return nil, err
	}
	for _, p := range pulls {
		if strings.HasPrefix(p.Head.Ref, prefix) {
			return &PullRequest{Number: p.Number, URL: p.HTMLURL, Branch: p.Head.Ref, Existing: true}, nil
		}
	}
	return nil, nil
}

func (g *GitHub) baseBranch(ctx context.Context, repo string, src Source) (string, error) {
	if src.TargetRevision != "" && src.TargetRevision != "HEAD" {
		return src.TargetRevision, nil
	}
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(ctx, http.MethodGet, repo, nil, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

func TitleFor(app, to string) string {
	return fmt.Sprintf("Roll back %s to %s", app, short(to))
}

func (g *GitHub) RolledBackSince(ctx context.Context, src Source, app, since string) (bool, error) {
	owner, name, ok := ParseRepo(src.RepoURL)
	if !ok {
		return false, ErrUnsupported
	}
	repo := "/repos/" + owner + "/" + name
	base, err := g.baseBranch(ctx, repo, src)
	if err != nil {
		return false, err
	}
	var cmp struct {
		Commits []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
	}
	if err := g.do(ctx, http.MethodGet, repo+"/compare/"+url.PathEscape(since)+"..."+url.PathEscape(base), nil, &cmp); err != nil {
		return false, err
	}
	prefix := fmt.Sprintf("Roll back %s to ", app)
	for _, c := range cmp.Commits {
		if strings.HasPrefix(c.Commit.Message, prefix) || hasTrailer(c.Commit.Message, app) {
			return true, nil
		}
	}
	return false, nil
}

func (g *GitHub) OpenRollback(ctx context.Context, req Request) (*PullRequest, error) {
	if err := Supported(req.Source); err != nil {
		return nil, err
	}
	if req.FromRevision == "" || req.ToRevision == "" || req.FromRevision == req.ToRevision {
		return nil, fmt.Errorf("%w: pick an earlier synced revision", ErrNoChange)
	}
	owner, name, _ := ParseRepo(req.Source.RepoURL)
	repo := "/repos/" + owner + "/" + name
	dir := strings.Trim(req.Source.Path, "/")
	branchPrefix := BranchFor(req.App, req.FromRevision)

	if open, err := g.findOpen(ctx, repo, branchPrefix); err != nil || open != nil {
		return open, err
	}

	base, err := g.baseBranch(ctx, repo, req.Source)
	if err != nil {
		return nil, err
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, repo+"/git/ref/heads/"+base, nil, &ref); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("%w: targetRevision %q is not a branch, so there is nothing to open a pull request against", ErrUnsupported, base)
		}
		return nil, err
	}
	head := ref.Object.SHA

	headTree, err := g.commitTree(ctx, repo, head)
	if err != nil {
		return nil, err
	}
	oldRoot, err := g.commitTree(ctx, repo, req.ToRevision)
	if err != nil {
		return nil, err
	}
	curDir, err := g.subtree(ctx, repo, headTree, dir)
	if err != nil {
		return nil, err
	}
	oldDir, err := g.subtree(ctx, repo, oldRoot, dir)
	if err != nil {
		return nil, err
	}
	if oldDir == "" {
		return nil, fmt.Errorf("%w: %s did not exist at %s", ErrNoChange, dir, short(req.ToRevision))
	}
	if oldDir == curDir {
		return nil, fmt.Errorf("%w: %s on %s already matches %s", ErrNoChange, dir, base, short(req.ToRevision))
	}
	oldFiles, err := g.blobs(ctx, repo, oldDir)
	if err != nil {
		return nil, err
	}
	curFiles, err := g.blobs(ctx, repo, curDir)
	if err != nil {
		return nil, err
	}
	entries := diffEntries(dir, oldFiles, curFiles)

	var newTree struct {
		SHA string `json:"sha"`
	}
	if err := g.do(ctx, http.MethodPost, repo+"/git/trees", map[string]any{"base_tree": headTree, "tree": entries}, &newTree); err != nil {
		return nil, err
	}

	trigger := "Requested by " + req.User
	if req.Auto {
		trigger = "Opened automatically: " + req.Reason
	}
	message := fmt.Sprintf("%s\n\nRestores %s to its state at %s. %s.\n\n%s\n", TitleFor(req.App, req.ToRevision), dir, req.ToRevision, trigger, TrailerFor(req.App))
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := g.do(ctx, http.MethodPost, repo+"/git/commits", map[string]any{"message": message, "tree": newTree.SHA, "parents": []string{head}}, &commit); err != nil {
		return nil, err
	}

	branch := branchPrefix + short(req.ToRevision)
	if err := g.do(ctx, http.MethodPost, repo+"/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": commit.SHA}, nil); err != nil {
		if isStatus(err, http.StatusUnprocessableEntity) {
			return nil, fmt.Errorf("%w: branch %s already exists (a closed pull request leaves its branch until deleted)", ErrExists, branch)
		}
		return nil, err
	}

	body := g.describe(ctx, repo, req, dir, base, entries)
	var pr pull
	if err := g.do(ctx, http.MethodPost, repo+"/pulls", map[string]any{
		"title": TitleFor(req.App, req.ToRevision),
		"head":  branch, "base": base, "body": body,
	}, &pr); err != nil {
		if delErr := g.do(ctx, http.MethodDelete, repo+"/git/refs/heads/"+branch, nil, nil); delErr != nil {
			return nil, fmt.Errorf("%w (and deleting branch %s failed: %v)", err, branch, delErr)
		}
		return nil, err
	}
	return &PullRequest{Number: pr.Number, URL: pr.HTMLURL, Branch: branch}, nil
}

func (g *GitHub) describe(ctx context.Context, repo string, req Request, dir, base string, entries []treeEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Argo CD application **%s** self-heals, so a rollback has to go through git.\n\n", req.App)
	fmt.Fprintf(&b, "This restores `%s` to how it was at `%s`, the commit Argo CD synced before `%s`. ", dir, short(req.ToRevision), short(req.FromRevision))
	if req.Auto {
		fmt.Fprintf(&b, "clusterdeck opened it automatically because the rollout failed: %s.\n\n", req.Reason)
	} else {
		fmt.Fprintf(&b, "Requested by %s in clusterdeck.\n\n", req.User)
	}
	b.WriteString("### Files restored\n\n")
	for i, e := range entries {
		if i == maxListed {
			fmt.Fprintf(&b, "- … and %d more\n", len(entries)-maxListed)
			break
		}
		verb := "restored"
		if e.SHA == nil {
			verb = "deleted (added since)"
		}
		fmt.Fprintf(&b, "- `%s` %s\n", e.Path, verb)
	}

	var cmp struct {
		TotalCommits int      `json:"total_commits"`
		Commits      []commit `json:"commits"`
		Files        []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	cmpErr := g.do(ctx, http.MethodGet, repo+"/compare/"+url.PathEscape(req.ToRevision)+"..."+url.PathEscape(base), nil, &cmp)

	parent := path.Dir(dir)
	if parent == "." {
		parent = ""
	}
	rendered, resolveErr := g.renderedFrom(ctx, repo, dir, base, req.ToRevision)
	var outside []string
	if cmpErr == nil {
		for _, f := range cmp.Files {
			if strings.HasPrefix(f.Filename, dir+"/") {
				continue
			}
			if resolveErr == nil && rendered.has(f.Filename) || resolveErr != nil && (parent == "" || strings.HasPrefix(f.Filename, parent+"/")) {
				outside = append(outside, f.Filename)
			}
		}
	}

	paths := []string{dir}
	skipped := 0
	if resolveErr == nil {
		for i, f := range outside {
			if i == maxPathQueries {
				skipped = len(outside) - maxPathQueries
				break
			}
			paths = append(paths, f)
		}
	} else if parent != "" {
		paths = append(paths, parent)
	}
	var inRange map[string]bool
	if cmpErr == nil && cmp.TotalCommits <= len(cmp.Commits) {
		inRange = map[string]bool{}
		for _, c := range cmp.Commits {
			inRange[c.SHA] = true
		}
	}
	if since, err := g.commitsTouching(ctx, repo, base, req.ToRevision, paths, inRange); err == nil && len(since) > 0 {
		scope := "`" + dir + "`"
		switch {
		case resolveErr == nil && len(outside) > 0:
			scope += " or a file listed under *Not reverted*"
		case resolveErr != nil && parent != "":
			scope += " or `" + parent + "/`"
		}
		fmt.Fprintf(&b, "\n### Commits on %s since then touching %s\n\n", base, scope)
		for i, c := range since {
			if i == maxListed {
				fmt.Fprintf(&b, "- … and %d more\n", len(since)-maxListed)
				break
			}
			title, _, _ := strings.Cut(c.Commit.Message, "\n")
			fmt.Fprintf(&b, "- `%s` %s\n", short(c.SHA), title)
		}
		if skipped > 0 {
			fmt.Fprintf(&b, "\nCommits that only touched the last %d files under *Not reverted* are not listed.\n", skipped)
		}
	}

	if len(outside) > 0 {
		if resolveErr == nil {
			b.WriteString("\n### Not reverted: changed since, outside the application's path, used by its kustomization\n\n")
			fmt.Fprintf(&b, "`%s` includes these through its kustomization (`resources`, `components`, `patches` and the like) at `%s` or `%s`; check whether they caused the failure.\n\n", dir, short(req.ToRevision), short(base))
		} else {
			where := "`" + parent + "/`"
			if parent == "" {
				where = "the repository"
			}
			fmt.Fprintf(&b, "\n### Not reverted: changed since, in %s outside the application's path\n\n", where)
			fmt.Fprintf(&b, "clusterdeck could not resolve the application's kustomization (%v), so this lists every changed file in %s. If the application includes any of them, check whether they caused the failure.\n\n", resolveErr, where)
		}
		for i, f := range outside {
			if i == maxListed {
				fmt.Fprintf(&b, "- … and %d more\n", len(outside)-maxListed)
				break
			}
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
	}
	if cmpErr == nil && len(cmp.Files) >= maxCompareFiles {
		fmt.Fprintf(&b, "\nGitHub lists at most %d changed files for a comparison, so files changed beyond that are missing here.\n", maxCompareFiles)
	}
	b.WriteString("\n---\n_Opened by clusterdeck._\n")
	return b.String()
}

type commit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message   string `json:"message"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

func (g *GitHub) commitsTouching(ctx context.Context, repo, base, from string, paths []string, inRange map[string]bool) ([]commit, error) {
	var start commit
	if err := g.do(ctx, http.MethodGet, repo+"/commits/"+url.PathEscape(from), nil, &start); err != nil {
		return nil, err
	}
	since := start.Commit.Committer.Date
	seen := map[string]commit{}
	for _, p := range paths {
		q := url.Values{"sha": {base}, "path": {p}, "since": {since.UTC().Format(time.RFC3339)}, "per_page": {"100"}}
		var list []commit
		if err := g.do(ctx, http.MethodGet, repo+"/commits?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, c := range list {
			if c.SHA == start.SHA {
				continue
			}
			if inRange != nil && !inRange[c.SHA] || inRange == nil && !c.Commit.Committer.Date.After(since) {
				continue
			}
			seen[c.SHA] = c
		}
	}
	out := make([]commit, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Commit.Committer.Date, out[j].Commit.Committer.Date
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].SHA < out[j].SHA
	})
	return out, nil
}

func (g *GitHub) Get(ctx context.Context, p string, out any) error {
	return g.do(ctx, http.MethodGet, p, nil, out)
}

func IsForbidden(err error) bool { return isStatus(err, http.StatusForbidden) }
