// Package obs is ReleaseBot's logging and metrics surface.
//
// CloudWatch gets structured JSON on stdout; Datadog gets metrics. Metrics are
// emitted as DD_LAMBDA_ENHANCED / distribution-style log lines rather than a
// synchronous HTTP POST to the Datadog intake, because a blocking metric
// submission inside a Lambda handler adds the vendor's availability to your
// own and burns billed duration on every invoke.
package obs

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var mu sync.Mutex

type Logger struct {
	service   string
	env       string
	requestID string
	fields    map[string]any
}

func New(service, env string) *Logger {
	return &Logger{service: service, env: env, fields: map[string]any{}}
}

func (l *Logger) With(k string, v any) *Logger {
	n := &Logger{service: l.service, env: l.env, requestID: l.requestID,
		fields: make(map[string]any, len(l.fields)+1)}
	for k, v := range l.fields {
		n.fields[k] = v
	}
	n.fields[k] = v
	return n
}

func (l *Logger) WithRequestID(id string) *Logger {
	n := l.With("request_id", id)
	n.requestID = id
	return n
}

func (l *Logger) Info(msg string)  { l.emit("info", msg) }
func (l *Logger) Warn(msg string)  { l.emit("warn", msg) }
func (l *Logger) Error(msg string) { l.emit("error", msg) }

func (l *Logger) emit(level, msg string) {
	rec := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"level":     level,
		"message":   msg,
		"service":   l.service,
		"env":       l.env,
	}
	for k, v := range l.fields {
		rec[k] = Scrub(v)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"level":"error","message":"log marshal failed: %v"}`+"\n", err)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	os.Stdout.Write(append(b, '\n'))
}

// Count emits a Datadog custom metric through the Lambda extension's
// log-based intake format.
func (l *Logger) Count(metric string, value float64, tags ...string) {
	l.metric("count", metric, value, tags)
}

func (l *Logger) Distribution(metric string, value float64, tags ...string) {
	l.metric("distribution", metric, value, tags)
}

func (l *Logger) metric(kind, metric string, value float64, tags []string) {
	tags = append(tags, "service:"+l.service, "env:"+l.env)
	b, _ := json.Marshal(map[string]any{
		"m": metric, "v": value, "e": time.Now().Unix(), "t": tags, "k": kind,
	})
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(os.Stdout, "MONITORING|%s\n", b)
}

// Timed records a duration distribution and returns the elapsed time.
func (l *Logger) Timed(metric string, start time.Time, tags ...string) time.Duration {
	d := time.Since(start)
	l.Distribution(metric, float64(d.Milliseconds()), tags...)
	return d
}

// secretish are the substrings whose values must never reach a log line.
// ReleaseBot holds a GitHub token with branch-write rights; one careless
// log of a request body puts it in CloudWatch, in Datadog, and in anything
// that forwards either -- and the SOW calls that out explicitly.
var secretish = []string{"token", "secret", "password", "authorization", "signature", "api_key", "apikey"}

// Scrub redacts obviously-secret map keys before a value is logged.
func Scrub(v any) any {
	m, ok := v.(map[string]string)
	if !ok {
		return v
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		lk := strings.ToLower(k)
		redact := false
		for _, s := range secretish {
			if strings.Contains(lk, s) {
				redact = true
				break
			}
		}
		if redact {
			out[k] = "[redacted]"
		} else {
			out[k] = val
		}
	}
	return out
}
