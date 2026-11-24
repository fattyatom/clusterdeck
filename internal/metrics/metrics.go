package metrics

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"
)

var (
	ErrNotConfigured = errors.New("no metrics backend configured")
	ErrUnknownPanel  = errors.New("unknown panel")
)

type Panel struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Unit  string `json:"unit"`
}

var Panels = []Panel{
	{ID: "node_cpu", Title: "CPU by node", Unit: "cores"},
	{ID: "node_memory", Title: "Memory by node", Unit: "bytes"},
	{ID: "namespace_cpu", Title: "CPU by namespace", Unit: "cores"},
	{ID: "namespace_memory", Title: "Memory by namespace", Unit: "bytes"},
	{ID: "namespace_restarts", Title: "Container restarts by namespace", Unit: "count"},
}

type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

type Series struct {
	Label  string  `json:"label"`
	Points []Point `json:"points"`
}

type Provider interface {
	Name() string
	Query(ctx context.Context, panelID string, start, end time.Time, step time.Duration) ([]Series, error)
}

type None struct{}

func (None) Name() string { return "none" }
func (None) Query(context.Context, string, time.Time, time.Time, time.Duration) ([]Series, error) {
	return nil, ErrNotConfigured
}

func StepFor(window time.Duration) time.Duration {
	step := window / 120
	if step < time.Minute {
		return time.Minute
	}
	return step.Truncate(time.Minute)
}

func sortSeries(s []Series) []Series {
	sort.Slice(s, func(i, j int) bool { return s[i].Label < s[j].Label })
	for i := range s {
		sort.Slice(s[i].Points, func(a, b int) bool { return s[i].Points[a].T < s[i].Points[b].T })
	}
	return s
}

func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }
