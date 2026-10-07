// Package contract is the ReleaseBot behaviour suite.
//
// ONE definition of "correct", run against THREE targets:
//
//	localhost        -- in a pull request, free, no AWS
//	the test stage   -- after every deploy, in both accounts
//	on a schedule    -- which is what makes it a canary
//
// That matters more than it sounds during an account migration. The deliverable
// of the parallel-run phase is evidence that the production account behaves like
// the dev account. If the two accounts are checked by two different test suites,
// that evidence is worthless. Running the SAME suite against both and diffing
// the results is the proof.
//
// Every case carries a `Why` -- the failure it catches. That field is what turns
// a test report into something a non-engineer can read: "23 passed" says
// nothing, "forged Slack signature rejected" says the system is safe to leave
// on a public URL.
package contract

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

type Severity string

const (
	// Critical: a failure here means do not cut over, or roll back.
	Critical Severity = "critical"
	// Important: a failure degrades the service but does not make it unsafe.
	Important Severity = "important"
)

type Kind string

const (
	SlackSigned Kind = "slack-signed" // correctly signed, should be accepted
	SlackForged Kind = "slack-forged" // bad signature, must be rejected
	SlackStale  Kind = "slack-stale"  // valid signature, old timestamp
	JiraSecret  Kind = "jira-secret"  // correct shared secret
	JiraWrong   Kind = "jira-wrong"   // wrong shared secret
	Plain       Kind = "plain"        // unauthenticated, e.g. health
)

type Case struct {
	Name     string
	Why      string // what this catches, in words a manager can read
	Severity Severity
	Kind     Kind
	Method   string
	Path     string
	Form     url.Values // slack slash command fields
	JSON     string     // jira webhook body
	Mutating bool       // writes to GitHub; only runs against a shadow repo

	WantStatus          int
	WantBodyContains    []string
	WantBodyNotContains []string
}

// Build returns the suite. `repo` is the repository the mutating cases act on;
// `run` is a unique token so repeated runs do not collide on the same version.
func Build(repo, run string) []Case {
	v := func(n int) string { return fmt.Sprintf("0.%s.%d", run, n) }
	slash := func(text string) url.Values {
		return url.Values{
			"token":        {"contract-suite"},
			"user_name":    {"contract-suite"},
			"channel_id":   {"C0RELEASE"},
			"command":      {"/cut"},
			"text":         {text},
			"response_url": {""}, // empty: the suite asserts on the HTTP response, not Slack
		}
	}

	cutV := v(1)
	return []Case{
		{
			Name: "health endpoint answers", Severity: Critical, Kind: Plain,
			Why:    "The function is deployed, initialised, and reachable through API Gateway.",
			Method: "GET", Path: "/beta/health",
			WantStatus: 200, WantBodyContains: []string{`"ok":true`},
		},
		{
			Name: "health reports the expected environment", Severity: Critical, Kind: Plain,
			Why:    "Proves the stage is wired to the right function. A test stage reporting prod config is how the wrong bot ends up live.",
			Method: "GET", Path: "/beta/health",
			WantStatus: 200, WantBodyContains: []string{`"env"`},
		},

		// --- authentication: the only thing between a public URL and `main` ---
		{
			Name: "forged Slack signature is rejected", Severity: Critical, Kind: SlackForged,
			Why:    "The endpoint is public. Without this, anyone who finds the URL can cut a release branch.",
			Method: "POST", Path: "/beta/cut", Form: slash(repo + " 9.9.9"),
			WantStatus: 401, WantBodyNotContains: []string{"Cut ", "release/"},
		},
		{
			Name: "replayed Slack request is rejected", Severity: Critical, Kind: SlackStale,
			Why:    "A captured request stays validly signed forever. Without a timestamp window it can be replayed indefinitely.",
			Method: "POST", Path: "/beta/cut", Form: slash(repo + " 9.9.9"),
			WantStatus: 401,
		},
		{
			Name: "Jira webhook with the wrong secret is rejected", Severity: Critical, Kind: JiraWrong,
			Why:    "The Jira route is public too, and its only control is the shared secret.",
			Method: "POST", Path: "/beta/cut",
			JSON:       jiraBody("TEST-1", "9.9.9"),
			WantStatus: 401,
		},

		// --- input handling ---
		{
			Name: "malformed version is refused, not acted on", Severity: Important, Kind: SlackSigned,
			Why:    `"5.4" would create a branch nobody expects. The bot refuses and says why.`,
			Method: "POST", Path: "/beta/cut", Form: slash(repo + " 5.4"),
			WantStatus: 200, WantBodyContains: []string{"not a valid semantic version"},
		},
		{
			Name: "input errors return 200 with a readable message", Severity: Important, Kind: SlackSigned,
			Why:    "A non-2xx makes Slack show its own generic failure and the user never sees the real error.",
			Method: "POST", Path: "/beta/cut", Form: slash("nonsense"),
			WantStatus: 200, WantBodyContains: []string{"ephemeral"},
		},
		{
			Name: "Jira webhook with no fixVersion is ignored, not failed", Severity: Important, Kind: JiraSecret,
			Why:    "Jira disables a webhook after repeated non-2xx responses. 'Not for me' must be a 200.",
			Method: "POST", Path: "/beta/cut",
			JSON:       `{"webhookEvent":"jira:issue_updated","issue":{"key":"TEST-2","fields":{"project":{"key":"NEWS-APP"},"fixVersions":[]}}}`,
			WantStatus: 200, WantBodyContains: []string{"ignored"},
		},

		// --- behaviour: only against a shadow repository ---
		{
			Name: "cuts a release branch", Severity: Critical, Kind: SlackSigned, Mutating: true,
			Why:    "The primary function of the service.",
			Method: "POST", Path: "/beta/cut", Form: slash(repo + " " + cutV),
			WantStatus: 200, WantBodyContains: []string{"release/" + cutV},
		},
		{
			Name: "cutting the same release twice is safe", Severity: Critical, Kind: SlackSigned, Mutating: true,
			Why:    "Slack and Jira both retry. A retry must not corrupt a release in progress.",
			Method: "POST", Path: "/beta/cut", Form: slash(repo + " " + cutV),
			WantStatus: 200, WantBodyContains: []string{"already existed"},
		},
		{
			Name: "opens the mergeback pull request", Severity: Critical, Kind: SlackSigned, Mutating: true,
			Why:    "The second function of the service.",
			Method: "POST", Path: "/beta/merge", Form: slash(repo + " " + cutV),
			WantStatus: 200, WantBodyContains: []string{"mergeback PR"},
		},
		{
			Name: "re-requesting a mergeback reuses the open PR", Severity: Important, Kind: SlackSigned, Mutating: true,
			Why:    "Avoids a pile of duplicate pull requests on a retry.",
			Method: "POST", Path: "/beta/merge", Form: slash(repo + " " + cutV),
			WantStatus: 200, WantBodyContains: []string{"Reused open"},
		},
		{
			Name: "mergeback for a branch that was never cut fails clearly", Severity: Important, Kind: SlackSigned, Mutating: true,
			Why:    "GitHub's own error for this does not say the branch is missing. The bot must.",
			Method: "POST", Path: "/beta/merge", Form: slash(repo + " " + v(99)),
			WantStatus: 200,
			// Asserts the exact branch and repo, not just "does not exist".
			// The loose version passed against a repo that did not exist at all
			// -- a check that is green for a reason you did not intend is worse
			// than one that is red.
			WantBodyContains: []string{"release/" + v(99), repo},
		},
	}
}

func jiraBody(key, version string) string {
	return `{"webhookEvent":"jira:issue_updated","user":{"displayName":"contract-suite"},` +
		`"issue":{"key":"` + key + `","fields":{"summary":"contract","project":{"key":"NEWS-APP"},` +
		`"fixVersions":[{"name":"` + version + `"}]}}}`
}

// RunToken is a short, run-unique token used to build versions that cannot
// collide with a previous run or with a real release.
func RunToken(t time.Time) string {
	return strings.ReplaceAll(t.UTC().Format("0102-1504"), "-", "")
}
