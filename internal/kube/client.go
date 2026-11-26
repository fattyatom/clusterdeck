package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/fattyatom/clusterdeck/internal/gitops"
)

const (
	AppLabel         = "app.kubernetes.io/name"
	PartOfLabel      = "app.kubernetes.io/part-of"
	AnnApp           = "clusterdeck.io/app"
	AnnDescription   = "clusterdeck.io/description"
	AnnParams        = "clusterdeck.io/params"
	AnnTriggeredBy   = "clusterdeck.io/triggered-by"
	RoleBackup       = "backup"
	RoleRestoreCheck = "restore-check"
)

var (
	nodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}
	argoAppGVR     = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
)

type Options struct {
	HideNamespaces          []string
	BackupSelector          string
	RestoreCheckSelector    string
	RestoreTemplateSelector string
}

type GitRollbacker interface {
	OpenRollback(ctx context.Context, req gitops.Request) (*gitops.PullRequest, error)
	RolledBackSince(ctx context.Context, src gitops.Source, app, since string) (bool, error)
}

type Client struct {
	Core    kubernetes.Interface
	Dynamic dynamic.Interface
	Git     GitRollbacker
	Now     func() time.Time

	ActionNamespaces []string

	hidden          map[string]bool
	cronRoles       map[string]labels.Selector
	restoreSelector string
	restoreSel      labels.Selector
}

func New(core kubernetes.Interface, dyn dynamic.Interface, opts Options) (*Client, error) {
	c := &Client{Core: core, Dynamic: dyn, Now: time.Now, hidden: map[string]bool{}, cronRoles: map[string]labels.Selector{}}
	for _, ns := range opts.HideNamespaces {
		if ns = strings.TrimSpace(ns); ns != "" {
			c.hidden[ns] = true
		}
	}
	for role, sel := range map[string]string{RoleBackup: opts.BackupSelector, RoleRestoreCheck: opts.RestoreCheckSelector} {
		parsed, err := labels.Parse(sel)
		if err != nil || parsed.Empty() {
			return nil, fmt.Errorf("selector for %s must be a non-empty label selector, got %q", role, sel)
		}
		c.cronRoles[role] = parsed
	}
	restoreSel, err := labels.Parse(opts.RestoreTemplateSelector)
	if err != nil || restoreSel.Empty() {
		return nil, fmt.Errorf("restore template selector must be a non-empty label selector, got %q", opts.RestoreTemplateSelector)
	}
	c.restoreSelector, c.restoreSel = opts.RestoreTemplateSelector, restoreSel
	return c, nil
}

func (c *Client) visible(ns string) bool { return !c.hidden[ns] }

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
