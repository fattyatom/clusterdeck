package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const signozFixture = `{"status":"success","data":{"type":"time_series","data":{"results":[{"queryName":"A","aggregations":[{"index":0,"alias":"__result_0","series":[
 {"labels":[{"key":{"name":"k8s.node.name"},"value":"k3s-server-2"}],"values":[{"timestamp":1795543200000,"value":0.355},{"timestamp":1795542000000,"partial":true,"value":0.345}]},
 {"labels":[{"key":{"name":"k8s.node.name"},"value":"k3s-server-1"}],"values":[{"timestamp":1795542000000,"value":0.535}]}
]}]}]}}}`

func TestSigNozQuery(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/query_range" || r.Header.Get("SIGNOZ-API-KEY") != "key" {
			t.Errorf("request %s key=%q", r.URL.Path, r.Header.Get("SIGNOZ-API-KEY"))
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Write([]byte(signozFixture))
	}))
	defer srv.Close()

	s := &SigNoz{URL: srv.URL + "/", APIKey: "key", Filter: "k8s.cluster.name = 'home'"}
	end := time.UnixMilli(1795545904000)
	series, err := s.Query(context.Background(), "node_cpu", end.Add(-time.Hour), end, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 || series[0].Label != "k3s-server-1" || series[1].Points[0].T != 1795542000000 {
		t.Fatalf("series = %+v", series)
	}

	if body["requestType"] != "time_series" || body["end"].(float64) != 1795545904000 {
		t.Fatalf("body = %v", body)
	}
	spec := body["compositeQuery"].(map[string]any)["queries"].([]any)[0].(map[string]any)["spec"].(map[string]any)
	agg := spec["aggregations"].([]any)[0].(map[string]any)
	if agg["metricName"] != "k8s.node.cpu.usage" || spec["stepInterval"].(float64) != 120 || spec["signal"] != "metrics" {
		t.Fatalf("spec = %v", spec)
	}
	if spec["filter"].(map[string]any)["expression"] != "k8s.cluster.name = 'home'" {
		t.Fatalf("filter = %v", spec["filter"])
	}
}

func TestSigNozErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"status":"error"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	s := &SigNoz{URL: srv.URL, APIKey: "bad"}
	if _, err := s.Query(context.Background(), "node_cpu", time.Now(), time.Now(), time.Minute); err == nil {
		t.Fatal("want an error on 401")
	}
	if _, err := s.Query(context.Background(), "nope", time.Now(), time.Now(), time.Minute); !errors.Is(err, ErrUnknownPanel) {
		t.Fatalf("err = %v", err)
	}
	if _, err := parseSigNoz([]byte(`{"status":"error"}`)); err == nil {
		t.Fatal("a non-success status must not read as empty data")
	}
}

func TestPrometheusQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v1/query_range" || q.Get("step") != "60" || q.Get("query") != prometheusPanels["namespace_memory"] {
			t.Errorf("request %s %v", r.URL.Path, q)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"namespace":"shop"},"values":[[1700000060,"2048"],[1700000000,"1024"]]},
			{"metric":{},"values":[[1700000000,"NaN"]]}]}}`))
	}))
	defer srv.Close()
	p := &Prometheus{URL: srv.URL}
	series, err := p.Query(context.Background(), "namespace_memory", time.Unix(1700000000, 0), time.Unix(1700003600, 0), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 || series[0].Label != "shop" || series[0].Points[0].V != 1024 || series[0].Points[1].T != 1700000060000 {
		t.Fatalf("series = %+v", series)
	}
	if series[1].Label != "total" {
		t.Fatalf("unlabelled series = %+v", series[1])
	}
}

func TestPanelsHaveQueriesForEveryProvider(t *testing.T) {
	for _, p := range Panels {
		if _, ok := signozPanels[p.ID]; !ok {
			t.Errorf("panel %s has no SigNoz query", p.ID)
		}
		if _, ok := prometheusPanels[p.ID]; !ok {
			t.Errorf("panel %s has no PromQL query", p.ID)
		}
	}
}

func TestStepFor(t *testing.T) {
	if got := StepFor(time.Hour); got != time.Minute {
		t.Fatalf("1h step = %v", got)
	}
	if got := StepFor(7 * 24 * time.Hour); got != 84*time.Minute {
		t.Fatalf("7d step = %v", got)
	}
}
