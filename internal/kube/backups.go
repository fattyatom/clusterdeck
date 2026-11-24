package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	ErrInvalid   = errors.New("invalid request")
	ErrForbidden = errors.New("not allowed")
)

const (
	HealthOK        = "ok"
	HealthRunning   = "running"
	HealthFailed    = "failed"
	HealthStale     = "stale"
	HealthNever     = "never"
	HealthPending   = "pending"
	HealthSuspended = "suspended"
	HealthUnknown   = "unknown"

	recentJobs = 5
)

type JobRun struct {
	Name     string     `json:"name"`
	State    string     `json:"state"`
	Manual   bool       `json:"manual"`
	Started  *time.Time `json:"started"`
	Finished *time.Time `json:"finished"`
}

type ScheduledJob struct {
	Namespace   string     `json:"namespace"`
	Name        string     `json:"name"`
	App         string     `json:"app"`
	Role        string     `json:"role"`
	Description string     `json:"description"`
	Schedule    string     `json:"schedule"`
	TimeZone    string     `json:"timeZone"`
	Suspended   bool       `json:"suspended"`
	LastSuccess *time.Time `json:"lastSuccess"`
	NextRun     *time.Time `json:"nextRun"`
	Health      string     `json:"health"`
	Recent      []JobRun   `json:"recent"`
}

func (c *Client) ScheduledJobs(ctx context.Context) ([]ScheduledJob, error) {
	cjs, err := c.Core.BatchV1().CronJobs("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list cronjobs: %w", err)
	}
	jobsByNS := map[string][]batchv1.Job{}
	out := []ScheduledJob{}
	now := c.Now()
	for _, cj := range cjs.Items {
		role := c.cronRole(cj.Labels)
		if role == "" {
			continue
		}
		jobs, ok := jobsByNS[cj.Namespace]
		if !ok {
			list, err := c.Core.BatchV1().Jobs(cj.Namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("list jobs in %s: %w", cj.Namespace, err)
			}
			jobs = list.Items
			jobsByNS[cj.Namespace] = jobs
		}
		out = append(out, describeCronJob(cj, role, jobs, now))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (c *Client) cronRole(l map[string]string) string {
	for _, role := range []string{RoleBackup, RoleRestoreCheck} {
		if c.cronRoles[role].Matches(labels.Set(l)) {
			return role
		}
	}
	return ""
}

func describeCronJob(cj batchv1.CronJob, role string, jobs []batchv1.Job, now time.Time) ScheduledJob {
	app := cj.Annotations[AnnApp]
	if app == "" {
		app = cj.Labels[AppLabel]
	}
	sj := ScheduledJob{
		Namespace:   cj.Namespace,
		Name:        cj.Name,
		App:         app,
		Role:        role,
		Description: cj.Annotations[AnnDescription],
		Schedule:    cj.Spec.Schedule,
		Suspended:   cj.Spec.Suspend != nil && *cj.Spec.Suspend,
		Recent:      []JobRun{},
	}
	if cj.Spec.TimeZone != nil {
		sj.TimeZone = *cj.Spec.TimeZone
	}
	if cj.Status.LastSuccessfulTime != nil {
		t := cj.Status.LastSuccessfulTime.Time
		sj.LastSuccess = &t
	}

	var owned []batchv1.Job
	for _, j := range jobs {
		for _, ref := range j.OwnerReferences {
			if ref.Kind == "CronJob" && ref.Name == cj.Name && (cj.UID == "" || ref.UID == cj.UID) {
				owned = append(owned, j)
				break
			}
		}
	}
	sort.Slice(owned, func(i, k int) bool { return owned[i].CreationTimestamp.After(owned[k].CreationTimestamp.Time) })
	for i, j := range owned {
		if i == recentJobs {
			break
		}
		sj.Recent = append(sj.Recent, jobRun(j))
	}
	if sj.LastSuccess == nil {
		for _, r := range sj.Recent {
			if r.State == "succeeded" && r.Finished != nil {
				sj.LastSuccess = r.Finished
				break
			}
		}
	}

	sched, err := parseSchedule(sj.Schedule, sj.TimeZone)
	if err != nil {
		sj.Health = HealthUnknown
		return sj
	}
	next := sched.Next(now)
	sj.NextRun = &next
	sj.Health = health(sched, sj.Suspended, cj.CreationTimestamp.Time, sj.LastSuccess, sj.Recent, now)
	return sj
}

func parseSchedule(schedule, tz string) (cron.Schedule, error) {
	if tz != "" {
		schedule = "CRON_TZ=" + tz + " " + schedule
	}
	return cron.ParseStandard(schedule)
}

func health(sched cron.Schedule, suspended bool, created time.Time, lastSuccess *time.Time, recent []JobRun, now time.Time) string {
	if suspended {
		return HealthSuspended
	}
	if len(recent) > 0 {
		switch recent[0].State {
		case "running":
			return HealthRunning
		case "failed":
			return HealthFailed
		}
	}
	if lastSuccess == nil {
		if now.After(sched.Next(sched.Next(created))) {
			return HealthNever
		}
		return HealthPending
	}
	if now.After(sched.Next(sched.Next(*lastSuccess))) {
		return HealthStale
	}
	return HealthOK
}

func jobRun(j batchv1.Job) JobRun {
	r := JobRun{Name: j.Name, State: "pending", Manual: j.Annotations["cronjob.kubernetes.io/instantiate"] == "manual"}
	if j.Status.StartTime != nil {
		t := j.Status.StartTime.Time
		r.Started = &t
	}
	if j.Status.CompletionTime != nil {
		t := j.Status.CompletionTime.Time
		r.Finished = &t
	}
	for _, cond := range j.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			r.State = "succeeded"
			return r
		case batchv1.JobFailed:
			r.State = "failed"
			if r.Finished == nil {
				t := cond.LastTransitionTime.Time
				r.Finished = &t
			}
			return r
		}
	}
	if j.Status.Active > 0 {
		r.State = "running"
	}
	return r
}

func (c *Client) RunCronJob(ctx context.Context, ns, name, user string) (string, error) {
	cj, err := c.Core.BatchV1().CronJobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if c.cronRole(cj.Labels) == "" {
		return "", fmt.Errorf("%w: cronjob %s/%s is not labelled as a backup or restore check", ErrForbidden, ns, name)
	}
	prefix := name
	if len(prefix) > 44 {
		prefix = prefix[:44]
	}
	annotations := copyMap(cj.Spec.JobTemplate.Annotations)
	annotations["cronjob.kubernetes.io/instantiate"] = "manual"
	annotations[AnnTriggeredBy] = user
	controller := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: prefix + "-manual-",
			Namespace:    ns,
			Labels:       copyMap(cj.Spec.JobTemplate.Labels),
			Annotations:  annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "CronJob",
				Name:       cj.Name,
				UID:        cj.UID,
				Controller: &controller,
			}},
		},
		Spec: *cj.Spec.JobTemplate.Spec.DeepCopy(),
	}
	created, err := c.Core.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	if created.Name != "" {
		return created.Name, nil
	}
	return created.GenerateName, nil
}
