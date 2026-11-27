package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
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

	"github.com/fattyatom/clusterdeck/internal/gitops"
	"github.com/fattyatom/clusterdeck/internal/kube"
	"github.com/fattyatom/clusterdeck/internal/metrics"
	"github.com/fattyatom/clusterdeck/internal/server"
	"github.com/fattyatom/clusterdeck/web"
)

type demoGit struct{ n int }

func (g *demoGit) OpenRollback(_ context.Context, req gitops.Request) (*gitops.PullRequest, error) {
	g.n++
	return &gitops.PullRequest{Number: 40 + g.n, URL: fmt.Sprintf("https://github.com/example/infra/pull/%d", 40+g.n), Branch: gitops.BranchFor(req.App, req.FromRevision)}, nil
}

func (g *demoGit) RolledBackSince(context.Context, gitops.Source, string, string) (bool, error) {
	return false, nil
}

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
	revisioned := func(d *appsv1.Deployment, rev int64, ann map[string]string) *appsv1.Deployment {
		d.UID = types.UID(d.Namespace + "-" + d.Name)
		d.Annotations = map[string]string{kube.AnnRevision: strconv.FormatInt(rev, 10)}
		for k, v := range ann {
			d.Annotations[k] = v
		}
		d.Status.UpdatedReplicas = d.Status.ReadyReplicas
		d.Status.AvailableReplicas = d.Status.ReadyReplicas
		d.Status.Replicas = *d.Spec.Replicas
		return d
	}
	replicaSet := func(d *appsv1.Deployment, rev int64, image string, age time.Duration, ready int32) *appsv1.ReplicaSet {
		hash := d.Name + "h" + strconv.FormatInt(rev, 10)
		tmpl := d.Spec.Template.DeepCopy()
		tmpl.Labels["pod-template-hash"] = hash
		tmpl.Spec.Containers[0].Image = image
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: d.Name + "-" + hash, Namespace: d.Namespace, CreationTimestamp: ago(age),
				Labels:          map[string]string{"pod-template-hash": hash},
				Annotations:     map[string]string{kube.AnnRevision: strconv.FormatInt(rev, 10)},
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: d.Name, UID: d.UID}}},
			Spec:   appsv1.ReplicaSetSpec{Template: *tmpl},
			Status: appsv1.ReplicaSetStatus{Replicas: ready, ReadyReplicas: ready},
		}
	}
	pod := func(ns, name, owner, node string, ready bool, restarts int32) *corev1.Pod {
		st := corev1.ConditionTrue
		if !ready {
			st = corev1.ConditionFalse
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"pod": owner, "pod-template-hash": owner + "h" + map[string]string{"web": "3", "worker": "2", "server": "5", "api": "8"}[owner]}},
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

	webDep := revisioned(dep("shop", "web", "shop", 2, 2), 3, nil)
	worker := revisioned(dep("shop", "worker", "shop", 1, 1), 2, nil)
	srv := revisioned(dep("photos", "server", "photos", 0, 1), 5, map[string]string{kube.AnnAutoRollback: "true"})
	srv.Status.UpdatedReplicas = 1
	api := revisioned(dep("blog", "api", "blog", 1, 1), 8, map[string]string{kube.AnnAutoRollback: "true", kube.AnnAutoRollbackFrom: "7", kube.AnnAutoRollbackTo: "8", kube.AnnAutoRollbackAt: now.Add(-40 * time.Minute).UTC().Format(time.RFC3339)})
	badPod := pod("photos", "server-6b8-pqrst", "server", "agent-1", false, 14)
	badPod.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}

	cs := fake.NewSimpleClientset(
		node("server-1", "control-plane", true), node("server-2", "control-plane", true), node("agent-1", "worker", false),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "photos"}},
		webDep, worker, srv, api,
		replicaSet(webDep, 1, "ghcr.io/example/web:1.3.0", 9*24*time.Hour, 0), replicaSet(webDep, 2, "ghcr.io/example/web:1.4.1", 3*24*time.Hour, 0), replicaSet(webDep, 3, "ghcr.io/example/web:1.4.2", 5*time.Hour, 2),
		replicaSet(worker, 1, "ghcr.io/example/worker:1.4.1", 3*24*time.Hour, 0), replicaSet(worker, 2, "ghcr.io/example/worker:1.4.2", 5*time.Hour, 1),
		replicaSet(srv, 4, "ghcr.io/example/server:2.0.0", 6*24*time.Hour, 0), replicaSet(srv, 5, "ghcr.io/example/server:2.1.0", 25*time.Minute, 0),
		replicaSet(api, 6, "ghcr.io/example/api:0.9.0", 2*24*time.Hour, 0), replicaSet(api, 7, "ghcr.io/example/api:1.0.0-rc1", 45*time.Minute, 0), replicaSet(api, 8, "ghcr.io/example/api:0.9.0", 40*time.Minute, 1),
		pod("blog", "api-77f-uvwxy", "api", "server-2", true, 0), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "blog"}},
		pod("shop", "web-7d9f-abcde", "web", "server-1", true, 0), pod("shop", "web-7d9f-fghij", "web", "server-2", true, 0),
		pod("shop", "worker-5c4-klmno", "worker", "server-1", true, 2), badPod,
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
	for ns, st := range map[string][3]string{"shop": {"Synced", "Healthy", "auto"}, "photos": {"Synced", "Degraded", "heal"}} {
		automated := map[string]any{"prune": true}
		if st[2] == "heal" {
			automated["selfHeal"] = true
		}
		dyn.Tracker().Create(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"},
			&unstructured.Unstructured{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
				"metadata": map[string]any{"name": ns, "namespace": "argocd"},
				"spec": map[string]any{"destination": map[string]any{"namespace": ns}, "syncPolicy": map[string]any{"automated": automated},
					"source": map[string]any{"repoURL": "https://github.com/example/infra.git", "path": "apps/" + ns + "/production", "targetRevision": "main"}},
				"status": map[string]any{"sync": map[string]any{"status": st[0], "revision": "3f2a9c1e5b7d"}, "health": map[string]any{"status": st[1]},
					"history": []any{
						map[string]any{"id": int64(11), "revision": "8d0e4b2aa1c3", "deployedAt": now.Add(-3 * 24 * time.Hour).UTC().Format(time.RFC3339)},
						map[string]any{"id": int64(12), "revision": "3f2a9c1e5b7d", "deployedAt": now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)},
					}}}}, "argocd")
	}

	cluster, err := kube.New(cs, dyn, kube.Options{
		BackupSelector: "clusterdeck.io/role=backup", RestoreCheckSelector: "clusterdeck.io/role=restore-check",
		RestoreTemplateSelector: "clusterdeck.io/role=restore-template",
	})
	if err != nil {
		log.Fatal(err)
	}
	cluster.Git = &demoGit{}
	h := server.New(demoLogs{cluster}, wave{}, server.Options{
		ClusterName: "demo-cluster", AuthHeader: "X-Forwarded-Email", ActionsEnabled: true, AutoRollback: true, GitRollback: true,
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

type demoLogs struct{ *kube.Client }

var demoMessages = []string{
	`level=info msg="GET /api/items 200" duration=12ms`,
	`level=debug msg="cache hit" key=items:42`,
	`level=info msg="GET /healthz 200" duration=1ms`,
	`level=warn msg="slow query" duration=1.8s table=orders`,
	`level=error msg="upstream timeout" upstream=payments err="context deadline exceeded"`,
	`    at payments.Client.Charge (client.go:88)`,
	`{"level":"info","msg":"job finished","jobs":3}`,
	`GET /metrics 200`,
}

func (d demoLogs) PodLogs(_ context.Context, _, _ string, q kube.LogQuery) (*kube.Logs, error) {
	end := time.Now().UTC()
	var b strings.Builder
	for i := 2000; i >= 0; i-- {
		at := end.Add(-time.Duration(i) * 7 * time.Second)
		if !q.Since.IsZero() && at.Before(q.Since) {
			continue
		}
		fmt.Fprintf(&b, "%s %s\n", at.Format(time.RFC3339Nano), demoMessages[(i*7+i/3)%len(demoMessages)])
	}
	return kube.FilterLogs([]byte(b.String()), q), nil
}
