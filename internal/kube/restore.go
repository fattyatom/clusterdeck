package kube

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

const TemplateKey = "job.yaml"

var paramValue = regexp.MustCompile(`^[A-Za-z0-9._:@+=-]{0,253}$`)

type Param struct {
	Name    string `json:"name"`
	Default string `json:"default"`
}

type RestoreTemplate struct {
	Namespace   string  `json:"namespace"`
	Name        string  `json:"name"`
	App         string  `json:"app"`
	Description string  `json:"description"`
	Params      []Param `json:"params"`
	Error       string  `json:"error,omitempty"`
}

func (c *Client) RestoreTemplates(ctx context.Context) ([]RestoreTemplate, error) {
	cms, err := c.Core.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{LabelSelector: c.restoreSelector})
	if err != nil {
		return nil, fmt.Errorf("list restore templates: %w", err)
	}
	out := []RestoreTemplate{}
	for _, cm := range cms.Items {
		t := RestoreTemplate{
			Namespace:   cm.Namespace,
			Name:        cm.Name,
			App:         cm.Annotations[AnnApp],
			Description: cm.Annotations[AnnDescription],
			Params:      []Param{},
		}
		job, err := decodeTemplate(&cm)
		if err != nil {
			t.Error = err.Error()
		} else {
			defaults := envDefaults(job)
			for _, name := range declaredParams(&cm) {
				t.Params = append(t.Params, Param{Name: name, Default: defaults[name]})
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (c *Client) RunRestore(ctx context.Context, ns, name string, params map[string]string, confirm, user string) (string, error) {
	if confirm != ns+"/"+name {
		return "", fmt.Errorf("%w: type %s/%s to confirm the restore", ErrInvalid, ns, name)
	}
	cm, err := c.Core.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if !c.isRestoreTemplate(cm) {
		return "", fmt.Errorf("%w: configmap %s/%s is not a restore template", ErrForbidden, ns, name)
	}
	job, err := decodeTemplate(cm)
	if err != nil {
		return "", err
	}
	declared := map[string]bool{}
	for _, p := range declaredParams(cm) {
		declared[p] = true
	}
	for k, v := range params {
		if !declared[k] {
			return "", fmt.Errorf("%w: parameter %q is not declared by the template", ErrInvalid, k)
		}
		if !paramValue.MatchString(v) || strings.Contains(v, "..") {
			return "", fmt.Errorf("%w: parameter %q has characters a restore value may not use", ErrInvalid, k)
		}
	}

	job.Namespace = ns
	if job.Name != "" {
		job.GenerateName, job.Name = job.Name+"-", ""
	}
	if job.GenerateName == "" {
		job.GenerateName = name + "-"
	}
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[AnnTriggeredBy] = user
	job.Annotations["clusterdeck.io/template"] = name
	applyEnv(&job.Spec.Template.Spec, params)

	created, err := c.Core.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	if created.Name != "" {
		return created.Name, nil
	}
	return created.GenerateName, nil
}

func (c *Client) isRestoreTemplate(cm *corev1.ConfigMap) bool {
	return c.restoreSel.Matches(labels.Set(cm.Labels))
}

func declaredParams(cm *corev1.ConfigMap) []string {
	var out []string
	for _, p := range strings.Split(cm.Annotations[AnnParams], ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func decodeTemplate(cm *corev1.ConfigMap) (*batchv1.Job, error) {
	raw, ok := cm.Data[TemplateKey]
	if !ok {
		return nil, fmt.Errorf("%w: template has no %s key", ErrInvalid, TemplateKey)
	}
	job := &batchv1.Job{}
	if err := yaml.UnmarshalStrict([]byte(raw), job); err != nil {
		return nil, fmt.Errorf("%w: %s is not a Job manifest: %v", ErrInvalid, TemplateKey, err)
	}
	if job.Kind != "Job" || (job.APIVersion != "batch/v1" && job.APIVersion != "") {
		return nil, fmt.Errorf("%w: %s must be a batch/v1 Job", ErrInvalid, TemplateKey)
	}
	if job.Namespace != "" && job.Namespace != cm.Namespace {
		return nil, fmt.Errorf("%w: a template may only create a Job in its own namespace", ErrForbidden)
	}
	if err := checkPodSpec(&job.Spec.Template.Spec); err != nil {
		return nil, err
	}
	return job, nil
}

func checkPodSpec(spec *corev1.PodSpec) error {
	if spec.HostNetwork || spec.HostPID || spec.HostIPC {
		return fmt.Errorf("%w: restore templates may not use host namespaces", ErrForbidden)
	}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			return fmt.Errorf("%w: restore templates may not mount hostPath volumes", ErrForbidden)
		}
	}
	for _, ctr := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		sc := ctr.SecurityContext
		if sc != nil && sc.Privileged != nil && *sc.Privileged {
			return fmt.Errorf("%w: restore templates may not run privileged containers", ErrForbidden)
		}
	}
	return nil
}

func envDefaults(job *batchv1.Job) map[string]string {
	out := map[string]string{}
	spec := job.Spec.Template.Spec
	for _, ctr := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		for _, e := range ctr.Env {
			if _, seen := out[e.Name]; !seen && e.ValueFrom == nil {
				out[e.Name] = e.Value
			}
		}
	}
	return out
}

func applyEnv(spec *corev1.PodSpec, params map[string]string) {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	set := func(ctrs []corev1.Container) {
		for i := range ctrs {
			for _, k := range keys {
				replaced := false
				for j := range ctrs[i].Env {
					if ctrs[i].Env[j].Name == k {
						ctrs[i].Env[j] = corev1.EnvVar{Name: k, Value: params[k]}
						replaced = true
					}
				}
				if !replaced {
					ctrs[i].Env = append(ctrs[i].Env, corev1.EnvVar{Name: k, Value: params[k]})
				}
			}
		}
	}
	set(spec.InitContainers)
	set(spec.Containers)
}
