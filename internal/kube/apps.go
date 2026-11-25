package kube

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

type PodInfo struct {
	Name       string   `json:"name"`
	Phase      string   `json:"phase"`
	Ready      bool     `json:"ready"`
	Restarts   int32    `json:"restarts"`
	Node       string   `json:"node"`
	Containers []string `json:"containers"`
}

type Workload struct {
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Ready   int32     `json:"ready"`
	Desired int32     `json:"desired"`
	Images  []string  `json:"images"`
	Pods    []PodInfo `json:"pods"`
}

type ArgoStatus struct {
	Application string `json:"application"`
	Sync        string `json:"sync"`
	Health      string `json:"health"`
	Revision    string `json:"revision"`
}

type App struct {
	Name      string       `json:"name"`
	Namespace string       `json:"namespace"`
	PartOf    string       `json:"partOf,omitempty"`
	Healthy   bool         `json:"healthy"`
	Restarts  int32        `json:"restarts"`
	Workloads []Workload   `json:"workloads"`
	Argo      []ArgoStatus `json:"argo"`
}

type workloadSource struct {
	kind     string
	meta     metav1.ObjectMeta
	template corev1.PodTemplateSpec
	selector *metav1.LabelSelector
	ready    int32
	desired  int32
}

func replicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

func (c *Client) Apps(ctx context.Context) ([]App, error) {
	deps, err := c.Core.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	sts, err := c.Core.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list statefulsets: %w", err)
	}
	dss, err := c.Core.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list daemonsets: %w", err)
	}
	pods, err := c.Core.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	var sources []workloadSource
	for _, d := range deps.Items {
		sources = append(sources, workloadSource{"Deployment", d.ObjectMeta, d.Spec.Template, d.Spec.Selector, d.Status.ReadyReplicas, replicas(d.Spec.Replicas)})
	}
	for _, s := range sts.Items {
		sources = append(sources, workloadSource{"StatefulSet", s.ObjectMeta, s.Spec.Template, s.Spec.Selector, s.Status.ReadyReplicas, replicas(s.Spec.Replicas)})
	}
	for _, d := range dss.Items {
		sources = append(sources, workloadSource{"DaemonSet", d.ObjectMeta, d.Spec.Template, d.Spec.Selector, d.Status.NumberReady, d.Status.DesiredNumberScheduled})
	}

	podsByNS := map[string][]corev1.Pod{}
	for _, p := range pods.Items {
		podsByNS[p.Namespace] = append(podsByNS[p.Namespace], p)
	}
	argo := c.argoByNamespace(ctx)

	apps := map[types.NamespacedName]*App{}
	for _, src := range sources {
		if !c.visible(src.meta.Namespace) {
			continue
		}
		name := src.template.Labels[AppLabel]
		if name == "" {
			name = src.meta.Labels[AppLabel]
		}
		if name == "" {
			name = src.meta.Name
		}
		key := types.NamespacedName{Namespace: src.meta.Namespace, Name: name}
		app, ok := apps[key]
		if !ok {
			app = &App{Name: name, Namespace: src.meta.Namespace, Healthy: true, Argo: argo[src.meta.Namespace]}
			if app.Argo == nil {
				app.Argo = []ArgoStatus{}
			}
			apps[key] = app
		}
		if app.PartOf == "" {
			app.PartOf = src.template.Labels[PartOfLabel]
			if app.PartOf == "" {
				app.PartOf = src.meta.Labels[PartOfLabel]
			}
		}
		w := Workload{Kind: src.kind, Name: src.meta.Name, Ready: src.ready, Desired: src.desired, Images: []string{}, Pods: []PodInfo{}}
		for _, ctr := range src.template.Spec.Containers {
			w.Images = append(w.Images, ctr.Image)
		}
		if sel, err := metav1.LabelSelectorAsSelector(src.selector); err == nil && !sel.Empty() {
			for _, p := range podsByNS[src.meta.Namespace] {
				if sel.Matches(labels.Set(p.Labels)) {
					info := podInfo(p)
					app.Restarts += info.Restarts
					w.Pods = append(w.Pods, info)
				}
			}
		}
		if w.Ready < w.Desired {
			app.Healthy = false
		}
		app.Workloads = append(app.Workloads, w)
	}

	out := make([]App, 0, len(apps))
	for _, a := range apps {
		sort.Slice(a.Workloads, func(i, j int) bool { return a.Workloads[i].Name < a.Workloads[j].Name })
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func podInfo(p corev1.Pod) PodInfo {
	info := PodInfo{Name: p.Name, Phase: string(p.Status.Phase), Node: p.Spec.NodeName, Containers: []string{}}
	for _, ctr := range p.Spec.Containers {
		info.Containers = append(info.Containers, ctr.Name)
	}
	for _, cs := range p.Status.ContainerStatuses {
		info.Restarts += cs.RestartCount
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			info.Ready = cond.Status == corev1.ConditionTrue
		}
	}
	return info
}

func (c *Client) argoByNamespace(ctx context.Context) map[string][]ArgoStatus {
	out := map[string][]ArgoStatus{}
	if c.Dynamic == nil {
		return out
	}
	list, err := c.Dynamic.Resource(argoAppGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, item := range list.Items {
		ns, _, _ := unstructured.NestedString(item.Object, "spec", "destination", "namespace")
		if ns == "" {
			continue
		}
		sync, _, _ := unstructured.NestedString(item.Object, "status", "sync", "status")
		health, _, _ := unstructured.NestedString(item.Object, "status", "health", "status")
		rev, _, _ := unstructured.NestedString(item.Object, "status", "sync", "revision")
		out[ns] = append(out[ns], ArgoStatus{Application: item.GetName(), Sync: sync, Health: health, Revision: rev})
	}
	return out
}

func (c *Client) PodLogs(ctx context.Context, ns, pod, container string, tail int64) (string, error) {
	opts := &corev1.PodLogOptions{Container: container, TailLines: &tail, LimitBytes: ptr(int64(512 * 1024))}
	raw, err := c.Core.CoreV1().Pods(ns).GetLogs(pod, opts).DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("logs %s/%s: %w", ns, pod, err)
	}
	return string(raw), nil
}

func (c *Client) RestartWorkload(ctx context.Context, ns, kind, name string) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, c.Now().UTC().Format(time.RFC3339)))
	var err error
	switch kind {
	case "deployment":
		_, err = c.Core.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case "statefulset":
		_, err = c.Core.AppsV1().StatefulSets(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case "daemonset":
		_, err = c.Core.AppsV1().DaemonSets(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	default:
		return fmt.Errorf("%w: cannot restart kind %q", ErrInvalid, kind)
	}
	return err
}

func ptr[T any](v T) *T { return &v }
