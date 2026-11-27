package kube

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var Levels = []string{"debug", "info", "warn", "error"}

const (
	logByteLimit = 8 << 20
	scanLines    = 20000
)

type LogQuery struct {
	Container string
	Tail      int64
	Since     time.Time
	Until     time.Time
	Levels    []string
	Contains  string
	Previous  bool
}

type LogLine struct {
	Time  *time.Time `json:"time,omitempty"`
	Level string     `json:"level"`
	Text  string     `json:"text"`
}

type Logs struct {
	Lines     []LogLine      `json:"lines"`
	Scanned   int            `json:"scanned"`
	Matched   int            `json:"matched"`
	Counts    map[string]int `json:"counts"`
	Truncated bool           `json:"truncated"`
}

func (q LogQuery) filtered() bool {
	return len(q.Levels) > 0 || q.Contains != "" || !q.Until.IsZero()
}

func (c *Client) PodLogs(ctx context.Context, ns, pod string, q LogQuery) (*Logs, error) {
	for _, l := range q.Levels {
		if !ValidLevel(l) {
			return nil, fmt.Errorf("%w: level must be one of %s or unknown", ErrInvalid, strings.Join(Levels, ", "))
		}
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Until.After(q.Since) {
		return nil, fmt.Errorf("%w: until must be after since", ErrInvalid)
	}
	opts := &corev1.PodLogOptions{Container: q.Container, Timestamps: true, Previous: q.Previous, LimitBytes: ptr(int64(logByteLimit))}
	if !q.Since.IsZero() {
		opts.SinceTime = &metav1.Time{Time: q.Since}
	}
	switch {
	case !q.Until.IsZero():
	case q.filtered():
		opts.TailLines = ptr(int64(scanLines))
	default:
		opts.TailLines = ptr(q.Tail)
	}
	raw, err := c.Core.CoreV1().Pods(ns).GetLogs(pod, opts).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("logs %s/%s: %w", ns, pod, err)
	}
	out := FilterLogs(raw, q)
	out.Truncated = out.Truncated || len(raw) >= logByteLimit
	return out, nil
}

func FilterLogs(raw []byte, q LogQuery) *Logs {
	want := map[string]bool{}
	for _, l := range q.Levels {
		want[l] = true
	}
	needle := strings.ToLower(q.Contains)
	out := &Logs{Lines: []LogLine{}, Counts: map[string]int{}}
	prev := ""
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		ts, text := splitTimestamp(line)
		if ts != nil && !q.Until.IsZero() && !ts.Before(q.Until) {
			break
		}
		level := DetectLevel(text)
		if level == "" && continuation(text) {
			level = prev
		}
		prev = level
		out.Scanned++
		key := level
		if key == "" {
			key = "unknown"
		}
		out.Counts[key]++
		if len(want) > 0 && !want[key] {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(text), needle) {
			continue
		}
		out.Matched++
		out.Lines = append(out.Lines, LogLine{Time: ts, Level: level, Text: text})
	}
	if sc.Err() != nil {
		out.Truncated = true
	}
	if q.Tail > 0 && int64(len(out.Lines)) > q.Tail {
		out.Lines = out.Lines[int64(len(out.Lines))-q.Tail:]
		out.Truncated = true
	}
	return out
}

func ValidLevel(l string) bool {
	if l == "unknown" {
		return true
	}
	for _, v := range Levels {
		if v == l {
			return true
		}
	}
	return false
}

func splitTimestamp(line string) (*time.Time, string) {
	i := strings.IndexByte(line, ' ')
	if i < 20 || i > 40 {
		return nil, line
	}
	t, err := time.Parse(time.RFC3339Nano, line[:i])
	if err != nil {
		return nil, line
	}
	return &t, line[i+1:]
}

func continuation(text string) bool {
	return strings.HasPrefix(text, " ") || strings.HasPrefix(text, "\t") ||
		strings.HasPrefix(text, "Caused by") || strings.HasPrefix(text, "Traceback")
}

var (
	logfmtLevel = regexp.MustCompile(`(?i)(?:^|\s)(?:level|lvl|severity)=["']?([a-z]+)`)
	klogLevel   = regexp.MustCompile(`^([IWEF])\d{4} \d{2}:\d{2}:\d{2}`)
	wordLevel   = regexp.MustCompile(`(?:^|[\s\[(|:])(TRACE|DEBUG|INFO|NOTICE|WARN|WARNING|ERROR|ERR|FATAL|PANIC|CRITICAL|CRIT|SEVERE)(?:$|[\s\]):|,])`)
	jsonKeys    = []string{"level", "severity", "lvl", "levelname", "log.level", "severity_text", "loglevel"}
	klogLevels  = map[string]string{"I": "info", "W": "warn", "E": "error", "F": "error"}
)

func DetectLevel(text string) string {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(t), &m) == nil {
			for _, k := range jsonKeys {
				if l := normalizeLevel(fmt.Sprint(m[k])); l != "" {
					return l
				}
			}
			return ""
		}
	}
	if m := logfmtLevel.FindStringSubmatch(t); m != nil {
		if l := normalizeLevel(m[1]); l != "" {
			return l
		}
	}
	if m := klogLevel.FindStringSubmatch(t); m != nil {
		return klogLevels[m[1]]
	}
	head := t
	if len(head) > 80 {
		head = head[:80]
	}
	if m := wordLevel.FindStringSubmatch(head); m != nil {
		return normalizeLevel(m[1])
	}
	return ""
}

func normalizeLevel(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace", "debug", "dbug", "dbg", "10", "20":
		return "debug"
	case "info", "information", "notice", "inf", "30":
		return "info"
	case "warn", "warning", "wrn", "40":
		return "warn"
	case "error", "err", "eror", "fatal", "panic", "critical", "crit", "severe", "alert", "emergency", "50", "60":
		return "error"
	}
	return ""
}
