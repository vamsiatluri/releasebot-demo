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
