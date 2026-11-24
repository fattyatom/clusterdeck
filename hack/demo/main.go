package main

import (
	"context"
	"log"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fattyatom/clusterdeck/internal/kube"
	"github.com/fattyatom/clusterdeck/internal/metrics"
	"github.com/fattyatom/clusterdeck/internal/server"
	"github.com/fattyatom/clusterdeck/web"
)

type wave struct{}

func (wave) Name() string { return "demo" }
func (wave) Query(_ context.Context, panel string, start, end time.Time, step time.Duration) ([]metrics.Series, error) {
	labels := map[string][]string{
		"node_cpu":           {"server-1", "server-2", "agent-1"},
		"node_memory":        {"server-1", "server-2", "agent-1"},
		"namespace_cpu":      {"shop", "monitoring", "argocd", "photos"},
		"namespace_memory":   {"shop", "monitoring", "argocd", "photos"},
		"namespace_restarts": {"shop", "monitoring"},
	}[panel]
	if labels == nil {
		return nil, metrics.ErrUnknownPanel
	}
	scale := map[string]float64{"node_cpu": 1.2, "node_memory": 3e9, "namespace_cpu": 0.4, "namespace_memory": 8e8, "namespace_restarts": 3}[panel]
	var out []metrics.Series
	for i, l := range labels {
		s := metrics.Series{Label: l}
		for t := start; t.Before(end); t = t.Add(step) {
			x := float64(t.Unix())/3600 + float64(i)
			v := scale * (0.6 + 0.3*math.Sin(x) + 0.1*math.Sin(x*7)) / float64(i+1)
			if panel == "namespace_restarts" {
				v = math.Floor(float64(t.Sub(start)) / float64(end.Sub(start)) * scale * float64(2-i))
			}
			s.Points = append(s.Points, metrics.Point{T: t.UnixMilli(), V: v})
		}
		out = append(out, s)
	}
	return out, nil
}

func main() {
	now := time.Now()
	ago := func(d time.Duration) metav1.Time { return metav1.NewTime(now.Add(-d)) }
	ptr := func(i int32) *int32 { return &i }

	node := func(name, role string, ready bool) *corev1.Node {
		st := corev1.ConditionTrue
		if !ready {
			st = corev1.ConditionUnknown
		}
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"node-role.kubernetes.io/" + role: "true"}},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}},
				NodeInfo:    corev1.NodeSystemInfo{KubeletVersion: "v1.34.1+k3s1"},
			},
		}
	}
	dep := func(ns, name, app string, ready, want int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr(want),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"pod": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pod": name, kube.AppLabel: app}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: "ghcr.io/example/" + name + ":1.4.2"}}},
				},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
		}
	}
	pod := func(ns, name, owner, node string, ready bool, restarts int32) *corev1.Pod {
		st := corev1.ConditionTrue
		if !ready {
			st = corev1.ConditionFalse
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"pod": owner}},
			Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: owner}}},
			Status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: st}},
				ContainerStatuses: []corev1.ContainerStatus{{RestartCount: restarts}},
			},
		}
	}
	cron := func(ns, name, role, schedule, desc string) *batchv1.CronJob {
		return &batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name), CreationTimestamp: ago(30 * 24 * time.Hour),
				Labels: map[string]string{"clusterdeck.io/role": role}, Annotations: map[string]string{kube.AnnDescription: desc}},
			Spec: batchv1.CronJobSpec{Schedule: schedule, JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "b", Image: "busybox"}}}}}}},
		}
	}
	job := func(cj *batchv1.CronJob, name string, age time.Duration, cond batchv1.JobConditionType) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cj.Namespace, CreationTimestamp: ago(age),
			OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: cj.Name, UID: cj.UID}}}}
		fin := ago(age - time.Minute)
		j.Status.StartTime = func() *metav1.Time { t := ago(age); return &t }()
		j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, LastTransitionTime: fin}}
		if cond == batchv1.JobComplete {
			j.Status.CompletionTime = &fin
		}
		return j
	}

	dbBackup := cron("shop", "db-backup", "backup", "17 * * * *", "Hourly pg_dump to the NAS")
	files := cron("shop", "storage-backup", "backup", "30 2 * * *", "Nightly tar of uploads")
	check := cron("shop", "db-restore-check", "restore-check", "43 5 1 * *", "Monthly restore into a scratch database")
	photos := cron("photos", "db-backup", "backup", "5 * * * *", "Hourly pg_dump")

	tmpl := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-db", Namespace: "shop", Labels: map[string]string{"clusterdeck.io/role": "restore-template"},
			Annotations: map[string]string{kube.AnnApp: "shop", kube.AnnParams: "DUMP", kube.AnnDescription: "Wipes the shop database and restores a dump. Empty DUMP means the newest."}},
		Data: map[string]string{kube.TemplateKey: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  generateName: shop-restore-\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n        - name: r\n          image: postgres:16\n          env:\n            - {name: DUMP, value: \"\"}\n"},
	}

	cs := fake.NewSimpleClientset(
		node("server-1", "control-plane", true), node("server-2", "control-plane", true), node("agent-1", "worker", false),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "photos"}},
		dep("shop", "web", "shop", 2, 2), dep("shop", "worker", "shop", 1, 1), dep("photos", "server", "photos", 0, 1),
		pod("shop", "web-7d9f-abcde", "web", "server-1", true, 0), pod("shop", "web-7d9f-fghij", "web", "server-2", true, 0),
		pod("shop", "worker-5c4-klmno", "worker", "server-1", true, 2), pod("photos", "server-6b8-pqrst", "server", "agent-1", false, 14),
		dbBackup, files, check, photos,
		job(dbBackup, "db-backup-1", 3*time.Hour, batchv1.JobComplete), job(dbBackup, "db-backup-2", 2*time.Hour, batchv1.JobComplete), job(dbBackup, "db-backup-3", 43*time.Minute, batchv1.JobComplete),
		job(files, "storage-backup-1", 20*time.Hour, batchv1.JobComplete),
		job(check, "db-restore-check-1", 23*24*time.Hour, batchv1.JobComplete),
		job(photos, "photos-backup-1", 5*time.Hour, batchv1.JobComplete), job(photos, "photos-backup-2", 4*time.Hour, batchv1.JobFailed),
		tmpl,
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "photos"}, Type: corev1.EventTypeWarning, Reason: "BackOff",
			Message: "Back-off restarting failed container server", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "server-6b8-pqrst"}, LastTimestamp: ago(2 * time.Minute), Count: 14},
	)

	cs.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if obj, ok := a.(k8stesting.CreateAction).GetObject().(metav1.Object); ok && obj.GetName() == "" && obj.GetGenerateName() != "" {
			obj.SetName(obj.GetGenerateName() + strconv.FormatInt(time.Now().UnixNano()%100000, 36))
		}
		return false, nil, nil
	})

	gvrs := map[schema.GroupVersionResource]string{
		{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}:      "NodeMetricsList",
		{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}: "ApplicationList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrs)
	for name, use := range map[string][2]string{"server-1": {"2300m", "6Gi"}, "server-2": {"900m", "4Gi"}} {
		dyn.Tracker().Create(schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"},
			&unstructured.Unstructured{Object: map[string]any{"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
				"metadata": map[string]any{"name": name}, "usage": map[string]any{"cpu": use[0], "memory": use[1]}}}, "")
	}
	for ns, st := range map[string][2]string{"shop": {"Synced", "Healthy"}, "photos": {"OutOfSync", "Degraded"}} {
		dyn.Tracker().Create(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"},
			&unstructured.Unstructured{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
				"metadata": map[string]any{"name": ns, "namespace": "argocd"},
				"spec":     map[string]any{"destination": map[string]any{"namespace": ns}},
				"status":   map[string]any{"sync": map[string]any{"status": st[0], "revision": "3f2a9c1"}, "health": map[string]any{"status": st[1]}}}}, "argocd")
	}

	cluster, err := kube.New(cs, dyn, kube.Options{
		BackupSelector: "clusterdeck.io/role=backup", RestoreCheckSelector: "clusterdeck.io/role=restore-check",
		RestoreTemplateSelector: "clusterdeck.io/role=restore-template",
	})
	if err != nil {
		log.Fatal(err)
	}
	h := server.New(cluster, wave{}, server.Options{
		ClusterName: "demo-cluster", AuthHeader: "X-Forwarded-Email", ActionsEnabled: true,
		Static: web.FS(), Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	addr := ":8099"
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		addr = v
	}
	log.Printf("demo on %s", addr)
	log.Fatal(http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Forwarded-Email", "demo@example.com")
		h.ServeHTTP(w, r)
	})))
}
