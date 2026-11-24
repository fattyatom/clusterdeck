package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type signozPanel struct {
	metric           string
	timeAggregation  string
	spaceAggregation string
	groupBy          string
}

var signozPanels = map[string]signozPanel{
	"node_cpu":           {"k8s.node.cpu.usage", "avg", "sum", "k8s.node.name"},
	"node_memory":        {"k8s.node.memory.working_set", "avg", "sum", "k8s.node.name"},
	"namespace_cpu":      {"k8s.pod.cpu.usage", "avg", "sum", "k8s.namespace.name"},
	"namespace_memory":   {"k8s.pod.memory.working_set", "avg", "sum", "k8s.namespace.name"},
	"namespace_restarts": {"k8s.container.restarts", "max", "sum", "k8s.namespace.name"},
}

type SigNoz struct {
	URL    string
	APIKey string
	Filter string
	Client *http.Client
}

func (s *SigNoz) Name() string { return "signoz" }

func (s *SigNoz) Query(ctx context.Context, panelID string, start, end time.Time, step time.Duration) ([]Series, error) {
	p, ok := signozPanels[panelID]
	if !ok {
		return nil, ErrUnknownPanel
	}
	spec := map[string]any{
		"name":         "A",
		"signal":       "metrics",
		"stepInterval": int(step.Seconds()),
		"limit":        20,
		"order":        []any{map[string]any{"key": map[string]any{"name": "__result"}, "direction": "desc"}},
		"aggregations": []any{map[string]any{
			"metricName":       p.metric,
			"timeAggregation":  p.timeAggregation,
			"spaceAggregation": p.spaceAggregation,
		}},
		"groupBy": []any{map[string]any{"name": p.groupBy}},
	}
	if s.Filter != "" {
		spec["filter"] = map[string]any{"expression": s.Filter}
	}
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": "v1",
		"start":         start.UnixMilli(),
		"end":           end.UnixMilli(),
		"requestType":   "time_series",
		"compositeQuery": map[string]any{
			"queries": []any{map[string]any{"type": "builder_query", "spec": spec}},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.URL, "/")+"/api/v5/query_range", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("SIGNOZ-API-KEY", s.APIKey)
	client := s.Client
	if client == nil {
		client = httpClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signoz: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("signoz: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return parseSigNoz(raw)
}

type signozResponse struct {
	Status string `json:"status"`
	Data   struct {
		Data struct {
			Results []struct {
				Aggregations []struct {
					Series []struct {
						Labels []struct {
							Key struct {
								Name string `json:"name"`
							} `json:"key"`
							Value any `json:"value"`
						} `json:"labels"`
						Values []struct {
							Timestamp int64   `json:"timestamp"`
							Value     float64 `json:"value"`
						} `json:"values"`
					} `json:"series"`
				} `json:"aggregations"`
			} `json:"results"`
		} `json:"data"`
	} `json:"data"`
}

func parseSigNoz(raw []byte) ([]Series, error) {
	var r signozResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("signoz: decode: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("signoz: status %q: %s", r.Status, truncate(string(raw), 300))
	}
	out := []Series{}
	for _, res := range r.Data.Data.Results {
		for _, agg := range res.Aggregations {
			for _, s := range agg.Series {
				var parts []string
				for _, l := range s.Labels {
					parts = append(parts, fmt.Sprint(l.Value))
				}
				series := Series{Label: strings.Join(parts, " "), Points: []Point{}}
				if series.Label == "" {
					series.Label = "total"
				}
				for _, v := range s.Values {
					series.Points = append(series.Points, Point{T: v.Timestamp, V: v.Value})
				}
				out = append(out, series)
			}
		}
	}
	return sortSeries(out), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
