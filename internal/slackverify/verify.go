// Package slackverify implements Slack's request signing scheme.
//
// Slack signs every request to a slash-command or interactivity URL:
//
//	basestring = "v0:" + X-Slack-Request-Timestamp + ":" + rawBody
//	X-Slack-Signature = "v0=" + hex(HMAC_SHA256(signingSecret, basestring))
//
// Three ways this is commonly got wrong, all of which pass a happy-path test:
//
//  1. Comparing with == instead of hmac.Equal. A byte-by-byte compare leaks the
//     prefix length through timing and is a forgeable signature given enough
//     attempts.
//  2. Verifying a re-serialised body. The HMAC covers the EXACT bytes Slack
//     sent. Parse the form AFTER verifying, never before -- url.Values.Encode()
//     reorders and re-escapes and the signature will never match.
//  3. No timestamp window. Without one, a captured request is replayable
//     forever. Slack's guidance is five minutes.
package slackverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderSignature = "X-Slack-Signature"
	HeaderTimestamp = "X-Slack-Request-Timestamp"
	version         = "v0"
	// ReplayWindow matches Slack's documented recommendation.
	ReplayWindow = 5 * time.Minute
)

var (
	ErrMissing   = errors.New("slackverify: signature or timestamp header missing")
	ErrStale     = errors.New("slackverify: timestamp outside replay window")
	ErrSignature = errors.New("slackverify: signature mismatch")
	// ⚠️ A CONFIGURATION error, deliberately distinct from ErrSignature. The
	// two are indistinguishable from the outside -- both end in a 401 -- and
	// that is precisely why they must be distinguishable in the LOG.
	ErrUnresolvedReference = errors.New("slackverify: the signing secret is still an unresolved reference — the resolver did not run, so this build cannot verify anything")
)

// Verify checks a Slack request. body must be the raw, undecoded request body.
func Verify(secret, signature, timestamp string, body []byte, now time.Time) error {
	if secret == "" {
		return errors.New("slackverify: signing secret not configured")
	}
	if signature == "" || timestamp == "" {
		return ErrMissing
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("slackverify: bad timestamp %q: %w", timestamp, err)
	}
	// Absolute value: a clock skewed either way is equally a problem, and only
	// checking the past lets a future-dated capture replay indefinitely.
	if d := now.Sub(time.Unix(ts, 0)); d > ReplayWindow || d < -ReplayWindow {
		return ErrStale
	}

	// ⛔ A SECRET THAT IS STILL A REFERENCE MEANS THE RESOLVER NEVER RAN.
	//
	// This exact failure cost an hour: a function was deployed with an OLD
	// binary that predated internal/secretref, while its environment had
	// already been switched to `ssm:/releasebot/prod/secrets/...`. The old code
	// read that PATH as the signing secret and HMAC'd 57 characters of
	// parameter path against every request. Every real Slack click returned
	// 401 "unauthorized", which is indistinguishable from a wrong secret, a
	// rotated secret, or a forged request.
	//
	// ⚠️ A value of this shape can NEVER be a valid signing secret, so saying
	// so costs nothing and converts a silent authentication failure into a
	// named configuration error. Checked here rather than at INIT on purpose:
	// this is the function that cares, and a future caller that forgets to
	// resolve is caught wherever it forgot.
	if strings.HasPrefix(secret, "ssm:") || strings.HasPrefix(secret, "secretsmanager:") {
		return ErrUnresolvedReference
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(version + ":" + timestamp + ":"))
	mac.Write(body)
	want := version + "=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(want), []byte(signature)) {
		return ErrSignature
	}
	return nil
}

// Sign produces a valid signature. Used by the local harness and the smoke
// tests so they exercise the real verification path rather than bypassing it.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(version + ":" + timestamp + ":"))
	mac.Write(body)
	return version + "=" + hex.EncodeToString(mac.Sum(nil))
}
