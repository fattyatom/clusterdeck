package gitops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"sigs.k8s.io/yaml"
)

const maxKustomizeDirs = 40

var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

type pathRef struct {
	Path string `json:"path"`
}

type generatorArgs struct {
	Files []string `json:"files"`
	Envs  []string `json:"envs"`
	Env   string   `json:"env"`
}

type kustomization struct {
	Resources             []string        `json:"resources"`
	Components            []string        `json:"components"`
	Bases                 []string        `json:"bases"`
	Crds                  []string        `json:"crds"`
	Configurations        []string        `json:"configurations"`
	Transformers          []string        `json:"transformers"`
	Generators            []string        `json:"generators"`
	Validators            []string        `json:"validators"`
	PatchesStrategicMerge []string        `json:"patchesStrategicMerge"`
	Patches               []pathRef       `json:"patches"`
	PatchesJSON6902       []pathRef       `json:"patchesJson6902"`
	Replacements          []pathRef       `json:"replacements"`
	ConfigMapGenerator    []generatorArgs `json:"configMapGenerator"`
	SecretGenerator       []generatorArgs `json:"secretGenerator"`
}

type renderSet struct {
	files map[string]bool
	dirs  map[string]bool
}

func newRenderSet() *renderSet {
	return &renderSet{files: map[string]bool{}, dirs: map[string]bool{}}
}

func (s *renderSet) has(file string) bool {
	if s.files[file] {
		return true
	}
	for d := path.Dir(file); d != "." && d != "/"; d = path.Dir(d) {
		if s.dirs[d] {
			return true
		}
	}
	return false
}

type contentEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (g *GitHub) contents(ctx context.Context, repo, p, ref string) ([]contentEntry, *contentEntry, error) {
	var parts []string
	for _, s := range strings.Split(p, "/") {
		parts = append(parts, url.PathEscape(s))
	}
	var raw json.RawMessage
	if err := g.do(ctx, http.MethodGet, repo+"/contents/"+strings.Join(parts, "/")+"?ref="+url.QueryEscape(ref), nil, &raw); err != nil {
		return nil, nil, err
	}
	if t := strings.TrimSpace(string(raw)); strings.HasPrefix(t, "[") {
		var list []contentEntry
		err := json.Unmarshal(raw, &list)
		return list, nil, err
	}
	var one contentEntry
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, nil, err
	}
	return nil, &one, nil
}

func localRef(dir, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.Contains(ref, "\n") || strings.Contains(ref, "://") || strings.HasPrefix(ref, "github.com/") || strings.HasPrefix(ref, "git@") || path.IsAbs(ref) {
		return "", false
	}
	p := path.Clean(path.Join(dir, ref))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

func looksLikeFile(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}

type kustomizeResolver struct {
	g       *GitHub
	repo    string
	ref     string
	set     *renderSet
	visited map[string]bool
}

func (r *kustomizeResolver) dir(ctx context.Context, dir string) (bool, error) {
	if r.visited[dir] {
		return true, nil
	}
	r.visited[dir] = true
	if len(r.visited) > maxKustomizeDirs {
		return false, fmt.Errorf("more than %d kustomize directories", maxKustomizeDirs)
	}
	list, file, err := r.g.contents(ctx, r.repo, dir, r.ref)
	if isStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if file != nil {
		r.set.files[dir] = true
		return false, nil
	}
	name := ""
	for _, want := range kustomizationNames {
		for _, e := range list {
			if e.Type == "file" && e.Name == want && name == "" {
				name = want
			}
		}
	}
	if name == "" {
		r.set.dirs[dir] = true
		return false, nil
	}
	kfile := path.Join(dir, name)
	_, entry, err := r.g.contents(ctx, r.repo, kfile, r.ref)
	if err != nil {
		return false, err
	}
	if entry == nil || entry.Encoding != "base64" {
		return false, fmt.Errorf("%s is not a readable file", kfile)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(entry.Content, "\n", ""))
	if err != nil {
		return false, fmt.Errorf("%s: %w", kfile, err)
	}
	var k kustomization
	if err := yaml.Unmarshal(raw, &k); err != nil {
		return false, fmt.Errorf("%s: %w", kfile, err)
	}
	r.set.files[kfile] = true

	var either, files []string
	for _, l := range [][]string{k.Resources, k.Components, k.Bases, k.Crds, k.Configurations, k.Transformers, k.Generators, k.Validators} {
		either = append(either, l...)
	}
	files = append(files, k.PatchesStrategicMerge...)
	for _, l := range [][]pathRef{k.Patches, k.PatchesJSON6902, k.Replacements} {
		for _, p := range l {
			files = append(files, p.Path)
		}
	}
	for _, gen := range append(k.ConfigMapGenerator, k.SecretGenerator...) {
		for _, f := range gen.Files {
			if _, src, ok := strings.Cut(f, "="); ok {
				f = src
			}
			files = append(files, f)
		}
		files = append(files, gen.Envs...)
		files = append(files, gen.Env)
	}
	for _, ref := range files {
		if p, ok := localRef(dir, ref); ok {
			r.set.files[p] = true
		}
	}
	for _, ref := range either {
		p, ok := localRef(dir, ref)
		if !ok {
			continue
		}
		if looksLikeFile(p) {
			r.set.files[p] = true
			continue
		}
		if _, err := r.dir(ctx, p); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (g *GitHub) renderedFrom(ctx context.Context, repo, dir string, refs ...string) (*renderSet, error) {
	set := newRenderSet()
	for _, ref := range refs {
		r := &kustomizeResolver{g: g, repo: repo, ref: ref, set: set, visited: map[string]bool{}}
		ok, err := r.dir(ctx, dir)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%s has no kustomization at %s", dir, short(ref))
		}
	}
	return set, nil
}
