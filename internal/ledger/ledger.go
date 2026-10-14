// Package ledger records what ReleaseBot actually shipped.
//
// WHY THIS EXISTS: "what went out for TODAY last Thursday, and which tickets
// did it close?" is a question nobody could answer. The facts were spread
// across Slack (edited, archived, unsearchable six weeks later), CloudWatch
// (expires, and querying it is a job rather than a page), and GitHub (true,
// but says nothing about which property a repo belongs to).
//
// One row is written per cut and per mergeback, BY THE FUNCTION THAT DID THE
// WORK, at the moment it did it. That matters: a record written by the actor
// cannot drift from the action. A record assembled afterwards by a pipeline
// step, or by a person, can and does.
//
// ⚠️ A ledger write MUST NEVER fail the release. Cutting a branch and then
// failing because the audit row did not persist would mean the bot reports
// failure for work it completed -- and the operator's next move is to run it
// again. Every error here is logged and swallowed.
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/awssig"
)

const table = "releasebot-ledger"

// Entry is one thing ReleaseBot did.
type Entry struct {
	Env      string   // prod | test
	Action   string   // cut | mergeback
	Repo     string   // msnbc/news-app
	Property string   // "TODAY", "Telemundo", "NBC News", ...
	Version  string   // 5.4.0
	Branch   string   // release/5.4.0
	SHA      string   // the commit the branch was cut at
	Actor    string   // display name, for humans
	ActorID  string   // ⚠️ the IMMUTABLE id — see release.Command.ActorID
	Source   string   // slack | jira | api
	Issues   []string // JIRA keys referenced by the change
	Commits  int      // how many commits this release carries
	URL      string   // the PR, for a mergeback
	DryRun   bool     // the sandbox runs dry; a row that hides that is a lie
	// Which build of ReleaseBot performed this. ⚠️ NOT an Actions run number --
	// see release.Service.BotVersion for why that would be an invented fact.
	BotVersion string
	BotCommit  string
	At         time.Time
}

// issueKey matches a Jira key: two or more uppercase alphanumerics, a hyphen,
// digits. Deliberately strict -- a loose pattern turns "UTF-8" and "COVID-19"
// into ticket references and the release notes stop being trustworthy.
var issueKey = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[0-9]{1,6}\b`)

// Issues extracts the unique Jira keys from text, in a stable order.
func Issues(texts ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range texts {
		for _, m := range issueKey.FindAllString(t, -1) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Properties maps a repository to the brand it belongs to.
//
// It is CONFIGURATION, not code: the map lives at /releasebot/<env>/properties
// as JSON, so adding a property is a Parameter Store write rather than a
// release of the release tool.
func Property(mapping, repo string) string {
	if mapping == "" {
		return ""
	}
	var m map[string]string
	if json.Unmarshal([]byte(mapping), &m) != nil {
		return ""
	}
	// Keyed on the bare repo name so msnbc/news-app and nbcnews/news-app
	// resolve the same -- the owner changes during an account migration and
	// the brand does not.
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:]
	}
	if p, ok := m[name]; ok {
		return p
	}
	return m[repo]
}

// Put appends one row. It never returns an error that should stop a release;
// the error is returned only so the caller can log it.
func Put(ctx context.Context, e Entry) error {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		return fmt.Errorf("no AWS_REGION")
	}
	creds, err := awssig.FromEnvironment()
	if err != nil {
		return err
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}

	str := func(v string) map[string]any { return map[string]any{"S": v} }
	item := map[string]any{
		// The partition is the ENVIRONMENT, which is also what each execution
		// role's IAM condition pins. A test function physically cannot write a
		// prod row.
		"pk":         str("ENV#" + e.Env),
		"sk":         str(e.At.Format(time.RFC3339Nano) + "#" + e.Repo + "#" + e.Action),
		"action":     str(e.Action),
		"repo":       str(e.Repo),
		"version":    str(e.Version),
		"branch":     str(e.Branch),
		"sha":        str(e.SHA),
		"actor":      str(e.Actor),
		"actorId":    str(e.ActorID),
		"botVersion": str(e.BotVersion),
		"botCommit":  str(e.BotCommit),
		"source":     str(e.Source),
		"at":         str(e.At.Format(time.RFC3339)),
		"dryRun":     map[string]any{"BOOL": e.DryRun},
		"commits":    map[string]any{"N": fmt.Sprint(e.Commits)},
	}
	if e.Property != "" {
		item["property"] = str(e.Property)
	}
	if e.URL != "" {
		item["url"] = str(e.URL)
	}
	if len(e.Issues) > 0 {
		ss := make([]string, len(e.Issues))
		copy(ss, e.Issues)
		item["issues"] = map[string]any{"SS": ss}
	}

	body, err := json.Marshal(map[string]any{"TableName": table, "Item": item})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://dynamodb."+region+".amazonaws.com/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	// ⚠️ DynamoDB is AWS-JSON 1.0, not 1.1 like SSM. The wrong content type is
	// rejected with an UnknownOperationException that names neither.
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810.PutItem")
	awssig.Sign(req, body, creds, "dynamodb", region, time.Now())

	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != 200 {
		// Never echo the body: a DynamoDB error names the table ARN and the
		// calling identity.
		return fmt.Errorf("dynamodb returned %d", resp.StatusCode)
	}
	return nil
}
