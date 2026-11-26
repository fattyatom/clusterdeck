package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type NodeInfo struct {
	Name          string   `json:"name"`
	Ready         bool     `json:"ready"`
	Unschedulable bool     `json:"unschedulable"`
	Roles         []string `json:"roles"`
	Version       string   `json:"version"`
	Pressure      []string `json:"pressure"`
	CPUAllocMilli int64    `json:"cpuAllocatableMilli"`
	MemAllocBytes int64    `json:"memAllocatableBytes"`
	CPUUsageMilli *int64   `json:"cpuUsageMilli"`
	MemUsageBytes *int64   `json:"memUsageBytes"`
	PodCount      int      `json:"podCount"`
}

type EventInfo struct {
	Namespace string    `json:"namespace"`
	Object    string    `json:"object"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Count     int32     `json:"count"`
	Time      time.Time `json:"time"`
}

type PodBrief struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Node      string `json:"node"`
	Restarts  int32  `json:"restarts"`
}

type Overview struct {
	Pods       []PodBrief     `json:"pods"`
	Nodes      []NodeInfo     `json:"nodes"`
	Namespaces int            `json:"namespaces"`
	PodPhases  map[string]int `json:"podPhases"`
	Warnings   []EventInfo    `json:"warnings"`
	LiveUsage  bool           `json:"liveUsage"`
}

const maxWarnings = 15

func (c *Client) Overview(ctx context.Context) (*Overview, error) {
	nodes, err := c.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	pods, err := c.Core.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	namespaces, err := c.Core.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	ov := &Overview{Namespaces: len(namespaces.Items), PodPhases: map[string]int{}}
	podsPerNode := map[string]int{}
	for _, p := range pods.Items {
		ov.PodPhases[string(p.Status.Phase)]++
		var restarts int32
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}
		ov.Pods = append(ov.Pods, PodBrief{Namespace: p.Namespace, Name: p.Name, Phase: string(p.Status.Phase), Node: p.Spec.NodeName, Restarts: restarts})
		if p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodPending {
			podsPerNode[p.Spec.NodeName]++
		}
	}

	usage := c.nodeUsage(ctx)
	ov.LiveUsage = usage != nil
	for _, n := range nodes.Items {
		info := NodeInfo{
			Name:          n.Name,
			Unschedulable: n.Spec.Unschedulable,
			Version:       n.Status.NodeInfo.KubeletVersion,
			CPUAllocMilli: n.Status.Allocatable.Cpu().MilliValue(),
			MemAllocBytes: n.Status.Allocatable.Memory().Value(),
			PodCount:      podsPerNode[n.Name],
			Roles:         []string{},
			Pressure:      []string{},
		}
		for k := range n.Labels {
			if role, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok {
				info.Roles = append(info.Roles, role)
			}
		}
		sort.Strings(info.Roles)
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				info.Ready = cond.Status == corev1.ConditionTrue
			} else if cond.Status == corev1.ConditionTrue {
				info.Pressure = append(info.Pressure, string(cond.Type))
			}
		}
		if u, ok := usage[n.Name]; ok {
			cpu, mem := u[0], u[1]
			info.CPUUsageMilli, info.MemUsageBytes = &cpu, &mem
		}
		ov.Nodes = append(ov.Nodes, info)
	}
	sort.Slice(ov.Nodes, func(i, j int) bool { return ov.Nodes[i].Name < ov.Nodes[j].Name })

	ov.Warnings, err = c.warnings(ctx)
	if err != nil {
		return nil, err
	}
	return ov, nil
}

func (c *Client) nodeUsage(ctx context.Context) map[string][2]int64 {
	if c.Dynamic == nil {
		return nil
	}
	list, err := c.Dynamic.Resource(nodeMetricsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	out := map[string][2]int64{}
	for _, item := range list.Items {
		u, _, _ := unstructured.NestedStringMap(item.Object, "usage")
		cpu, errC := resource.ParseQuantity(u["cpu"])
		mem, errM := resource.ParseQuantity(u["memory"])
		if errC == nil && errM == nil {
			out[item.GetName()] = [2]int64{cpu.MilliValue(), mem.Value()}
		}
	}
	return out
}

func (c *Client) warnings(ctx context.Context) ([]EventInfo, error) {
	events, err := c.Core.CoreV1().Events("").List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	out := []EventInfo{}
	for _, e := range events.Items {
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		t := e.LastTimestamp.Time
		if t.IsZero() {
			t = e.EventTime.Time
		}
		if t.IsZero() {
			t = e.CreationTimestamp.Time
		}
		out = append(out, EventInfo{
			Namespace: e.Namespace,
			Object:    strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name,
			Reason:    e.Reason,
			Message:   e.Message,
			Count:     e.Count,
			Time:      t,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > maxWarnings {
		out = out[:maxWarnings]
	}
	return out, nil
}
