package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
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
)

var now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func defaultOpts() Options {
	return Options{
		HideNamespaces:          []string{"kube-system"},
		BackupSelector:          "clusterdeck.io/role=backup",
		RestoreCheckSelector:    "clusterdeck.io/role=restore-check",
		RestoreTemplateSelector: "clusterdeck.io/role=restore-template",
	}
}

func newClient(t *testing.T, dyn *dynamicfake.FakeDynamicClient, objs ...runtime.Object) (*Client, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	var c *Client
	var err error
	if dyn == nil {
		c, err = New(cs, nil, defaultOpts())
	} else {
		c, err = New(cs, dyn, defaultOpts())
	}
	if err != nil {
		t.Fatal(err)
	}
	c.Now = func() time.Time { return now }
	return c, cs
}

func TestNewRejectsEmptySelectors(t *testing.T) {
	opts := defaultOpts()
	opts.BackupSelector = ""
	if _, err := New(fake.NewSimpleClientset(), nil, opts); err == nil {
		t.Fatal("an empty backup selector would match every CronJob; want an error")
	}
	opts = defaultOpts()
	opts.RestoreTemplateSelector = ""
	if _, err := New(fake.NewSimpleClientset(), nil, opts); err == nil {
		t.Fatal("an empty restore selector would match every ConfigMap; want an error")
	}
}

func node(name string, ready bool) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: status},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
			},
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.1+k3s1"},
		},
	}
}

func TestOverview(t *testing.T) {
	scheme := runtime.NewScheme()
	nm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
		"metadata": map[string]any{"name": "n1"},
		"usage":    map[string]any{"cpu": "1500m", "memory": "2Gi"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		nodeMetricsGVR: "NodeMetricsList",
		argoAppGVR:     "ApplicationList",
	})
	if err := dyn.Tracker().Create(nodeMetricsGVR, nm, ""); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "a"}, Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e", Namespace: "a"},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackOff",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "p"},
		LastTimestamp:  metav1.NewTime(now),
	}
	normal := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "a"}, Type: corev1.EventTypeNormal}
	c, _ := newClient(t, dyn, node("n1", true), node("n2", false), pod, ev, normal,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "a"}})

	ov, err := c.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Nodes) != 2 || !ov.Nodes[0].Ready || ov.Nodes[1].Ready {
		t.Fatalf("nodes = %+v", ov.Nodes)
	}
	n1 := ov.Nodes[0]
	if n1.CPUUsageMilli == nil || *n1.CPUUsageMilli != 1500 || *n1.MemUsageBytes != 2<<30 {
		t.Fatalf("live usage not read: %+v", n1)
	}
	if n1.CPUAllocMilli != 4000 || n1.PodCount != 1 || n1.Roles[0] != "control-plane" || n1.Pressure[0] != "MemoryPressure" {
		t.Fatalf("node fields: %+v", n1)
	}
	if ov.Nodes[1].CPUUsageMilli != nil {
		t.Fatal("a node without metrics must report nil usage, not zero")
	}
	if ov.PodPhases["Running"] != 1 || ov.Namespaces != 1 || !ov.LiveUsage {
		t.Fatalf("overview = %+v", ov)
	}
	if len(ov.Warnings) != 1 || ov.Warnings[0].Object != "pod/p" {
		t.Fatalf("warnings = %+v", ov.Warnings)
	}
}

func deployment(ns, name, app string, ready, desired int32) *appsv1.Deployment {
	sel := map[string]string{"pod": name}
	tmplLabels := map[string]string{"pod": name}
	if app != "" {
		tmplLabels[AppLabel] = app
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &desired,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: tmplLabels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img:" + name}}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func TestAppsGroupsWorkloadsAndPods(t *testing.T) {
	scheme := runtime.NewScheme()
	argo := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "shop", "namespace": "argocd"},
		"spec":     map[string]any{"destination": map[string]any{"namespace": "shop"}},
		"status": map[string]any{
			"sync":   map[string]any{"status": "Synced", "revision": "abc"},
			"health": map[string]any{"status": "Healthy"},
		},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		nodeMetricsGVR: "NodeMetricsList",
		argoAppGVR:     "ApplicationList",
	})
	if err := dyn.Tracker().Create(argoAppGVR, argo, "argocd"); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop", Labels: map[string]string{"pod": "web"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{RestartCount: 3}},
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	c, _ := newClient(t, dyn,
		deployment("shop", "web", "shop", 1, 1),
		deployment("shop", "worker", "shop", 0, 1),
		deployment("shop", "cache", "", 1, 1),
		deployment("kube-system", "coredns", "coredns", 1, 1),
		pod,
	)
	apps, err := c.Apps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Fatalf("want shop + cache (kube-system hidden), got %+v", apps)
	}
	cache, shop := apps[0], apps[1]
	if cache.Name != "cache" || !cache.Healthy {
		t.Fatalf("unlabelled workload should be its own healthy app: %+v", cache)
	}
	if shop.Name != "shop" || len(shop.Workloads) != 2 || shop.Healthy {
		t.Fatalf("shop should group web+worker and be unhealthy: %+v", shop)
	}
	if shop.Restarts != 3 || len(shop.Workloads[0].Pods) != 1 || shop.Workloads[0].Pods[0].Name != "web-1" {
		t.Fatalf("pod not attributed by selector: %+v", shop.Workloads)
	}
	if len(shop.Argo) != 1 || shop.Argo[0].Sync != "Synced" || shop.Argo[0].Revision != "abc" {
		t.Fatalf("argo = %+v", shop.Argo)
	}
}

func TestHealth(t *testing.T) {
	sched, err := parseSchedule("0 * * * *", "")
	if err != nil {
		t.Fatal(err)
	}
	created := now.Add(-48 * time.Hour)
	at := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	cases := []struct {
		name      string
		suspended bool
		created   time.Time
		last      *time.Time
		recent    []JobRun
		want      string
	}{
		{"suspended wins", true, created, nil, nil, HealthSuspended},
		{"running", false, created, at(time.Hour), []JobRun{{State: "running"}}, HealthRunning},
		{"latest failed", false, created, at(time.Hour), []JobRun{{State: "failed"}, {State: "succeeded"}}, HealthFailed},
		{"fresh", false, created, at(30 * time.Minute), []JobRun{{State: "succeeded"}}, HealthOK},
		{"one missed run is still ok", false, created, at(90 * time.Minute), nil, HealthOK},
		{"two missed runs is stale", false, created, at(150 * time.Minute), nil, HealthStale},
		{"never ran", false, created, nil, nil, HealthNever},
		{"brand new", false, now.Add(-10 * time.Minute), nil, nil, HealthPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := health(sched, tc.suspended, tc.created, tc.last, tc.recent, now); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func cronJob(ns, name string, labels map[string]string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name), Labels: labels, CreationTimestamp: metav1.NewTime(now.Add(-72 * time.Hour)), Annotations: map[string]string{AnnDescription: "hourly dump"}},
		Spec: batchv1.CronJobSpec{
			Schedule: "17 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "x"}},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "backup", Image: "busybox"}},
				}}},
			},
		},
	}
}

func ownedJob(cj *batchv1.CronJob, name string, created time.Time, cond batchv1.JobConditionType) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: cj.Namespace, CreationTimestamp: metav1.NewTime(created),
		OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: cj.Name, UID: cj.UID}},
	}}
	if cond != "" {
		fin := metav1.NewTime(created.Add(time.Minute))
		j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, LastTransitionTime: fin}}
		if cond == batchv1.JobComplete {
			j.Status.CompletionTime = &fin
		}
	}
	return j
}

func TestScheduledJobs(t *testing.T) {
	backup := cronJob("shop", "db-backup", map[string]string{"clusterdeck.io/role": "backup", AppLabel: "shop"})
	check := cronJob("shop", "db-restore-check", map[string]string{"clusterdeck.io/role": "restore-check"})
	other := cronJob("shop", "cleanup", nil)
	c, _ := newClient(t, nil, backup, check, other,
		ownedJob(backup, "b-old", now.Add(-2*time.Hour), batchv1.JobComplete),
		ownedJob(backup, "b-new", now.Add(-40*time.Minute), batchv1.JobComplete),
		ownedJob(check, "c-1", now.Add(-time.Hour), batchv1.JobFailed),
	)
	jobs, err := c.ScheduledJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("unlabelled cronjob must be ignored, got %+v", jobs)
	}
	b := jobs[0]
	if b.Name != "db-backup" || b.Role != RoleBackup || b.App != "shop" || b.Health != HealthOK || b.Description != "hourly dump" {
		t.Fatalf("backup = %+v", b)
	}
	if len(b.Recent) != 2 || b.Recent[0].Name != "b-new" || b.LastSuccess == nil || !b.LastSuccess.Equal(now.Add(-39*time.Minute)) {
		t.Fatalf("recent/lastSuccess wrong: %+v", b)
	}
	if b.NextRun == nil || !b.NextRun.Equal(time.Date(2026, 11, 24, 12, 17, 0, 0, time.UTC)) {
		t.Fatalf("next run = %v", b.NextRun)
	}
	if jobs[1].Role != RoleRestoreCheck || jobs[1].Health != HealthFailed {
		t.Fatalf("restore check = %+v", jobs[1])
	}
}

func TestRunCronJob(t *testing.T) {
	backup := cronJob("shop", "db-backup", map[string]string{"clusterdeck.io/role": "backup"})
	other := cronJob("shop", "cleanup", nil)
	c, cs := newClient(t, nil, backup, other)

	if _, err := c.RunCronJob(context.Background(), "shop", "cleanup", "me@example.com"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an unlabelled cronjob must not be runnable, err = %v", err)
	}
	if _, err := c.RunCronJob(context.Background(), "shop", "db-backup", "me@example.com"); err != nil {
		t.Fatal(err)
	}
	jobs, _ := cs.BatchV1().Jobs("shop").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("want one job, got %d", len(jobs.Items))
	}
	j := jobs.Items[0]
	if j.GenerateName != "db-backup-manual-" || j.Annotations["cronjob.kubernetes.io/instantiate"] != "manual" || j.Annotations[AnnTriggeredBy] != "me@example.com" {
		t.Fatalf("job meta = %+v", j.ObjectMeta)
	}
	if len(j.OwnerReferences) != 1 || j.OwnerReferences[0].UID != backup.UID || j.Labels["app"] != "x" {
		t.Fatalf("job must be owned by the cronjob and keep its labels: %+v", j.ObjectMeta)
	}
	if j.Spec.Template.Spec.Containers[0].Image != "busybox" {
		t.Fatal("job spec not copied from the cronjob")
	}
}

const restoreJob = `apiVersion: batch/v1
kind: Job
metadata:
  generateName: shop-restore-
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: restore
          image: postgres:16
          env:
            - name: DUMP
              value: ""
            - name: PGHOST
              value: db
`

func restoreTemplate(ns, name, job string, labelled bool) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: map[string]string{AnnParams: "DUMP", AnnApp: "shop", AnnDescription: "wipes the db"}},
		Data:       map[string]string{TemplateKey: job},
	}
	if labelled {
		cm.Labels = map[string]string{"clusterdeck.io/role": "restore-template"}
	}
	return cm
}

func TestRestoreTemplates(t *testing.T) {
	bad := strings.Replace(restoreJob, "restartPolicy: Never", "hostNetwork: true\n      restartPolicy: Never", 1)
	c, _ := newClient(t, nil,
		restoreTemplate("shop", "restore-db", restoreJob, true),
		restoreTemplate("shop", "evil", bad, true),
		restoreTemplate("shop", "unrelated", restoreJob, false),
	)
	ts, err := c.RestoreTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("want 2 labelled templates, got %+v", ts)
	}
	if ts[0].Name != "evil" || ts[0].Error == "" {
		t.Fatalf("a hostNetwork template must be listed with an error: %+v", ts[0])
	}
	if ts[1].Params[0].Name != "DUMP" || ts[1].App != "shop" || ts[1].Error != "" {
		t.Fatalf("template = %+v", ts[1])
	}
}

func TestRunRestore(t *testing.T) {
	ctx := context.Background()
	privileged := strings.Replace(restoreJob, "image: postgres:16", "image: postgres:16\n          securityContext:\n            privileged: true", 1)
	otherNS := strings.Replace(restoreJob, "generateName: shop-restore-", "generateName: x-\n  namespace: kube-system", 1)
	c, cs := newClient(t, nil,
		restoreTemplate("shop", "restore-db", restoreJob, true),
		restoreTemplate("shop", "priv", privileged, true),
		restoreTemplate("shop", "escape", otherNS, true),
		restoreTemplate("shop", "plain", restoreJob, false),
	)
	cases := []struct {
		name, tmpl, confirm string
		params              map[string]string
		want                error
	}{
		{"wrong confirm", "restore-db", "shop/other", nil, ErrInvalid},
		{"undeclared param", "restore-db", "shop/restore-db", map[string]string{"PGHOST": "evil"}, ErrInvalid},
		{"shell metacharacters", "restore-db", "shop/restore-db", map[string]string{"DUMP": "x; rm -rf /"}, ErrInvalid},
		{"path traversal", "restore-db", "shop/restore-db", map[string]string{"DUMP": "../../etc/passwd"}, ErrInvalid},
		{"privileged", "priv", "shop/priv", nil, ErrForbidden},
		{"other namespace", "escape", "shop/escape", nil, ErrForbidden},
		{"unlabelled configmap", "plain", "shop/plain", nil, ErrForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.RunRestore(ctx, "shop", tc.tmpl, tc.params, tc.confirm, "me"); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if jobs, _ := cs.BatchV1().Jobs("").List(ctx, metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Fatalf("refused restores must create nothing, got %d jobs", len(jobs.Items))
	}

	if _, err := c.RunRestore(ctx, "shop", "restore-db", map[string]string{"DUMP": "shop-20261124T0017Z.dump"}, "shop/restore-db", "me"); err != nil {
		t.Fatal(err)
	}
	jobs, _ := cs.BatchV1().Jobs("shop").List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs.Items))
	}
	j := jobs.Items[0]
	env := j.Spec.Template.Spec.Containers[0].Env
	if env[0].Name != "DUMP" || env[0].Value != "shop-20261124T0017Z.dump" || env[1].Value != "db" || len(env) != 2 {
		t.Fatalf("env = %+v", env)
	}
	if j.GenerateName != "shop-restore-" || j.Annotations[AnnTriggeredBy] != "me" || j.Annotations["clusterdeck.io/template"] != "restore-db" {
		t.Fatalf("job meta = %+v", j.ObjectMeta)
	}
}

func TestRestartWorkload(t *testing.T) {
	c, cs := newClient(t, nil, deployment("shop", "web", "shop", 1, 1))
	ctx := context.Background()
	if err := c.RestartWorkload(ctx, "shop", "deployment", "web"); err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments("shop").Get(ctx, "web", metav1.GetOptions{})
	if got := d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]; got != "2026-11-24T12:00:00Z" {
		t.Fatalf("restartedAt = %q", got)
	}
	if err := c.RestartWorkload(ctx, "shop", "pod", "web"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestOverviewPropagatesAPIErrors(t *testing.T) {
	c, cs := newClient(t, nil)
	cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	if _, err := c.Overview(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}
