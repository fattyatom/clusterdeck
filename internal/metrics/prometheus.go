package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var prometheusPanels = map[string]string{
	"node_cpu":           `sum by (instance) (rate(node_cpu_seconds_total{mode!="idle"}[5m]))`,
	"node_memory":        `sum by (instance) (node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes)`,
	"namespace_cpu":      `topk(20, sum by (namespace) (rate(container_cpu_usage_seconds_total{container!=""}[5m])))`,
	"namespace_memory":   `topk(20, sum by (namespace) (container_memory_working_set_bytes{container!=""}))`,
	"namespace_restarts": `topk(20, sum by (namespace) (kube_pod_container_status_restarts_total))`,
}

type Prometheus struct {
	URL    string
	Client *http.Client
}

func (p *Prometheus) Name() string { return "prometheus" }

func (p *Prometheus) Query(ctx context.Context, panelID string, start, end time.Time, step time.Duration) ([]Series, error) {
	q, ok := prometheusPanels[panelID]
	if !ok {
		return nil, ErrUnknownPanel
	}
	v := url.Values{}
	v.Set("query", q)
	v.Set("start", strconv.FormatInt(start.Unix(), 10))
	v.Set("end", strconv.FormatInt(end.Unix(), 10))
	v.Set("step", strconv.Itoa(int(step.Seconds())))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.URL, "/")+"/api/v1/query_range?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	client := p.Client
	if client == nil {
		client = httpClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return parsePrometheus(raw)
}

func parsePrometheus(raw []byte) ([]Series, error) {
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("prometheus: decode: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", r.Error)
	}
	out := []Series{}
	for _, res := range r.Data.Result {
		keys := make([]string, 0, len(res.Metric))
		for k := range res.Metric {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, res.Metric[k])
		}
		s := Series{Label: strings.Join(parts, " "), Points: []Point{}}
		if s.Label == "" {
			s.Label = "total"
		}
		for _, pair := range res.Values {
			ts, okT := pair[0].(float64)
			str, okV := pair[1].(string)
			if !okT || !okV {
				continue
			}
			val, err := strconv.ParseFloat(str, 64)
			if err != nil {
				continue
			}
			s.Points = append(s.Points, Point{T: int64(ts * 1000), V: val})
		}
		out = append(out, s)
	}
	return sortSeries(out), nil
}
