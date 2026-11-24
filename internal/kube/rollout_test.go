package kube

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func rolloutDeployment(rev int64, annotations map[string]string) *appsv1.Deployment {
	d := deployment("shop", "web", "shop", 2, 2)
	d.UID = types.UID("dep-web")
	d.Generation, d.Status.ObservedGeneration = 3, 3
	d.Status.UpdatedReplicas, d.Status.AvailableReplicas, d.Status.Replicas = 2, 2, 2
	d.Annotations = map[string]string{AnnRevision: strconv.FormatInt(rev, 10)}
	for k, v := range annotations {
		d.Annotations[k] = v
	}
	d.Spec.Template.Spec.Containers[0].Image = "web:" + strconv.FormatInt(rev, 10)
	return d
}

func rs(rev int64, image string, age time.Duration) *appsv1.ReplicaSet {
	hash := "h" + strconv.FormatInt(rev, 10)
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-" + hash, Namespace: "shop", CreationTimestamp: metav1.NewTime(now.Add(-age)),
			Labels:          map[string]string{"pod-template-hash": hash},
			Annotations:     map[string]string{AnnRevision: strconv.FormatInt(rev, 10)},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: "dep-web"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pod": "web", "pod-template-hash": hash, AppLabel: "shop"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
		}},
	}
}

func crashingPod(rev int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-new-1", Namespace: "shop", Labels: map[string]string{"pod": "web", "pod-template-hash": "h" + strconv.FormatInt(rev, 10)}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
}

func argoDyn(t *testing.T, apps ...map[string]any) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nodeMetricsGVR: "NodeMetricsList", argoAppGVR: "ApplicationList",
	})
	for _, a := range apps {
		if err := dyn.Tracker().Create(argoAppGVR, &unstructured.Unstructured{Object: a}, "argocd"); err != nil {
			t.Fatal(err)
		}
	}
	return dyn
}

func argoAppObj(name string, automated map[string]any) map[string]any {
	spec := map[string]any{"destination": map[string]any{"namespace": "shop"}}
	if automated != nil {
		spec["syncPolicy"] = map[string]any{"automated": automated}
	}
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": name, "namespace": "argocd"},
		"spec":     spec,
		"status": map[string]any{
			"sync":   map[string]any{"status": "Synced", "revision": "abcdef1234"},
			"health": map[string]any{"status": "Healthy"},
			"history": []any{
				map[string]any{"id": int64(1), "revision": "1111111", "deployedAt": "2026-11-20T10:00:00Z"},
				map[string]any{"id": int64(2), "revision": "abcdef1234", "deployedAt": "2026-11-24T10:00:00Z"},
			},
		},
	}
}

func TestRolloutsHealthyWithoutGitOps(t *testing.T) {
	c, _ := newClient(t, nil, rolloutDeployment(3, nil), rs(1, "web:1", 72*time.Hour), rs(2, "web:2", 48*time.Hour), rs(3, "web:3", time.Hour))
	rs, err := c.Rollouts(context.Background(), "shop", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("rollouts = %+v", rs)
	}
	r := rs[0]
	if r.Phase != "complete" || r.Revision != 3 || r.Manager.Kind != "none" || !r.Rollback.Allowed || r.Rollback.Auto {
		t.Fatalf("rollout = %+v", r)
	}
	if len(r.History) != 3 || r.History[0].Revision != 3 || !r.History[0].Current || r.History[2].Images[0] != "web:1" {
		t.Fatalf("history = %+v", r.History)
	}
	want := []string{"Applied:done", "Rolled out:done", "Available:done", "Healthy:done"}
	for i, st := range r.Stages {
		if st.Name+":"+st.State != want[i] {
			t.Fatalf("stages = %+v", r.Stages)
		}
	}
}

func TestRolloutsFailedStage(t *testing.T) {
	d := rolloutDeployment(3, nil)
	d.Status.AvailableReplicas = 1
	c, _ := newClient(t, nil, d, rs(2, "web:2", 48*time.Hour), rs(3, "web:3", time.Minute), crashingPod(3))
	r, _ := c.Rollouts(context.Background(), "shop", "shop")
	if r[0].Phase != "failed" || r[0].Stages[3].State != StageFailed || !strings.Contains(r[0].Stages[3].Detail, "CrashLoopBackOff") {
		t.Fatalf("rollout = %+v", r[0])
	}
}

func TestManagerDetection(t *testing.T) {
	cases := []struct {
		name      string
		automated map[string]any
		tracking  string
		autoSync  bool
		selfHeal  bool
		allowed   bool
	}{
		{"manual sync", nil, "", false, false, true},
		{"auto sync only", map[string]any{"prune": true}, "", true, false, true},
		{"self heal", map[string]any{"selfHeal": true}, "", true, true, false},
		{"self heal disabled via enabled=false", map[string]any{"selfHeal": true, "enabled": false}, "", false, false, true},
		{"tracking id names the app", map[string]any{"selfHeal": true}, "shop:apps/Deployment:shop/web", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ann := map[string]string{}
			if tc.tracking != "" {
				ann[AnnArgoTracking] = tc.tracking
			}
			dyn := argoDyn(t, argoAppObj("shop", tc.automated))
			c, _ := newClient(t, dyn, rolloutDeployment(2, ann), rs(1, "web:1", time.Hour), rs(2, "web:2", time.Minute))
			r, err := c.Rollouts(context.Background(), "shop", "shop")
			if err != nil {
				t.Fatal(err)
			}
			m := r[0].Manager
			if m.Kind != "argocd" || m.Application != "shop" || m.AutoSync != tc.autoSync || m.SelfHeal != tc.selfHeal || r[0].Rollback.Allowed != tc.allowed {
				t.Fatalf("manager = %+v rollback = %+v", m, r[0].Rollback)
			}
			if len(m.History) != 2 || m.History[0].ID != 2 || m.History[0].DeployedAt == nil {
				t.Fatalf("history newest first: %+v", m.History)
			}
			if r[0].Stages[0].Name != "Synced" || !strings.Contains(r[0].Stages[0].Detail, "abcdef1") {
				t.Fatalf("sync stage = %+v", r[0].Stages[0])
			}
		})
	}
}

func TestTrackingToAnotherAppIsNotMatchedByNamespace(t *testing.T) {
	dyn := argoDyn(t, argoAppObj("shop", map[string]any{"selfHeal": true}))
	c, _ := newClient(t, dyn, rolloutDeployment(2, map[string]string{AnnArgoTracking: "other:apps/Deployment:shop/web"}), rs(1, "web:1", time.Hour), rs(2, "web:2", time.Minute))
	r, _ := c.Rollouts(context.Background(), "shop", "shop")
	if r[0].Manager.Kind != "none" {
		t.Fatalf("a deployment tracked by an unknown app must not borrow its namespace's app: %+v", r[0].Manager)
	}
}

func TestManualRollback(t *testing.T) {
	ctx := context.Background()
	c, cs := newClient(t, nil, rolloutDeployment(3, nil), rs(1, "web:1", 72*time.Hour), rs(2, "web:2", 48*time.Hour), rs(3, "web:3", time.Hour))

	if _, err := c.Rollback(ctx, "shop", "web", 3, "me"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rolling back to the current revision: %v", err)
	}
	if _, err := c.Rollback(ctx, "shop", "web", 9, "me"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown revision: %v", err)
	}
	msg, err := c.Rollback(ctx, "shop", "web", 0, "me")
	if err != nil || !strings.Contains(msg, "from revision 3 to 2") {
		t.Fatalf("msg = %q err = %v", msg, err)
	}
	d, _ := cs.AppsV1().Deployments("shop").Get(ctx, "web", metav1.GetOptions{})
	if d.Spec.Template.Spec.Containers[0].Image != "web:2" || d.Spec.Template.Labels["pod-template-hash"] != "" {
		t.Fatalf("template = %+v", d.Spec.Template)
	}
	if _, err := c.Rollback(ctx, "shop", "web", 1, "me"); err != nil {
		t.Fatal(err)
	}
	d, _ = cs.AppsV1().Deployments("shop").Get(ctx, "web", metav1.GetOptions{})
	if d.Spec.Template.Spec.Containers[0].Image != "web:1" {
		t.Fatalf("explicit revision: %s", d.Spec.Template.Spec.Containers[0].Image)
	}
	events, _ := cs.CoreV1().Events("shop").List(ctx, metav1.ListOptions{})
	if len(events.Items) == 0 || events.Items[0].Reason != "ClusterdeckRollback" {
		t.Fatalf("events = %+v", events.Items)
	}
}

func TestRollbackRefusedWhenArgoSelfHeals(t *testing.T) {
	dyn := argoDyn(t, argoAppObj("shop", map[string]any{"selfHeal": true}))
	c, cs := newClient(t, dyn, rolloutDeployment(2, nil), rs(1, "web:1", time.Hour), rs(2, "web:2", time.Minute))
	_, err := c.Rollback(context.Background(), "shop", "web", 0, "me")
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "git") {
		t.Fatalf("err = %v", err)
	}
	d, _ := cs.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if d.Spec.Template.Spec.Containers[0].Image != "web:2" {
		t.Fatal("a refused rollback must change nothing")
	}
}

func TestAutoRollback(t *testing.T) {
	ctx := context.Background()
	opted := map[string]string{AnnAutoRollback: "true"}

	t.Run("healthy rollout is left alone", func(t *testing.T) {
		c, _ := newClient(t, nil, rolloutDeployment(2, opted), rs(1, "web:1", time.Hour), rs(2, "web:2", time.Hour))
		if res, err := c.AutoRollback(ctx, nil); err != nil || len(res) != 0 {
			t.Fatalf("res = %+v err = %v", res, err)
		}
	})
	t.Run("not opted in", func(t *testing.T) {
		c, _ := newClient(t, nil, rolloutDeployment(2, nil), rs(1, "web:1", time.Hour), rs(2, "web:2", time.Hour), crashingPod(2))
		if res, _ := c.AutoRollback(ctx, nil); len(res) != 0 {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("crash inside the grace period waits", func(t *testing.T) {
		c, _ := newClient(t, nil, rolloutDeployment(2, opted), rs(1, "web:1", time.Hour), rs(2, "web:2", time.Minute), crashingPod(2))
		if res, _ := c.AutoRollback(ctx, nil); len(res) != 0 {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("crash after the grace period rolls back once", func(t *testing.T) {
		c, cs := newClient(t, nil, rolloutDeployment(2, opted), rs(1, "web:1", time.Hour), rs(2, "web:2", 10*time.Minute), crashingPod(2))
		res, err := c.AutoRollback(ctx, []string{"shop"})
		if err != nil || len(res) != 1 || res[0].From != 2 || res[0].To != 1 {
			t.Fatalf("res = %+v err = %v", res, err)
		}
		d, _ := cs.AppsV1().Deployments("shop").Get(ctx, "web", metav1.GetOptions{})
		if d.Spec.Template.Spec.Containers[0].Image != "web:1" || d.Annotations[AnnAutoRollbackFrom] != "2" || d.Annotations[AnnAutoRollbackTo] != "3" {
			t.Fatalf("deployment = %+v", d.ObjectMeta.Annotations)
		}
		events, _ := cs.CoreV1().Events("shop").List(ctx, metav1.ListOptions{})
		if len(events.Items) != 1 || events.Items[0].Type != corev1.EventTypeWarning {
			t.Fatalf("events = %+v", events.Items)
		}
	})
	t.Run("never rolls back its own rollback", func(t *testing.T) {
		ann := map[string]string{AnnAutoRollback: "true", AnnAutoRollbackFrom: "2", AnnAutoRollbackTo: "3"}
		c, _ := newClient(t, nil, rolloutDeployment(3, ann), rs(2, "web:2", time.Hour), rs(3, "web:1", time.Hour), crashingPod(3))
		if res, _ := c.AutoRollback(ctx, nil); len(res) != 0 {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("progress deadline skips the grace period", func(t *testing.T) {
		d := rolloutDeployment(2, map[string]string{AnnAutoRollback: "true", AnnRollbackGrace: "1h"})
		d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
		c, _ := newClient(t, nil, d, rs(1, "web:1", time.Hour), rs(2, "web:2", time.Minute))
		if res, _ := c.AutoRollback(ctx, nil); len(res) != 1 {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("self-healed apps are skipped", func(t *testing.T) {
		dyn := argoDyn(t, argoAppObj("shop", map[string]any{"selfHeal": true}))
		c, _ := newClient(t, dyn, rolloutDeployment(2, opted), rs(1, "web:1", time.Hour), rs(2, "web:2", time.Hour), crashingPod(2))
		if res, _ := c.AutoRollback(ctx, nil); len(res) != 0 {
			t.Fatalf("res = %+v", res)
		}
	})
}
