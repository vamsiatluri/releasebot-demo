package slackapprove

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/slackverify"
)

const secret = "test-signing-secret"

func payload(userID string) []byte {
	v := url.Values{}
	v.Set("payload", `{"type":"block_actions","user":{"id":"`+userID+`","name":"someone"},
		"actions":[{"action_id":"approve","value":"123456"}]}`)
	return []byte(v.Encode())
}

func signed(t *testing.T, c Config, body []byte, userID string) Result {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := slackverify.Sign(secret, ts, body)
	return c.Handle(context.Background(), body, sig, ts, time.Now())
}

// An EMPTY approver list must permit nobody. Read as "allow all", a
// misconfiguration would become an open door on a public endpoint that can
// deploy to production.
func TestEmptyApproverListFailsClosed(t *testing.T) {
	c := Config{SigningSecret: secret, Approvers: nil}
	r := signed(t, c, payload("U123"), "U123")
	if r.Message == "" || r.Message[:10] == ":white_che" {
		t.Fatalf("empty allowlist approved something: %q", r.Message)
	}
}

func TestOnlyListedUsersMayApprove(t *testing.T) {
	c := Config{SigningSecret: secret, Approvers: []string{"UALLOWED"}}
	if c.permitted("UOTHER") {
		t.Fatal("an unlisted user was permitted")
	}
	if !c.permitted("UALLOWED") {
		t.Fatal("a listed user was refused")
	}
	// Slack ids are case-stable, but a config pasted with different casing
	// should not silently lock the owner out of their own gate.
	if !c.permitted("uallowed") {
		t.Fatal("case difference refused a listed user")
	}
}

func TestForgedSignatureIsRejectedWithoutExplanation(t *testing.T) {
	c := Config{SigningSecret: secret, Approvers: []string{"U123"}}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r := c.Handle(context.Background(), payload("U123"), "v0="+
		"0000000000000000000000000000000000000000000000000000000000000000", ts, time.Now())
	if r.Status != 401 {
		t.Fatalf("expected 401, got %d", r.Status)
	}
	if r.Body != "unauthorized" {
		t.Fatalf("the rejection explained itself: %q", r.Body)
	}
}

func TestReplayedClickIsRejected(t *testing.T) {
	c := Config{SigningSecret: secret, Approvers: []string{"U123"}}
	body := payload("U123")
	ts := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	sig := slackverify.Sign(secret, ts, body) // genuinely signed, just old
	if r := c.Handle(context.Background(), body, sig, ts, time.Now()); r.Status != 401 {
		t.Fatalf("a 10-minute-old click was accepted: %d", r.Status)
	}
}
