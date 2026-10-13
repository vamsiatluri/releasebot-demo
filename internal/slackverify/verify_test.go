package slackverify

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

const secret = "8f742231b10e8888abcd99yyyzzz85a5"

func TestVerifyAcceptsAGenuineSignature(t *testing.T) {
	now := time.Unix(1700000000, 0)
	body := []byte("token=x&command=%2Fcut&text=news-app+5.4.0")
	ts := "1700000000"
	if err := Verify(secret, Sign(secret, ts, body), ts, body, now); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestVerifyRejectsATamperedBody(t *testing.T) {
	now := time.Unix(1700000000, 0)
	ts := "1700000000"
	sig := Sign(secret, ts, []byte("text=news-app+5.4.0"))
	// Same signature, different payload: the attacker swaps the repo.
	err := Verify(secret, sig, ts, []byte("text=payments-core+5.4.0"), now)
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("expected ErrSignature, got %v", err)
	}
}

func TestVerifyRejectsAReplayOutsideTheWindow(t *testing.T) {
	body := []byte("command=%2Fcut")
	ts := "1700000000"
	sig := Sign(secret, ts, body)
	// Captured request replayed six minutes later. Signature is still valid.
	later := time.Unix(1700000000, 0).Add(6 * time.Minute)
	if err := Verify(secret, sig, ts, body, later); !errors.Is(err, ErrStale) {
		t.Fatalf("expected ErrStale, got %v", err)
	}
}

func TestVerifyRejectsAFutureDatedTimestamp(t *testing.T) {
	body := []byte("command=%2Fcut")
	ts := "1700000600" // ten minutes ahead of "now"
	sig := Sign(secret, ts, body)
	now := time.Unix(1700000000, 0)
	if err := Verify(secret, sig, ts, body, now); !errors.Is(err, ErrStale) {
		t.Fatalf("expected ErrStale for a future timestamp, got %v", err)
	}
}

func TestVerifyRejectsAMissingSignature(t *testing.T) {
	if err := Verify(secret, "", "1700000000", nil, time.Unix(1700000000, 0)); !errors.Is(err, ErrMissing) {
		t.Fatalf("expected ErrMissing, got %v", err)
	}
}

// The migration rotates the signing secret at cutover. Proving the old secret
// stops working is the point of that step.
func TestVerifyRejectsTheOldSecretAfterRotation(t *testing.T) {
	now := time.Unix(1700000000, 0)
	body := []byte("command=%2Fcut")
	ts := "1700000000"
	sigFromOldSecret := Sign("old-dev-account-secret", ts, body)
	if err := Verify(secret, sigFromOldSecret, ts, body, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("expected ErrSignature, got %v", err)
	}
}

// ⛔ The hour-long failure this prevents: a function deployed with a binary
// that predates the secret resolver, while its environment already holds
// `ssm:/path`. The old code HMAC'd 57 characters of parameter path and every
// real Slack request came back 401 — indistinguishable from a wrong secret, a
// rotated secret, or a forgery.
func TestAnUnresolvedReferenceIsNotASignatureFailure(t *testing.T) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte("payload=%7B%7D")

	for _, ref := range []string{
		"ssm:/releasebot/prod/secrets/slack-signing-secret",
		"secretsmanager:releasebot/prod/slack",
	} {
		// Sign with the reference itself — the worst case, because the
		// signature is then internally CONSISTENT and would verify.
		sig := Sign(ref, ts, body)
		err := Verify(ref, sig, ts, body, time.Now())
		if err != ErrUnresolvedReference {
			t.Fatalf("a %q secret must be a named configuration error, got %v", ref, err)
		}
	}

	// ⚠️ And a real secret must be unaffected — this guard must not become a
	// reason a working deployment stops verifying.
	real := "abcdef0123456789abcdef0123456789"
	if err := Verify(real, Sign(real, ts, body), ts, body, time.Now()); err != nil {
		t.Fatalf("a genuine secret must still verify: %v", err)
	}
}
