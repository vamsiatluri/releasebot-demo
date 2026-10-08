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

// A Result that carries a Message must also carry somewhere to put it.
//
// This is the regression guard for the defect that made the button appear
// dead: Slack discards the body of the HTTP response to a `block_actions`
// interaction, so a Message without a ResponseURL is composed, returned,
// acknowledged with 200 -- and never seen by anybody.
func TestEveryMessageCarriesAResponseURL(t *testing.T) {
	const responseURL = "https://hooks.slack.com/actions/T1/B2/abc"

	cases := map[string]string{
		"not an approver": `{"type":"block_actions","user":{"id":"UNOPE","name":"nope"},
			"response_url":"` + responseURL + `","actions":[{"action_id":"approve","value":"123"}]}`,
		"button with no run id": `{"type":"block_actions","user":{"id":"UOK","name":"ok"},
			"response_url":"` + responseURL + `","actions":[{"action_id":"approve","value":""}]}`,
	}

	cfg := Config{SigningSecret: secret, Approvers: []string{"UOK"}}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			body := "payload=" + url.QueryEscape(payload)
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			res := cfg.Handle(context.Background(), []byte(body),
				slackverify.Sign(secret, ts, []byte(body)), ts, time.Now())

			if res.Message == "" {
				t.Fatalf("expected a message explaining the outcome, got none")
			}
			if res.ResponseURL != responseURL {
				t.Errorf("message would be invisible: ResponseURL = %q, want %q",
					res.ResponseURL, responseURL)
			}
		})
	}
}
