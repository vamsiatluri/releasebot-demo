// Package jira validates inbound Jira webhooks.
//
// Jira Cloud does not sign webhooks the way Slack does. The two controls that
// actually exist are a shared secret carried in the webhook URL or a custom
// header, and an allowlist of Atlassian's published egress ranges. ReleaseBot
// uses the shared secret; it is compared in constant time for the same reason
// the Slack signature is, and it is read from a header rather than the query
// string so it does not land in API Gateway access logs.
package jira

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
)

const HeaderSecret = "X-Releasebot-Jira-Secret"

var (
	ErrNotConfigured = errors.New("jira: webhook secret not configured")
	ErrUnauthorized  = errors.New("jira: webhook secret mismatch")
)

func VerifySecret(configured, presented string) error {
	if configured == "" {
		return ErrNotConfigured
	}
	if !hmac.Equal([]byte(configured), []byte(presented)) {
		return ErrUnauthorized
	}
	return nil
}

// Event is the slice of a Jira webhook ReleaseBot reads.
type Event struct {
	WebhookEvent string `json:"webhookEvent"`
	Issue        struct {
		Key    string `json:"key"`
		Fields struct {
			Summary     string `json:"summary"`
			FixVersions []struct {
				Name string `json:"name"`
			} `json:"fixVersions"`
			Project struct {
				Key string `json:"key"`
			} `json:"project"`
		} `json:"fields"`
	} `json:"issue"`
	User struct {
		DisplayName string `json:"displayName"`
	} `json:"user"`
}

func Parse(body []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return e, fmt.Errorf("jira: unparseable webhook body: %w", err)
	}
	if e.WebhookEvent == "" {
		return e, errors.New("jira: body has no webhookEvent field")
	}
	return e, nil
}

// FixVersion returns the first fixVersion, which is what ReleaseBot treats as
// the release version.
func (e Event) FixVersion() string {
	if len(e.Issue.Fields.FixVersions) == 0 {
		return ""
	}
	return e.Issue.Fields.FixVersions[0].Name
}
