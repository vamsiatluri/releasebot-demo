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

type payload struct {
	Account string   `json:"account"`
	Region  string   `json:"region"`
	ReadAt  string   `json:"readAt"`
	Pairs   []pair   `json:"pairs"`
	Health  []health `json:"health"`
	InSync  bool     `json:"inSync"`
	Notes   []string `json:"notes"`
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
