// Package slackapprove lets a reviewer approve a pending production deployment
// from a button in Slack.
//
// ⚠️ READ THIS BEFORE ENABLING IT — it is a deliberate weakening of the gate.
//
// GitHub's required-reviewer rule says "only these named people may approve,
// and they must be signed in to GitHub to do it". A Slack button says "anyone
// who can click in this channel can trigger an approval, which GitHub will then
// record as whoever owns the token this service holds".
//
// Those are not the same control. The second one:
//
//   - moves the identity check from GitHub to this service
//   - delegates it, by default, to Slack channel membership
//   - attributes every approval to one token-holder in GitHub's audit trail,
//     so the log stops telling you WHO decided
//
// So this handler does three things to claw that back, and none of them are
// optional:
//
//  1. Verifies the Slack signature on every request. The endpoint is public;
//     without this, anyone who learns the URL can approve a production deploy.
//  2. Checks the CLICKING Slack user against an explicit allowlist. Channel
//     membership is not authorisation.
//  3. Records who clicked, in the message it posts back, so the Slack thread
//     carries the attribution that GitHub's log no longer can.
//
// Even with all three, the honest summary for a security reviewer is: the gate
// is now enforced by this Lambda rather than by GitHub. That may be a fine trade
// for convenience. It should be a decision somebody makes, not a side effect of
// wanting a button.
package slackapprove

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// audit records every attempt against this endpoint.
//
// An endpoint that can promote to production needs a log of who tried, whether
// they were permitted, and what happened -- and it needs it MORE than an
// ordinary endpoint, not less, precisely because it moves the identity check off
// GitHub. Without it, a refusal and a success look identical from outside: the
// first version of this shipped with no logging at all, and the only way to tell
// a rejected click from an unreached endpoint was that the deployment stayed
// pending.
//
// ⛔ Never log the signature, the body, or the token. The user id and the
// outcome are the whole useful record.
func audit(event, user, runID, detail string) {
	b, _ := json.Marshal(map[string]string{
		"event": event, "slack_user": user, "run_id": runID, "detail": detail,
	})
	log.Printf("AUDIT %s", b)
}

type Config struct {
	SigningSecret string
	GitHubToken   string
	GitHubAPI     string
	Owner         string
	Repo          string
	EnvironmentID int
	// Approvers are Slack user IDs permitted to approve. Empty means NOBODY --
	// failing closed, because an empty allowlist read as "allow all" would turn
	// a misconfiguration into an open door on a public endpoint.
	Approvers []string
}

// Interaction is the slice of Slack's block_actions payload we need.
type Interaction struct {
	Type string `json:"type"`
	User struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user"`
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

type Result struct {
	Status  int
	Body    string
	Message string // what to post back to Slack
}

// Handle processes one Slack interaction. rawBody must be the exact bytes Slack
// sent -- the signature covers those, not a re-encoding.
func (c Config) Handle(ctx context.Context, rawBody []byte, signature, timestamp string, now time.Time) Result {
	if err := verify(c.SigningSecret, signature, timestamp, rawBody, now); err != nil {
		audit("rejected.signature", "", "", err.Error())
		// 401 and nothing else. Saying WHY is a free oracle for someone probing
		// a public endpoint that can deploy to production.
		return Result{Status: 401, Body: "unauthorized"}
	}

	form, err := url.ParseQuery(string(rawBody))
	if err != nil {
		return Result{Status: 400, Body: "bad payload"}
	}
	var in Interaction
	if err := json.Unmarshal([]byte(form.Get("payload")), &in); err != nil {
		return Result{Status: 400, Body: "unparseable interaction"}
	}
	if len(in.Actions) == 0 {
		return Result{Status: 200, Body: ""}
	}

	audit("click", in.User.ID, in.Actions[0].Value, "name="+in.User.Name)

	if !c.permitted(in.User.ID) {
		audit("rejected.not_approver", in.User.ID, in.Actions[0].Value,
			fmt.Sprintf("allowlist has %d entries", len(c.Approvers)))
		// The refusal names the RAW id, not just a mention. A Slack user id
		// differs per workspace, so the allowlist cannot be filled in from
		// anywhere else reliably -- echoing it makes the first refused click
		// the thing that tells an operator what to configure, instead of a
		// lookup they have to go and perform. It reveals nothing a member of
		// the workspace cannot already see.
		return Result{Status: 200, Body: "",
			Message: fmt.Sprintf(":no_entry: <@%s> (`%s`) is not on the approver list for "+
				"production. Approval is restricted regardless of who can post in this channel.\n"+
				"_To permit this person, add `%s` to SLACK_APPROVERS._",
				in.User.ID, in.User.ID, in.User.ID)}
	}

	runID := strings.TrimSpace(in.Actions[0].Value)
	if runID == "" {
		return Result{Status: 200, Body: "", Message: ":warning: that button carried no run id."}
	}

	if err := c.approve(ctx, runID, in.User.Name); err != nil {
		audit("approve.failed", in.User.ID, runID, err.Error())
		return Result{Status: 200, Body: "",
			Message: ":x: Could not approve run `" + runID + "`: " + err.Error()}
	}
	audit("approved", in.User.ID, runID, "production")
	return Result{Status: 200, Body: "",
		Message: fmt.Sprintf(":white_check_mark: Approved by <@%s>. Run `%s` is deploying to production now.",
			in.User.ID, runID)}
}

func (c Config) permitted(userID string) bool {
	for _, a := range c.Approvers {
		if strings.EqualFold(strings.TrimSpace(a), userID) {
			return true
		}
	}
	return false
}

// approve calls GitHub's pending-deployments API.
func (c Config) approve(ctx context.Context, runID, who string) error {
	body, _ := json.Marshal(map[string]any{
		"environment_ids": []int{c.EnvironmentID},
		"state":           "approved",
		"comment":         "Approved from Slack by " + who,
	})
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/pending_deployments",
		strings.TrimRight(c.GitHubAPI, "/"), c.Owner, c.Repo, runID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.GitHubToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
