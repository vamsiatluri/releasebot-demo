package slackapprove

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/slackverify"
)

// The deploy workflow's card: action_id robbie_deploy_approve / _reject, value
// = workflow run id. GITHUB_REPO is owner/name and GITHUB_ENVIRONMENT a name,
// so the environment id is looked up from the run's pending deployments.
func TestDeployCardContract(t *testing.T) {
	type call struct{ method, path, body string }
	cases := []struct {
		name       string
		actionID   string
		value      string
		user       string
		pending    string
		wantCalls  []string
		wantState  string
		wantEnvID  int
		messageHas string
	}{
		{
			name:     "approve",
			actionID: "robbie_deploy_approve", value: "987654", user: "UAPPROVER",
			pending:    `[{"environment":{"id":11,"name":"staging"},"current_user_can_approve":true},{"environment":{"id":42,"name":"Production"},"current_user_can_approve":true}]`,
			wantCalls:  []string{"GET /repos/acme-demo/kettle/actions/runs/987654/pending_deployments", "POST /repos/acme-demo/kettle/actions/runs/987654/pending_deployments"},
			wantState:  "approved",
			wantEnvID:  42,
			messageHas: "Approved by <@UAPPROVER>",
		},
		{
			name:     "reject",
			actionID: "robbie_deploy_reject", value: "987654", user: "UAPPROVER",
			pending:    `[{"environment":{"id":42,"name":"production"},"current_user_can_approve":true}]`,
			wantCalls:  []string{"GET /repos/acme-demo/kettle/actions/runs/987654/pending_deployments", "POST /repos/acme-demo/kettle/actions/runs/987654/pending_deployments"},
			wantState:  "rejected",
			wantEnvID:  42,
			messageHas: "Rejected by <@UAPPROVER>",
		},
		{
			name:     "nothing pending",
			actionID: "robbie_deploy_approve", value: "987654", user: "UAPPROVER",
			pending:    `[]`,
			wantCalls:  []string{"GET /repos/acme-demo/kettle/actions/runs/987654/pending_deployments"},
			messageHas: "nothing waiting on `production`",
		},
		{
			name:     "token user cannot approve",
			actionID: "robbie_deploy_approve", value: "987654", user: "UAPPROVER",
			pending:    `[{"environment":{"id":42,"name":"production"},"current_user_can_approve":false}]`,
			wantCalls:  []string{"GET /repos/acme-demo/kettle/actions/runs/987654/pending_deployments"},
			messageHas: "not a required reviewer",
		},
		{
			name:     "non-approver never reaches GitHub",
			actionID: "robbie_deploy_approve", value: "987654", user: "USTRANGER",
			messageHas: "not on the approver list",
		},
		{
			name:     "run id must be numeric",
			actionID: "robbie_deploy_approve", value: "../../admin", user: "UAPPROVER",
			messageHas: "no valid workflow run id",
		},
		{
			name:     "someone else's button is ignored",
			actionID: "app_release_approve", value: "987654", user: "USTRANGER",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []call
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				calls = append(calls, call{r.Method, r.URL.Path, string(b)})
				if r.Header.Get("Authorization") != "Bearer approval-token" {
					w.WriteHeader(401)
					return
				}
				if r.Method == "GET" {
					w.Write([]byte(tc.pending))
					return
				}
				w.Write([]byte(`[]`))
			}))
			defer gh.Close()

			cfg := Config{SigningSecret: secret, GitHubToken: "approval-token", GitHubAPI: gh.URL,
				Owner: "acme-demo", Repo: "kettle", EnvironmentName: "production",
				Approvers: []string{"UAPPROVER"}, AccountLabel: "rehearsal"}
			payload := `{"type":"block_actions","user":{"id":"` + tc.user + `","username":"pat"},` +
				`"response_url":"https://hooks.slack.com/actions/T1/B2/x",` +
				`"actions":[{"action_id":"` + tc.actionID + `","value":"` + tc.value + `"}]}`
			body := []byte("payload=" + url.QueryEscape(payload))
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			res := cfg.Handle(context.Background(), body, slackverify.Sign(secret, ts, body), ts, time.Now())

			if res.Status != 200 {
				t.Fatalf("status %d", res.Status)
			}
			if tc.messageHas == "" {
				if res.Message != "" || len(calls) != 0 {
					t.Fatalf("a foreign button did something: %q %v", res.Message, calls)
				}
				return
			}
			if !strings.Contains(res.Message, tc.messageHas) {
				t.Fatalf("message %q, want %q", res.Message, tc.messageHas)
			}
			if res.ResponseURL == "" {
				t.Fatal("message has nowhere to go")
			}
			var got []string
			for _, c := range calls {
				got = append(got, c.method+" "+c.path)
			}
			if strings.Join(got, "|") != strings.Join(tc.wantCalls, "|") {
				t.Fatalf("calls = %v\nwant %v", got, tc.wantCalls)
			}
			if tc.wantState != "" {
				var review struct {
					EnvironmentIDs []int  `json:"environment_ids"`
					State          string `json:"state"`
					Comment        string `json:"comment"`
				}
				json.Unmarshal([]byte(calls[len(calls)-1].body), &review)
				if review.State != tc.wantState || len(review.EnvironmentIDs) != 1 || review.EnvironmentIDs[0] != tc.wantEnvID ||
					!strings.Contains(review.Comment, "pat") {
					t.Fatalf("review = %+v", review)
				}
			}
		})
	}
}
