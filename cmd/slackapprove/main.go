// Command slackapprove is the bootstrap for the Slack approval endpoint.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/events"
	"github.com/vamsiatluri/releasebot-migration/internal/runtime"
	"github.com/vamsiatluri/releasebot-migration/internal/slackapprove"
	"github.com/vamsiatluri/releasebot-migration/internal/slackverify"
)

func main() {
	envID, _ := strconv.Atoi(os.Getenv("GITHUB_ENVIRONMENT_ID"))
	cfg := slackapprove.Config{
		SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		GitHubToken:   os.Getenv("GITHUB_APPROVAL_TOKEN"),
		GitHubAPI:     envOr("RELEASEBOT_GITHUB_API_URL", "https://api.github.com"),
		Owner:         os.Getenv("GITHUB_OWNER"),
		Repo:          os.Getenv("GITHUB_REPO"),
		EnvironmentID: envID,
		Approvers:     split(os.Getenv("SLACK_APPROVERS")),
	}

	runtime.Start(func(ctx context.Context, inv runtime.Invocation, p []byte) ([]byte, error) {
		var req events.APIGatewayProxyRequest
		if err := json.Unmarshal(p, &req); err != nil {
			return nil, fmt.Errorf("not an API Gateway event: %w", err)
		}
		res := cfg.Handle(ctx, []byte(req.Body),
			req.Header(slackverify.HeaderSignature),
			req.Header(slackverify.HeaderTimestamp),
			time.Now())

		// ⚠️ A Block Kit `block_actions` interaction does NOT take its reply
		// from this HTTP response. That is legacy attachment-message
		// behaviour; Slack reads the response only for its status code and
		// discards the body. Returning the message here looks right, returns
		// 200, and makes the button do nothing visible at all -- which is
		// exactly how it behaved until someone clicked it and asked why.
		//
		// The update goes to the interaction's response_url instead.
		if res.Message != "" && res.ResponseURL != "" {
			if err := tellSlack(ctx, res.ResponseURL, res.Message); err != nil {
				// Never fatal. The approval itself has already happened or
				// already failed; losing the confirmation must not turn a
				// successful approval into a retry the operator thinks is
				// needed.
				log.Printf("AUDIT {\"event\":\"reply.failed\",\"detail\":%q}", err.Error())
			}
		}
		return json.Marshal(events.APIGatewayProxyResponse{
			StatusCode: res.Status,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       res.Body,
		})
	})
}

// tellSlack posts the outcome back into the channel the button was clicked in.
//
// The URL arrives inside a payload we have already signature-verified, so it
// genuinely came from Slack -- but it is still an URL handed to us by an
// external system and then fetched by something holding a GitHub token, so the
// host is checked rather than trusted. That is the difference between an
// authenticated input and a safe one.
func tellSlack(ctx context.Context, responseURL, text string) error {
	u, err := url.Parse(responseURL)
	if err != nil {
		return fmt.Errorf("unparseable response_url: %w", err)
	}
	if u.Scheme != "https" || u.Host != "hooks.slack.com" {
		return fmt.Errorf("refusing to post to %s://%s", u.Scheme, u.Host)
	}

	b, _ := json.Marshal(map[string]any{
		"replace_original": true,
		"text":             text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func split(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
