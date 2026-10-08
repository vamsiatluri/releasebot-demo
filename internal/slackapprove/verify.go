package slackapprove

import (
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/slackverify"
)

// verify reuses the same signature check the release bot's own endpoints use.
// One implementation, one set of tests -- a second copy written for this
// endpoint is a second chance to get constant-time comparison or the replay
// window wrong.
func verify(secret, signature, timestamp string, body []byte, now time.Time) error {
	return slackverify.Verify(secret, signature, timestamp, body, now)
}
