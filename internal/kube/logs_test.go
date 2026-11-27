package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDetectLevel(t *testing.T) {
	cases := map[string]string{
		`{"level":"warn","msg":"slow"}`:                             "warn",
		`{"severity":"ERROR","message":"boom"}`:                     "error",
		`{"level":30,"msg":"pino info"}`:                            "info",
		`{"levelname":"DEBUG","msg":"x"}`:                           "debug",
		`{"msg":"no level","info":"WARN"}`:                          "",
		`time=2026-11-26T10:00:00Z level=error msg="db down"`:       "error",
		`ts=1 lvl=dbug msg=x`:                                       "debug",
		`I1126 10:00:00.123456       1 controller.go:12] synced`:    "info",
		`E1126 10:00:00.123456       1 controller.go:12] failed`:    "error",
		`2026-11-26 10:00:00 [WARNING] disk at 91%`:                 "warn",
		`[2026-11-26 10:00:00] production.ERROR: Undefined index`:   "",
		`2026/11/26 10:00:00 INFO starting server on :8080`:         "info",
		`FATAL: password authentication failed for user "x"`:        "error",
		`GET /healthz 200 3ms`:                                      "",
		`the ERRORS table was renamed`:                              "",
		`2026-11-26 10:00:00.000 UTC [42] LOG:  checkpoint started`: "",
	}
	for in, want := range cases {
		if got := DetectLevel(in); got != want {
			t.Errorf("DetectLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

const sample = `2026-11-26T10:00:00.000000001Z level=info msg="start"
2026-11-26T10:05:00Z level=error msg="request failed"
2026-11-26T10:05:00.5Z   at handler (app.js:10)
2026-11-26T10:10:00Z GET /healthz 200
2026-11-26T10:15:00Z level=warn msg="slow request timeout"
2026-11-26T10:20:00Z level=error msg="timeout talking to db"
`

func TestFilterLogs(t *testing.T) {
	all := FilterLogs([]byte(sample), LogQuery{Tail: 500})
	if all.Scanned != 6 || all.Matched != 6 || len(all.Lines) != 6 || all.Truncated {
		t.Fatalf("all: %+v", all)
	}
	if all.Lines[0].Time == nil || all.Lines[0].Time.Nanosecond() != 1 || all.Lines[0].Text != `level=info msg="start"` {
		t.Fatalf("timestamp not split: %+v", all.Lines[0])
	}
	if all.Lines[2].Level != "error" || all.Lines[3].Level != "" {
		t.Fatalf("continuation lines should inherit the level: %+v", all.Lines[2:4])
	}
	if all.Counts["error"] != 3 || all.Counts["warn"] != 1 || all.Counts["info"] != 1 || all.Counts["unknown"] != 1 {
		t.Fatalf("counts: %v", all.Counts)
	}

	errs := FilterLogs([]byte(sample), LogQuery{Tail: 500, Levels: []string{"error"}})
	if errs.Matched != 3 || errs.Scanned != 6 || errs.Counts["info"] != 1 {
		t.Fatalf("errors only: %+v", errs)
	}

	unknown := FilterLogs([]byte(sample), LogQuery{Tail: 500, Levels: []string{"unknown"}})
	if len(unknown.Lines) != 1 || unknown.Lines[0].Text != "GET /healthz 200" {
		t.Fatalf("unknown only: %+v", unknown.Lines)
	}

	search := FilterLogs([]byte(sample), LogQuery{Tail: 500, Levels: []string{"warn", "error"}, Contains: "TIMEOUT"})
	if search.Matched != 2 {
		t.Fatalf("search: %+v", search.Lines)
	}

	until := FilterLogs([]byte(sample), LogQuery{Tail: 500, Until: time.Date(2026, 11, 26, 10, 10, 0, 0, time.UTC)})
	if until.Scanned != 3 || until.Lines[len(until.Lines)-1].Level != "error" {
		t.Fatalf("until is exclusive: %+v", until)
	}

	tail := FilterLogs([]byte(sample), LogQuery{Tail: 2})
	if len(tail.Lines) != 2 || tail.Matched != 6 || !tail.Truncated || !strings.Contains(tail.Lines[1].Text, "db") {
		t.Fatalf("tail keeps the newest: %+v", tail)
	}

	plain := FilterLogs([]byte("no timestamp here\n"), LogQuery{Tail: 5})
	if plain.Lines[0].Time != nil || plain.Lines[0].Text != "no timestamp here" {
		t.Fatalf("plain: %+v", plain.Lines)
	}
}

func TestPodLogsValidates(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1"}}
	c, _ := newClient(t, nil, pod)
	if _, err := c.PodLogs(context.Background(), "shop", "web-1", LogQuery{Tail: 10, Levels: []string{"loud"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad level: %v", err)
	}
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	if _, err := c.PodLogs(context.Background(), "shop", "web-1", LogQuery{Tail: 10, Since: at, Until: at}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty window: %v", err)
	}
	out, err := c.PodLogs(context.Background(), "shop", "web-1", LogQuery{Tail: 10, Since: at})
	if err != nil || out.Scanned != 1 || out.Lines[0].Text != "fake logs" {
		t.Fatalf("fake logs: %v %+v", err, out)
	}
}
