package kube

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	AnnRevision            = "deployment.kubernetes.io/revision"
	AnnChangeCause         = "kubernetes.io/change-cause"
	AnnArgoTracking        = "argocd.argoproj.io/tracking-id"
	LabelArgoInstance      = "app.kubernetes.io/instance"
	AnnAutoRollback        = "clusterdeck.io/auto-rollback"
	AnnRollbackGrace       = "clusterdeck.io/rollback-grace"
	AnnAutoRollbackFrom    = "clusterdeck.io/auto-rollback-from"
	AnnAutoRollbackTo      = "clusterdeck.io/auto-rollback-to"
	AnnAutoRollbackAt      = "clusterdeck.io/auto-rollback-at"
	DefaultRollbackGrace   = 5 * time.Minute
	maxHistory             = 10
	StageDone, StageActive = "done", "active"
	StageFailed            = "failed"
	StagePending           = "pending"
)

var badWaiting = map[string]bool{
	"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "InvalidImageName": true,
}

type Stage struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

type Revision struct {
	Revision    int64     `json:"revision"`
	Images      []string  `json:"images"`
	Created     time.Time `json:"created"`
	Replicas    int32     `json:"replicas"`
	Ready       int32     `json:"ready"`
	Current     bool      `json:"current"`
	ChangeCause string    `json:"changeCause"`
}

type SyncRecord struct {
	ID         int64      `json:"id"`
	Revision   string     `json:"revision"`
	DeployedAt *time.Time `json:"deployedAt"`
}

type Manager struct {
	Kind         string       `json:"kind"`
	Application  string       `json:"application"`
	AutoSync     bool         `json:"autoSync"`
	SelfHeal     bool         `json:"selfHeal"`
	SyncStatus   string       `json:"syncStatus"`
	Health       string       `json:"health"`
	SyncRevision string       `json:"syncRevision"`
	History      []SyncRecord `json:"history"`
}

type RollbackPolicy struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	Auto    bool   `json:"auto"`
	Grace   string `json:"grace"`
}

type AutoRollbackRecord struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	At   string `json:"at"`
}

type Rollout struct {
	Namespace    string              `json:"namespace"`
	Name         string              `json:"name"`
	Revision     int64               `json:"revision"`
	Desired      int32               `json:"desired"`
	Updated      int32               `json:"updated"`
	Ready        int32               `json:"ready"`
	Available    int32               `json:"available"`
	Phase        string              `json:"phase"`
	Message      string              `json:"message"`
	Problems     []string            `json:"problems"`
	Stages       []Stage             `json:"stages"`
	History      []Revision          `json:"history"`
	Manager      Manager             `json:"manager"`
	Rollback     RollbackPolicy      `json:"rollback"`
	LastAutoBack *AutoRollbackRecord `json:"lastAutoRollback"`
}

type argoApp struct {
	name, namespace string
	obj             map[string]any
}

func revisionOf(meta metav1.ObjectMeta) int64 {
	n, _ := strconv.ParseInt(meta.Annotations[AnnRevision], 10, 64)
	return n
}

func (c *Client) Rollouts(ctx context.Context, ns, app string) ([]Rollout, error) {
	deps, err := c.Core.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	rss, err := c.Core.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list replicasets: %w", err)
	}
	pods, err := c.Core.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	apps := c.argoApps(ctx)
	out := []Rollout{}
	for _, d := range deps.Items {
		if appName(d.ObjectMeta, d.Spec.Template) != app {
			continue
		}
		out = append(out, c.describeRollout(&d, rss.Items, pods.Items, apps))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func appName(meta metav1.ObjectMeta, tmpl corev1.PodTemplateSpec) string {
	if n := tmpl.Labels[AppLabel]; n != "" {
		return n
	}
	if n := meta.Labels[AppLabel]; n != "" {
		return n
	}
	return meta.Name
}

func ownedReplicaSets(d *appsv1.Deployment, rss []appsv1.ReplicaSet) []appsv1.ReplicaSet {
	var out []appsv1.ReplicaSet
	for _, rs := range rss {
		for _, ref := range rs.OwnerReferences {
			if ref.Kind == "Deployment" && ref.Name == d.Name && (d.UID == "" || ref.UID == d.UID) {
				out = append(out, rs)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return revisionOf(out[i].ObjectMeta) > revisionOf(out[j].ObjectMeta) })
	return out
}

func (c *Client) describeRollout(d *appsv1.Deployment, allRS []appsv1.ReplicaSet, pods []corev1.Pod, apps []argoApp) Rollout {
	r := Rollout{
		Namespace: d.Namespace,
		Name:      d.Name,
		Revision:  revisionOf(d.ObjectMeta),
		Desired:   replicas(d.Spec.Replicas),
		Updated:   d.Status.UpdatedReplicas,
		Ready:     d.Status.ReadyReplicas,
		Available: d.Status.AvailableReplicas,
		Problems:  []string{},
		History:   []Revision{},
	}
	owned := ownedReplicaSets(d, allRS)
	var current *appsv1.ReplicaSet
	for i, rs := range owned {
		rev := revisionOf(rs.ObjectMeta)
		if rev == r.Revision && current == nil {
			current = &owned[i]
		}
		if len(r.History) < maxHistory {
			images := []string{}
			for _, ctr := range rs.Spec.Template.Spec.Containers {
				images = append(images, ctr.Image)
			}
			r.History = append(r.History, Revision{
				Revision: rev, Images: images, Created: rs.CreationTimestamp.Time,
				Replicas: rs.Status.Replicas, Ready: rs.Status.ReadyReplicas,
				Current: rev == r.Revision, ChangeCause: rs.Annotations[AnnChangeCause],
			})
		}
	}

	failed, problems := rolloutFailure(d, current, pods)
	r.Problems = problems
	progressing := d.Status.ObservedGeneration < d.Generation || r.Updated < r.Desired || d.Status.Replicas > r.Updated || r.Available < r.Desired
	switch {
	case d.Spec.Paused:
		r.Phase = "paused"
	case failed:
		r.Phase = "failed"
	case progressing:
		r.Phase = "progressing"
	default:
		r.Phase = "complete"
	}
	for _, cond := range d.Status.Conditions {
		if cond.Type == appsv1.DeploymentProgressing {
			r.Message = cond.Message
		}
	}

	r.Manager = managerFor(d, apps)
	r.Stages = stages(r, d)
	r.Rollback = rollbackPolicy(d, r.Manager, len(owned))
	if from, ok := d.Annotations[AnnAutoRollbackFrom]; ok {
		f, _ := strconv.ParseInt(from, 10, 64)
		t, _ := strconv.ParseInt(d.Annotations[AnnAutoRollbackTo], 10, 64)
		r.LastAutoBack = &AutoRollbackRecord{From: f, To: t, At: d.Annotations[AnnAutoRollbackAt]}
	}
	return r
}

func rolloutFailure(d *appsv1.Deployment, current *appsv1.ReplicaSet, pods []corev1.Pod) (bool, []string) {
	problems := []string{}
	failed := false
	for _, cond := range d.Status.Conditions {
		if cond.Type == appsv1.DeploymentProgressing && cond.Status == corev1.ConditionFalse && cond.Reason == "ProgressDeadlineExceeded" {
			failed = true
			problems = append(problems, "progress deadline exceeded")
		}
	}
	if current == nil {
		return failed, problems
	}
	hash := current.Labels["pod-template-hash"]
	seen := map[string]bool{}
	for _, p := range pods {
		if hash == "" || p.Labels["pod-template-hash"] != hash {
			continue
		}
		for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			if w := cs.State.Waiting; w != nil && badWaiting[w.Reason] && !seen[w.Reason] {
				seen[w.Reason] = true
				failed = true
				problems = append(problems, fmt.Sprintf("%s: %s", p.Name, w.Reason))
			}
		}
	}
	return failed, problems
}

func stages(r Rollout, d *appsv1.Deployment) []Stage {
	out := []Stage{}
	if r.Manager.Kind == "argocd" {
		st := StageDone
		switch r.Manager.SyncStatus {
		case "OutOfSync":
			st = StageActive
		case "Unknown", "":
			st = StagePending
		}
		out = append(out, Stage{"Synced", st, strings.TrimSpace(r.Manager.SyncStatus + " " + short(r.Manager.SyncRevision))})
	} else {
		out = append(out, Stage{"Applied", StageDone, fmt.Sprintf("generation %d", d.Generation)})
	}

	rollState := StageDone
	if r.Updated < r.Desired || d.Status.ObservedGeneration < d.Generation || d.Status.Replicas > r.Updated {
		rollState = StageActive
	}
	if r.Phase == "failed" {
		rollState = StageFailed
	}
	if r.Phase == "paused" {
		rollState = StagePending
	}
	out = append(out, Stage{"Rolled out", rollState, fmt.Sprintf("revision %d · %d/%d updated", r.Revision, r.Updated, r.Desired)})

	availState := StageDone
	if r.Available < r.Desired {
		availState = StageActive
		if rollState == StageFailed {
			availState = StageFailed
		} else if rollState == StagePending {
			availState = StagePending
		}
	}
	out = append(out, Stage{"Available", availState, fmt.Sprintf("%d/%d available", r.Available, r.Desired)})

	healthState := StageDone
	detail := "no problems seen"
	switch {
	case len(r.Problems) > 0:
		healthState, detail = StageFailed, strings.Join(r.Problems, "; ")
	case availState != StageDone:
		healthState, detail = StagePending, "waiting for the rollout"
	case r.Manager.Kind == "argocd" && r.Manager.Health != "" && r.Manager.Health != "Healthy":
		healthState, detail = StageActive, "Argo CD: "+r.Manager.Health
	}
	return append(out, Stage{"Healthy", healthState, detail})
}

func short(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

func rollbackPolicy(d *appsv1.Deployment, m Manager, revisions int) RollbackPolicy {
	p := RollbackPolicy{Allowed: true, Auto: d.Annotations[AnnAutoRollback] == "true", Grace: DefaultRollbackGrace.String()}
	if g, err := time.ParseDuration(d.Annotations[AnnRollbackGrace]); err == nil && g > 0 {
		p.Grace = g.String()
	}
	switch {
	case m.Kind == "argocd" && m.SelfHeal:
		p.Allowed = false
		p.Reason = fmt.Sprintf("Argo CD application %s self-heals, so it would undo a rollback within seconds. Roll back in git instead (revert to the commit before %s).", m.Application, short(m.SyncRevision))
	case d.Spec.Paused:
		p.Allowed = false
		p.Reason = "the deployment is paused"
	case revisions < 2:
		p.Allowed = false
		p.Reason = "there is no earlier revision"
	case m.Kind == "argocd" && m.AutoSync:
		p.Reason = fmt.Sprintf("Argo CD will show %s OutOfSync and re-apply git on the next change.", m.Application)
	}
	return p
}

func (c *Client) argoApps(ctx context.Context) []argoApp {
	if c.Dynamic == nil {
		return nil
	}
	list, err := c.Dynamic.Resource(argoAppGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	out := make([]argoApp, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, argoApp{name: item.GetName(), namespace: item.GetNamespace(), obj: item.Object})
	}
	return out
}

func managerFor(d *appsv1.Deployment, apps []argoApp) Manager {
	m := Manager{Kind: "none", History: []SyncRecord{}}
	var match *argoApp
	tracked := ""
	if id := d.Annotations[AnnArgoTracking]; id != "" {
		tracked, _, _ = strings.Cut(id, ":")
	} else if inst := d.Labels[LabelArgoInstance]; inst != "" {
		tracked = inst
	}
	for i, a := range apps {
		if tracked != "" && a.name == tracked {
			match = &apps[i]
			break
		}
	}
	if match == nil && tracked == "" {
		for i, a := range apps {
			if ns, _, _ := unstructured.NestedString(a.obj, "spec", "destination", "namespace"); ns == d.Namespace {
				match = &apps[i]
				break
			}
		}
	}
	if match == nil {
		return m
	}
	m.Kind, m.Application = "argocd", match.name
	if auto, found, _ := unstructured.NestedMap(match.obj, "spec", "syncPolicy", "automated"); found && auto != nil {
		enabled, hasEnabled, _ := unstructured.NestedBool(match.obj, "spec", "syncPolicy", "automated", "enabled")
		m.AutoSync = !hasEnabled || enabled
		heal, _, _ := unstructured.NestedBool(match.obj, "spec", "syncPolicy", "automated", "selfHeal")
		m.SelfHeal = m.AutoSync && heal
	}
	m.SyncStatus, _, _ = unstructured.NestedString(match.obj, "status", "sync", "status")
	m.SyncRevision, _, _ = unstructured.NestedString(match.obj, "status", "sync", "revision")
	m.Health, _, _ = unstructured.NestedString(match.obj, "status", "health", "status")
	history, _, _ := unstructured.NestedSlice(match.obj, "status", "history")
	for i := len(history) - 1; i >= 0 && len(m.History) < maxHistory; i-- {
		h, ok := history[i].(map[string]any)
		if !ok {
			continue
		}
		rec := SyncRecord{}
		rec.ID, _, _ = unstructured.NestedInt64(h, "id")
		rec.Revision, _, _ = unstructured.NestedString(h, "revision")
		if at, _, _ := unstructured.NestedString(h, "deployedAt"); at != "" {
			if t, err := time.Parse(time.RFC3339, at); err == nil {
				rec.DeployedAt = &t
			}
		}
		m.History = append(m.History, rec)
	}
	return m
}

func (c *Client) Rollback(ctx context.Context, ns, name string, toRevision int64, user string) (string, error) {
	d, err := c.Core.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	rss, err := c.Core.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	owned := ownedReplicaSets(d, rss.Items)
	policy := rollbackPolicy(d, managerFor(d, c.argoApps(ctx)), len(owned))
	if !policy.Allowed {
		return "", fmt.Errorf("%w: %s", ErrForbidden, policy.Reason)
	}
	target, err := pickRevision(d, owned, toRevision)
	if err != nil {
		return "", err
	}
	from := revisionOf(d.ObjectMeta)
	if err := c.applyRevision(ctx, d, target, nil); err != nil {
		return "", err
	}
	c.event(ctx, d, corev1.EventTypeNormal, "ClusterdeckRollback", fmt.Sprintf("%s rolled back from revision %d to %d", user, from, revisionOf(target.ObjectMeta)))
	return fmt.Sprintf("rolled back %s/%s from revision %d to %d", ns, name, from, revisionOf(target.ObjectMeta)), nil
}

func pickRevision(d *appsv1.Deployment, owned []appsv1.ReplicaSet, toRevision int64) (*appsv1.ReplicaSet, error) {
	current := revisionOf(d.ObjectMeta)
	if toRevision == current && toRevision != 0 {
		return nil, fmt.Errorf("%w: revision %d is already current", ErrInvalid, toRevision)
	}
	for i, rs := range owned {
		rev := revisionOf(rs.ObjectMeta)
		if (toRevision == 0 && rev < current) || (toRevision != 0 && rev == toRevision) {
			return &owned[i], nil
		}
	}
	if toRevision == 0 {
		return nil, fmt.Errorf("%w: no revision before %d", ErrInvalid, current)
	}
	return nil, fmt.Errorf("%w: revision %d not found (only the last %d are kept)", ErrInvalid, toRevision, len(owned))
}

func (c *Client) applyRevision(ctx context.Context, d *appsv1.Deployment, target *appsv1.ReplicaSet, annotations map[string]string) error {
	tmpl := target.Spec.Template.DeepCopy()
	delete(tmpl.Labels, "pod-template-hash")
	updated := d.DeepCopy()
	updated.Spec.Template = *tmpl
	if len(annotations) > 0 && updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		updated.Annotations[k] = v
	}
	_, err := c.Core.AppsV1().Deployments(d.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

func (c *Client) event(ctx context.Context, d *appsv1.Deployment, kind, reason, msg string) {
	now := metav1.NewTime(c.Now())
	c.Core.CoreV1().Events(d.Namespace).Create(ctx, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{GenerateName: d.Name + ".", Namespace: d.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Deployment", APIVersion: "apps/v1", Name: d.Name, Namespace: d.Namespace, UID: d.UID},
		Type:           kind,
		Reason:         reason,
		Message:        msg,
		Source:         corev1.EventSource{Component: "clusterdeck"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}, metav1.CreateOptions{})
}

type AutoRollbackResult struct {
	Namespace, Name string
	From, To        int64
	Reason          string
}

func (c *Client) AutoRollback(ctx context.Context, namespaces []string) ([]AutoRollbackResult, error) {
	scopes := namespaces
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	apps := c.argoApps(ctx)
	var done []AutoRollbackResult
	for _, ns := range scopes {
		deps, err := c.Core.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return done, fmt.Errorf("list deployments: %w", err)
		}
		rsByNS := map[string][]appsv1.ReplicaSet{}
		podsByNS := map[string][]corev1.Pod{}
		for i := range deps.Items {
			d := &deps.Items[i]
			if d.Annotations[AnnAutoRollback] != "true" || d.Spec.Paused {
				continue
			}
			if _, ok := rsByNS[d.Namespace]; !ok {
				rss, err := c.Core.AppsV1().ReplicaSets(d.Namespace).List(ctx, metav1.ListOptions{})
				if err != nil {
					return done, err
				}
				pods, err := c.Core.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{})
				if err != nil {
					return done, err
				}
				rsByNS[d.Namespace], podsByNS[d.Namespace] = rss.Items, pods.Items
			}
			res, err := c.autoRollbackOne(ctx, d, rsByNS[d.Namespace], podsByNS[d.Namespace], apps)
			if err != nil {
				return done, fmt.Errorf("%s/%s: %w", d.Namespace, d.Name, err)
			}
			if res != nil {
				done = append(done, *res)
			}
		}
	}
	return done, nil
}

func (c *Client) autoRollbackOne(ctx context.Context, d *appsv1.Deployment, rss []appsv1.ReplicaSet, pods []corev1.Pod, apps []argoApp) (*AutoRollbackResult, error) {
	current := revisionOf(d.ObjectMeta)
	if to, _ := strconv.ParseInt(d.Annotations[AnnAutoRollbackTo], 10, 64); to != 0 && to == current {
		return nil, nil
	}
	owned := ownedReplicaSets(d, rss)
	if !rollbackPolicy(d, managerFor(d, apps), len(owned)).Allowed {
		return nil, nil
	}
	var currentRS *appsv1.ReplicaSet
	for i := range owned {
		if revisionOf(owned[i].ObjectMeta) == current {
			currentRS = &owned[i]
			break
		}
	}
	if currentRS == nil {
		return nil, nil
	}
	failed, problems := rolloutFailure(d, currentRS, pods)
	if !failed {
		return nil, nil
	}
	grace := DefaultRollbackGrace
	if g, err := time.ParseDuration(d.Annotations[AnnRollbackGrace]); err == nil && g > 0 {
		grace = g
	}
	deadline := strings.Contains(strings.Join(problems, ";"), "progress deadline")
	if !deadline && c.Now().Sub(currentRS.CreationTimestamp.Time) < grace {
		return nil, nil
	}
	target, err := pickRevision(d, owned, 0)
	if err != nil {
		return nil, nil
	}
	newRev := revisionOf(owned[0].ObjectMeta) + 1
	err = c.applyRevision(ctx, d, target, map[string]string{
		AnnAutoRollbackFrom: strconv.FormatInt(current, 10),
		AnnAutoRollbackTo:   strconv.FormatInt(newRev, 10),
		AnnAutoRollbackAt:   c.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	reason := strings.Join(problems, "; ")
	c.event(ctx, d, corev1.EventTypeWarning, "ClusterdeckAutoRollback",
		fmt.Sprintf("revision %d failed (%s); rolled back to the template of revision %d", current, reason, revisionOf(target.ObjectMeta)))
	return &AutoRollbackResult{Namespace: d.Namespace, Name: d.Name, From: current, To: revisionOf(target.ObjectMeta), Reason: reason}, nil
}
