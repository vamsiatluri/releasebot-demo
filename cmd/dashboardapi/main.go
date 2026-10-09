// Command dashboardapi answers one question for the management dashboard:
// what is each environment running right now, and do they agree?
//
// It reads AWS directly rather than trusting anything the pipeline wrote down,
// for the same reason the baseline tool reads a running account rather than its
// templates: a dashboard that renders what a deploy step *claimed* will keep
// saying so long after it stops being true.
//
// ⚠️ This endpoint sits behind a Cognito JWT authorizer on the HTTP API. It
// does no authentication of its own, and must not be reachable any other way.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/awssig"
	"github.com/vamsiatluri/releasebot-migration/internal/events"
	"github.com/vamsiatluri/releasebot-migration/internal/runtime"
)

// A pair is one logical function: the production copy and its test twin.
type pair struct {
	Name string  `json:"name"`
	Prod fnState `json:"prod"`
	Test fnState `json:"test"`
	Same bool    `json:"same"`
}

type fnState struct {
	Function string `json:"function"`
	Version  string `json:"version"`
	SHA      string `json:"sha"`
	Error    string `json:"error,omitempty"`
}

type health struct {
	Stage  string `json:"stage"`
	OK     bool   `json:"ok"`
	Env    string `json:"env"`
	Commit string `json:"commit"`
	Built  string `json:"built"`
	Error  string `json:"error,omitempty"`
}

// A job as GitHub reports it, plus where it sits in the release story.
type jobState struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type runState struct {
	Number     int        `json:"number"`
	ID         int64      `json:"id"`
	Commit     string     `json:"commit"`
	Title      string     `json:"title"`
	Actor      string     `json:"actor"`
	Status     string     `json:"status"`
	Conclusion string     `json:"conclusion"`
	URL        string     `json:"url"`
	Jobs       []jobState `json:"jobs"`
	ReadAt     string     `json:"readAt"`
	Stale      bool       `json:"stale,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type payload struct {
	Account string    `json:"account"`
	Region  string    `json:"region"`
	ReadAt  string    `json:"readAt"`
	Pairs   []pair    `json:"pairs"`
	Health  []health  `json:"health"`
	InSync  bool      `json:"inSync"`
	Run     *runState `json:"run,omitempty"`
	Notes   []string  `json:"notes"`
}

func main() {
	runtime.Start(func(ctx context.Context, inv runtime.Invocation, p []byte) ([]byte, error) {
		var req events.APIGatewayProxyRequest
		if err := json.Unmarshal(p, &req); err != nil {
			return nil, fmt.Errorf("not an API Gateway event: %w", err)
		}
		body, _ := json.Marshal(build(ctx))
		return json.Marshal(events.APIGatewayProxyResponse{
			StatusCode: 200,
			Headers: map[string]string{
				"Content-Type": "application/json",
				// The page is served from CloudFront and this API lives on a
				// different origin, so the browser requires this. The allowed
				// origin is configured on the HTTP API itself; echoing "*" here
				// as well would widen it.
				"Cache-Control": "no-store",
			},
			Body: string(body),
		})
	})
}

func build(ctx context.Context) payload {
	region := os.Getenv("AWS_REGION")
	out := payload{
		Account: os.Getenv("RELEASEBOT_ACCOUNT"),
		Region:  region,
		ReadAt:  time.Now().UTC().Format(time.RFC3339),
	}

	names := [][2]string{
		{"cutRelease", "cutReleaseTest"},
		{"releaseAutomationMergeback", "releaseAutomationMergebackTest"},
	}

	var wg sync.WaitGroup
	out.Pairs = make([]pair, len(names))
	for i, n := range names {
		wg.Add(1)
		go func(i int, prodName, testName string) {
			defer wg.Done()
			pr := readFn(ctx, prodName)
			te := readFn(ctx, testName)
			out.Pairs[i] = pair{
				Name: prodName,
				Prod: pr, Test: te,
				// Equal fingerprints mean the same bytes are deployed on both
				// sides. Unequal is NOT a fault -- it is the normal state while
				// a release is held at the gate -- so the UI must say which.
				Same: pr.SHA != "" && pr.SHA == te.SHA,
			}
		}(i, n[0], n[1])
	}

	base := os.Getenv("RELEASEBOT_HEALTH_BASE")
	stages := strings.Split(os.Getenv("RELEASEBOT_STAGES"), ",")
	out.Health = make([]health, 0, len(stages))
	var mu sync.Mutex
	for _, s := range stages {
		s = strings.TrimSpace(s)
		if s == "" || base == "" {
			continue
		}
		wg.Add(1)
		go func(stage string) {
			defer wg.Done()
			h := readHealth(ctx, base, stage)
			mu.Lock()
			out.Health = append(out.Health, h)
			mu.Unlock()
		}(s)
	}
	wg.Wait()

	// The pipeline's own state. Read live from GitHub rather than from anything
	// a deploy step wrote down -- a dashboard that renders what a step CLAIMED
	// keeps saying it long after it stops being true.
	if r := cachedRun(ctx); r != nil {
		out.Run = r
	}

	out.InSync = true
	for _, p := range out.Pairs {
		if !p.Same {
			out.InSync = false
		}
	}
	if !out.InSync {
		out.Notes = append(out.Notes,
			"Test is ahead of production. That is the approval gate holding a release, not drift.")
	}
	return out
}

// readFn asks the Lambda control plane what the `live` alias actually points at.
//
// It signs the call itself (see internal/awssig) rather than shelling out to
// the AWS CLI -- the CLI is NOT present in a provided.al2 deployment package,
// so exec'ing it would compile cleanly, deploy cleanly, and fail only at the
// first real request.
func readFn(ctx context.Context, name string) fnState {
	st := fnState{Function: name}

	creds, err := awssig.FromEnvironment()
	if err != nil {
		st.Error = "no credentials"
		return st
	}
	region := os.Getenv("AWS_REGION")
	endpoint := "https://lambda." + region + ".amazonaws.com" +
		"/2015-03-31/functions/" + url.PathEscape(name) + "/configuration?Qualifier=live"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		st.Error = "bad request"
		return st
	}
	awssig.SignGET(req, creds, "lambda", region, time.Now())

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		st.Error = "unreachable"
		return st
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != 200 {
		// Never echo the body: an AWS error can carry the ARN and the role.
		st.Error = fmt.Sprintf("lambda returned %d", resp.StatusCode)
		return st
	}
	var cfg struct {
		Version    string `json:"Version"`
		CodeSha256 string `json:"CodeSha256"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		st.Error = "unreadable configuration"
		return st
	}
	st.Version, st.SHA = cfg.Version, cfg.CodeSha256
	return st
}

func readHealth(ctx context.Context, base, stage string) health {
	h := health{Stage: stage}
	url := strings.TrimSuffix(base, "/") + "/" + stage + "/beta/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.Error = "bad health URL"
		return h
	}
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		h.Error = "unreachable"
		return h
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var got struct {
		OK     bool   `json:"ok"`
		Env    string `json:"env"`
		Commit string `json:"commit"`
		Built  string `json:"built"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		h.Error = fmt.Sprintf("HTTP %d, unparseable", resp.StatusCode)
		return h
	}
	h.OK, h.Env, h.Commit, h.Built = got.OK, got.Env, got.Commit, got.Built
	return h
}

// ── The pipeline panel, and why it is cached ────────────────────────────────
//
// Every browser tab polls this endpoint. Without a cache, N tabs at a 15s
// refresh is 8N GitHub calls a minute, and GitHub's limit is per CREDENTIAL,
// not per viewer -- so the dashboard would be the thing that exhausts the quota
// the release pipeline itself depends on. One cached read serves every tab.
//
// ⚠️ The repository is public, so this works with NO token at all -- but
// unauthenticated reads are capped at 60/hour PER EGRESS IP, and a Lambda's
// egress IP is shared. So the TTL is deliberately different for the two cases:
// a token buys 5,000/hour and a live-feeling refresh; without one the panel
// stays correct but refreshes slowly, and says so.
var runCache struct {
	mu  sync.Mutex
	at  time.Time
	val *runState
}

func runTTL() time.Duration {
	if os.Getenv("GITHUB_TOKEN") != "" {
		return 10 * time.Second
	}
	return 90 * time.Second
}

// cachedRun returns the pipeline panel, refreshing it at most once per TTL.
//
// On a refresh failure it serves the LAST GOOD value marked stale rather than
// an error. A dashboard that blanks the panel the moment GitHub rate-limits it
// has turned a slow refresh into an apparent outage.
func cachedRun(ctx context.Context) *runState {
	runCache.mu.Lock()
	defer runCache.mu.Unlock()

	if runCache.val != nil && time.Since(runCache.at) < runTTL() {
		return runCache.val
	}
	fresh := readRun(ctx)
	if fresh == nil {
		return runCache.val
	}
	if fresh.Error != "" && runCache.val != nil && runCache.val.Error == "" {
		stale := *runCache.val
		stale.Stale = true
		return &stale
	}
	fresh.ReadAt = time.Now().UTC().Format(time.RFC3339)
	runCache.at, runCache.val = time.Now(), fresh
	return fresh
}

// readRun fetches the most recent run of the release workflow and its jobs.
//
// ⚠️ A failure here must not fail the whole page. The environment state above
// is read from AWS and is still true; losing the pipeline panel is a degraded
// view, not an outage, and the UI says which part is missing.
func readRun(ctx context.Context) *runState {
	owner, repo := os.Getenv("GITHUB_OWNER"), os.Getenv("GITHUB_REPO")
	token, wf := os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_WORKFLOW_FILE")
	if owner == "" || repo == "" || wf == "" {
		return nil
	}

	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://api.github.com/repos/"+owner+"/"+repo+path, nil)
		if err != nil {
			return err
		}
		// Omitted entirely when empty -- an `Authorization: Bearer ` header with
		// no value is a 401, which would read as a bad token rather than as the
		// anonymous read it is meant to be.
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != 200 {
			// Never echo the body: a GitHub error names the repo and the token's
			// owner. Rate limiting is called out by name because "403" on its own
			// sends people looking for a permissions problem that is not there.
			if resp.StatusCode == 403 && resp.Header.Get("X-RateLimit-Remaining") == "0" {
				return fmt.Errorf("github rate limit reached")
			}
			return fmt.Errorf("github returned %d", resp.StatusCode)
		}
		return json.Unmarshal(raw, out)
	}

	var runs struct {
		WorkflowRuns []struct {
			ID           int64  `json:"id"`
			RunNumber    int    `json:"run_number"`
			HeadSHA      string `json:"head_sha"`
			DisplayTitle string `json:"display_title"`
			Status       string `json:"status"`
			Conclusion   string `json:"conclusion"`
			HTMLURL      string `json:"html_url"`
			Actor        struct {
				Login string `json:"login"`
			} `json:"actor"`
		} `json:"workflow_runs"`
	}
	if err := get("/actions/workflows/"+wf+"/runs?per_page=1", &runs); err != nil {
		return &runState{Error: err.Error()}
	}
	if len(runs.WorkflowRuns) == 0 {
		return nil
	}
	r := runs.WorkflowRuns[0]
	st := &runState{
		Number: r.RunNumber, ID: r.ID, Commit: shortSHA(r.HeadSHA),
		Title: r.DisplayTitle, Actor: r.Actor.Login,
		Status: r.Status, Conclusion: r.Conclusion, URL: r.HTMLURL,
	}

	var jobs struct {
		Jobs []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	if err := get(fmt.Sprintf("/actions/runs/%d/jobs", r.ID), &jobs); err == nil {
		for _, j := range jobs.Jobs {
			st.Jobs = append(st.Jobs, jobState{j.Name, j.Status, j.Conclusion})
		}
	}
	return st
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
