// Command slackapprove is the bootstrap for the Slack approval endpoint.
package main

import (
	"context"
	"encoding/json"
	"fmt"
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

		// Slack replaces the original message with whatever we return, which is
		// how the button stops being clickable once it has been used.
		body := res.Body
		if res.Message != "" {
			b, _ := json.Marshal(map[string]any{
				"replace_original": true,
				"text":             res.Message,
			})
			body = string(b)
		}
		return json.Marshal(events.APIGatewayProxyResponse{
			StatusCode: res.Status,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       body,
		})
	})
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
