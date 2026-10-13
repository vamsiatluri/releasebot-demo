package handler

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vamsiatluri/releasebot-migration/internal/events"
)

// A failed INIT must leave /health ANSWERING and everything else REFUSING.
// Getting this backwards in either direction is the bug worth pinning:
// a /health that 502s cannot report what is wrong, and a /cut that answers
// would be a release path running without credentials.
func TestUnreadyResponse(t *testing.T) {
	initErr := errors.New("config: GITHUB_TOKEN is required")

	for _, path := range []string{"/beta/cut", "/beta/merge", "/beta/slack/interact", ""} {
		if _, ok := UnreadyResponse(events.APIGatewayProxyRequest{Path: path}, initErr); ok {
			t.Fatalf("path %q must NOT be served while INIT has failed", path)
		}
	}

	raw, ok := UnreadyResponse(events.APIGatewayProxyRequest{Path: "/beta/health"}, initErr)
	if !ok {
		t.Fatal("/beta/health must answer while INIT has failed — it is the endpoint that reports it")
	}
	var resp events.APIGatewayProxyResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("response is not a valid proxy response (API Gateway would 502): %v", err)
	}
	if resp.StatusCode != 503 {
		t.Fatalf("want 503 (unready), got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["ready"] != false {
		t.Fatalf("ready must be false, got %v", body["ready"])
	}
	// The missing KEY must be named; a value never may be.
	if !strings.Contains(resp.Body, "GITHUB_TOKEN") {
		t.Fatal("the reason must name the missing key, or the 503 is as useless as the 502")
	}
	// ⚠️ And WHICH environment is unready. This response is hand-built rather
	// than produced by the normal health handler, so it silently lacked the
	// account label -- and an unready environment is exactly when someone is
	// comparing two dashboards trying to tell which one is broken.
	if !strings.Contains(resp.Body, `"account"`) {
		t.Fatal("the unready response must name the account, like the healthy one does")
	}
}
