package robbie

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vamsiatluri/releasebot-migration/internal/jiraclient"
	"github.com/vamsiatluri/releasebot-migration/internal/rarc"
)

func mustContain(t *testing.T, all string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q in:\n%s", w, all)
		}
	}
}

func TestInformationalCommands(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
	}{
		{name: "projects", text: "projects", want: []string{"*Configured projects (2):*", "• `Kettle`", "• `teapot-web`"}},
		{name: "ping", text: "ping", want: []string{"Pong!", "abc1234"}},
		{name: "help", text: "help", want: []string{"`precut [project]`", "`mergeback [project]`"}},
		{name: "unknown project", text: "cut samovar", want: []string{"Could not find project named `samovar`"}},
		{name: "usage", text: "boil kettle", want: []string{"Unsupported command: boil"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			all := r.run(t, tc.text)
			mustContain(t, all, tc.want...)
			mustContain(t, all, "<@U1>") // the asker is tagged in a channel
		})
	}
}

func TestDMRefusesReleaseCommands(t *testing.T) {
	r := newRig(t)
	r.bot.Run(context.Background(), Request{UserID: "U1", ChannelID: "D1", IsDM: true, Text: "cut kettle"})
	all := r.slack.all()
	mustContain(t, all, "can't be run in a DM")
	if strings.Contains(all, "<@U1>") {
		t.Error("tagged the user in their own DM")
	}
	if len(r.gh.calls) != 0 {
		t.Fatalf("a DM touched GitHub: %v", r.gh.calls)
	}
}

func TestPrecut(t *testing.T) {
	r := newRig(t)
	all := r.run(t, "precut kettle")
	mustContain(t, all, "Starting precut", "PR #41 by alice: whistle", "Changes in develop branch", "KETL-9 lid", "Precut complete")
	for _, bad := range []string{"#42", "#43", "#44"} {
		if strings.Contains(all, "PR "+bad) {
			t.Errorf("%s listed as ready (excluded, unapproved or unlabelled)", bad)
		}
	}
}

func TestCut(t *testing.T) {
	r := newRig(t)
	all := r.run(t, "cut kettle")
	mustContain(t, all,
		"Starting cut",
		"[C0DEMOTEAM] <@U1> <!subteam^S0DEMOQA> a new cut for the latest *Kettle* is building on staging now.",
		"aaaaaaa -- alice -- KETL-7: whistle on boil (#41)",
		"View tickets in JIRA",
		"cut complete")
	if strings.Contains(all, "Merge develop") || strings.Contains(all, "[skip-changelog]") {
		t.Errorf("bot merge commit or filtered commit in the changelog:\n%s", all)
	}
	calls := strings.Join(r.gh.calls, "\n")
	mustContain(t, calls, "mergepr 41 squash whistle (#41)", "merge releases<-develop")
	if strings.Contains(calls, "mergepr 42") || strings.Contains(calls, "mergepr 43") {
		t.Errorf("merged a PR that was not ready:\n%s", calls)
	}
	// KETL-7 uses the board's QA transition, OTHER-* the global one.
	mustContain(t, strings.Join(r.jira.transitions, ","), "KETL-7=21", "OTHER-3=31", "OTHER-4=31")
	// filterIssuesQuery went through search/jql semantics.
	if len(r.jira.searches) == 0 || !strings.Contains(r.jira.searches[0], "labels != botIgnore") {
		t.Errorf("filterIssuesQuery not applied: %v", r.jira.searches)
	}
	// The completion message carries the account context line.
	var ctxLine string
	for _, p := range r.slack.posts {
		if strings.Contains(p.text, "cut complete") {
			ctxLine = p.ctx
		}
	}
	mustContain(t, ctxLine, "account `rehearsal`", "env `test`", "request `req-1`")
}

func TestRelease(t *testing.T) {
	r := newRig(t)
	all := r.run(t, "release kettle patch")
	mustContain(t, all,
		"Starting *patch* release for Kettle, old version: *1.4.0* new version: *1.4.1*",
		"`1.4.0 → 1.4.1 (patch)`",
		"[C0DEMOREL] <@U1> We're ready for a *Kettle* release https://example-demo.atlassian.net/browse/DEMOREL-9. Tagged changes at `v1.4.1`",
		"Release *1.4.1* complete",
		"https://github.com/acme-demo/kettle/compare/v1.4.0...v1.4.1")

	// Both version files bumped in one commit, lockfile twice.
	if got := r.gh.commits["package.json"]; !strings.Contains(got, `"version": "1.4.1"`) {
		t.Errorf("package.json not bumped: %s", got)
	}
	if got := r.gh.commits["package-lock.json"]; strings.Count(got, `"1.4.1"`) != 2 {
		t.Errorf("lockfile not bumped twice: %s", got)
	}
	mustContain(t, strings.Join(r.gh.calls, "\n"), "tag v1.4.1 bumpsha0000000", "release v1.4.1")

	// ★ The release notes' first line is what the deploy card parses.
	first := strings.SplitN(r.gh.release.Body, "\n", 2)[0]
	if first != "v1.4.1 - https://github.com/acme-demo/kettle/compare/v1.4.0...v1.4.1" {
		t.Fatalf("release notes first line = %q", first)
	}
	mustContain(t, r.gh.release.Body, "KETL-7", "id%20in%20(KETL-7,OTHER-3,OTHER-4)")
	if strings.Contains(r.gh.release.Body, "`") {
		t.Error("backticks in release notes")
	}

	if len(r.jira.created) != 1 {
		t.Fatalf("created %d tickets", len(r.jira.created))
	}
	f := r.jira.created[0]
	b, _ := json.Marshal(f)
	mustContain(t, string(b),
		`"summary":"RELEASE: Kettle release v1.4.1"`,
		`"project":{"id":"30004"}`,
		`"issuetype":{"id":"30003"}`,
		`"reporter":{"accountId":"acct-1"}`,
		`"customfield_20001":[{"id":"30001"}]`,
		`"customfield_20002":{"id":"30005"}`,
		`"customfield_20003":{"content"`,
		`"customfield_29999":{"value":"No"}`)
	mustContain(t, strings.Join(r.slack.topics, ","), "C0DEMOTEAM=Kettle is at 1.4.1")
}

func TestReleaseEnvVarChangeFlag(t *testing.T) {
	r := newRig(t)
	all := r.run(t, "release kettle minor hasEnvChanges")
	mustContain(t, all, "Environment Variable Change: *Yes*", "1.4.0 → 1.5.0 (minor)")
	b, _ := json.Marshal(r.jira.created[0])
	mustContain(t, string(b), `"customfield_29999":{"value":"Yes"}`)
}

func TestReleaseReportsTheStepThatFailed(t *testing.T) {
	r := newRig(t)
	r.bot.Jira = failingCreate{r.jira}
	all := r.run(t, "release kettle patch")
	mustContain(t, all, ":white_check_mark: Created a GitHub tag", ":white_check_mark: Created a GitHub release",
		":x: Failed to create a JIRA release ticket")
}

type failingCreate struct{ *fakeJira }

func (failingCreate) CreateIssue(context.Context, map[string]any) (string, error) {
	return "", errors.New("jira: 400 field customfield_20001 is not on the screen")
}

// No field keys configured -> those fields are skipped, never guessed.
func TestTicketFieldsWithoutConfiguredKeys(t *testing.T) {
	c := fixture(t)
	c.Jira.Fields = rarc.JiraFields{}
	p := c.Project("teapot-web")
	fields, skipped := TicketFields(c, p, "", "v2.0.0", "notes", false)
	if strings.Join(skipped, ",") != "risk,platforms,notes,envVarChange" {
		t.Fatalf("skipped = %v", skipped)
	}
	for k := range fields {
		if strings.HasPrefix(k, "customfield_") {
			t.Errorf("sent unconfigured custom field %s", k)
		}
	}
	if _, ok := fields["reporter"]; ok {
		t.Error("reporter sent without an account id")
	}
}

func TestMergebackFromSlack(t *testing.T) {
	r := newRig(t)
	all := r.run(t, "mergeback kettle")
	mustContain(t, all, "Starting mergeback for Kettle", "Master mergeback was automerged", "Develop mergeback was automerged", "Mergeback complete")
	calls := strings.Join(r.gh.calls, "\n")
	mustContain(t, calls, "compare v1.3.9...v1.4.0", "openpr releases->main", "labels 101 mergeback", "openpr releases->develop", "mergepr 101 merge", "mergepr 102 merge")
	mustContain(t, strings.Join(r.jira.transitions, ","), "KETL-5=22")
	mustContain(t, strings.Join(r.jira.versions, ","), "30002:v1.4.0 Kettle")
	mustContain(t, strings.Join(r.jira.fixes, ","), "KETL-5=v1.4.0 Kettle")
}

func TestMergebackUsesTheReleaseTicketNotes(t *testing.T) {
	r := newRig(t)
	notes, _ := json.Marshal(jiraclient.TextToADF("v1.4.0 - https://github.com/acme-demo/kettle/compare/v1.3.9...v1.4.0\nKETL-11 handle\nhttps://x/issues/?jql=id%20in%20(KETL-11)"))
	r.jira.issue = jiraclient.Issue{Key: "DEMOREL-9", Fields: map[string]json.RawMessage{"customfield_20003": notes}}
	p := r.bot.Cfg.Project("kettle")
	if err := r.bot.Mergeback(context.Background(), Request{ChannelID: p.Slack.TeamChannel, Source: "jira"}, p, "DEMOREL-9"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.jira.transitions, ","); got != "KETL-11=22" {
		t.Fatalf("transitions = %s", got)
	}
	if strings.Contains(strings.Join(r.gh.calls, "\n"), "compare v1.3.9") {
		t.Error("scanned commits although the ticket had notes")
	}
}

func TestMergebackWithoutTwoTags(t *testing.T) {
	r := newRig(t)
	r.gh.tags = nil
	all := r.run(t, "mergeback kettle")
	mustContain(t, all, "could not run because there aren't two tags to compare")
	if strings.Contains(strings.Join(r.gh.calls, "\n"), "openpr") {
		t.Error("opened mergeback PRs without knowing what shipped")
	}
}

func TestJiraTicketCommand(t *testing.T) {
	r := newRig(t)
	r.gh.release.TagName, r.gh.release.Body = "v1.4.0", "v1.4.0 - https://github.com/acme-demo/kettle/compare/v1.3.9...v1.4.0\n"
	all := r.run(t, "jiraticket kettle")
	mustContain(t, all, "Creating JIRA release ticket", "Successfully created JIRA release ticket")
	b, _ := json.Marshal(r.jira.created[0])
	mustContain(t, string(b), `"summary":"RELEASE: Kettle release v1.4.0"`)
}
