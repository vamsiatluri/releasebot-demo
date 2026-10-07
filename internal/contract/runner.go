package contract

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/jira"
	"github.com/vamsiatluri/releasebot-migration/internal/slackverify"
)

type Result struct {
	Case     Case
	Pass     bool
	Status   int
	Body     string
	Failures []string
	Duration time.Duration
	Err      error
}

type Runner struct {
	BaseURL       string // e.g. https://abc123.execute-api.us-east-1.amazonaws.com/test
	SigningSecret string
	JiraSecret    string
	HTTP          *http.Client
	// AllowMutating gates the cases that write to GitHub. Off by default: the
	// suite must be safe to point at any stage without thinking about it.
	AllowMutating bool
}

func NewRunner(base, signing, jiraSecret string) *Runner {
	return &Runner{
		BaseURL:       strings.TrimRight(base, "/"),
		SigningSecret: signing,
		JiraSecret:    jiraSecret,
		HTTP:          &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *Runner) Run(ctx context.Context, cases []Case) []Result {
	out := make([]Result, 0, len(cases))
	for _, c := range cases {
		if c.Mutating && !r.AllowMutating {
			continue
		}
		out = append(out, r.one(ctx, c))
	}
	return out
}

func (r *Runner) one(ctx context.Context, c Case) Result {
	start := time.Now()
	res := Result{Case: c}

	req, err := r.build(ctx, c)
	if err != nil {
		res.Err = err
		res.Failures = []string{"could not build request: " + err.Error()}
		res.Duration = time.Since(start)
		return res
	}

	resp, err := r.HTTP.Do(req)
	if err != nil {
		res.Err = err
		res.Failures = []string{"request failed: " + err.Error()}
		res.Duration = time.Since(start)
		return res
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	res.Status = resp.StatusCode
	res.Body = string(body)
	res.Duration = time.Since(start)

	if c.WantStatus != 0 && resp.StatusCode != c.WantStatus {
		res.Failures = append(res.Failures,
			fmt.Sprintf("expected HTTP %d, got %d", c.WantStatus, resp.StatusCode))
	}
	for _, want := range c.WantBodyContains {
		if !strings.Contains(res.Body, want) {
			res.Failures = append(res.Failures, fmt.Sprintf("body did not contain %q", want))
		}
	}
	for _, unwanted := range c.WantBodyNotContains {
		if strings.Contains(res.Body, unwanted) {
			res.Failures = append(res.Failures, fmt.Sprintf("body unexpectedly contained %q", unwanted))
		}
	}
	res.Pass = len(res.Failures) == 0
	return res
}

func (r *Runner) build(ctx context.Context, c Case) (*http.Request, error) {
	u := r.BaseURL + c.Path

	switch c.Kind {
	case Plain:
		return http.NewRequestWithContext(ctx, c.Method, u, nil)

	case JiraSecret, JiraWrong:
		req, err := http.NewRequestWithContext(ctx, c.Method, u, strings.NewReader(c.JSON))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		secret := r.JiraSecret
		if c.Kind == JiraWrong {
			secret = "deliberately-the-wrong-secret"
		}
		req.Header.Set(jira.HeaderSecret, secret)
		return req, nil

	default: // the Slack kinds
		body := encode(c.Form)
		req, err := http.NewRequestWithContext(ctx, c.Method, u, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		ts := time.Now()
		if c.Kind == SlackStale {
			// Signed correctly, but ten minutes old. The signature is genuine;
			// only the replay window rejects it. That distinction is the whole
			// point of the case.
			ts = ts.Add(-10 * time.Minute)
		}
		tsStr := strconv.FormatInt(ts.Unix(), 10)
		req.Header.Set(slackverify.HeaderTimestamp, tsStr)

		sig := slackverify.Sign(r.SigningSecret, tsStr, []byte(body))
		if c.Kind == SlackForged {
			sig = "v0=" + strings.Repeat("0", 64)
		}
		req.Header.Set(slackverify.HeaderSignature, sig)
		return req, nil
	}
}

// encode produces the body ONCE, and the signature is computed over exactly
// these bytes. Re-encoding the form between signing and sending is the classic
// way to ship a signature check that can never pass.
func encode(v url.Values) string { return v.Encode() }
