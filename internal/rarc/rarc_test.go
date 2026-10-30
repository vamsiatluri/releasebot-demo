package rarc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func load(t *testing.T) *Config {
	t.Helper()
	raw, err := os.ReadFile("testdata/rarc-example.yml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func TestParseFixture(t *testing.T) {
	c := load(t)
	if c.GitHub.MergeLabel != "ready to merge" || c.GitHub.RecutLabel != "recut" {
		t.Errorf("github block: %+v", c.GitHub)
	}
	if c.Slack.ReleaseChannel != "C0DEMOREL" || c.Slack.SlackbotID != "U0DEMOBOT" {
		t.Errorf("slack block: %+v", c.Slack)
	}
	if c.Jira.BaseURL != "https://example-demo.atlassian.net" || c.Jira.QAStateID != 31 || c.Jira.DoneStateID != 41 {
		t.Errorf("jira block: %+v", c.Jira)
	}
	if want := (JiraFields{Platforms: "customfield_20001", Risk: "customfield_20002",
		Notes: "customfield_20003", EnvVarChange: "customfield_20004"}); c.Jira.Fields != want {
		t.Errorf("jira.fields = %+v, want %+v", c.Jira.Fields, want)
	}

	p := c.Project("kettle")
	if p == nil {
		t.Fatal("case-insensitive lookup of Kettle failed")
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{name: "fullRepo", got: p.FullRepo(), want: "acme-demo/kettle"},
		{name: "approvals", got: int(p.ApprovalsNeeded), want: 1},
		{name: "masterBranch (camelCase key)", got: p.MasterBranch, want: "main"},
		{name: "automergeback", got: p.AutoMergeback, want: true},
		{name: "filesWithVersion", got: strings.Join(p.FilesWithVersion, ","), want: "package.json,package-lock.json"},
		{name: "excludelabels", got: strings.Join(p.ExcludeLabels, ","), want: "do not merge"},
		{name: "filterIssuesQuery", got: p.FilterIssuesQuery, want: "(labels is EMPTY OR labels != botIgnore)"},
		{name: "jira title", got: p.TitleFor("v1.2.3"), want: "RELEASE: Kettle release v1.2.3"},
		{name: "jira title prefix", got: p.TitlePrefix(), want: "RELEASE: Kettle release "},
		{name: "issue type id", got: p.Jira.ReleaseIssueTypeID, want: "30003"},
		{name: "QaTeamMention (mixed case key)", got: strings.Join(p.Slack.QATeamMention, ","), want: "S0DEMOQA"},
		{name: "channel topic", got: p.Slack.ChannelTopic, want: "Kettle is at {version}"},
		{name: "issue id format (project wins)", got: c.IssueIDFormat(p), want: "(?i)[A-Z]+-\\d+"},
		{name: "issue id format (global)", got: c.IssueIDFormat(c.Project("TEAPOT-WEB")), want: "(?i)\\bDEMO-\\d+\\b"},
		{name: "envVarChange: project override wins", got: c.TicketFields(p).EnvVarChange, want: "customfield_29999"},
		{name: "envVarChange: global otherwise", got: c.TicketFields(c.Project("teapot-web")).EnvVarChange, want: "customfield_20004"},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
	if got := strings.Join(c.Names(), ","); got != "Kettle,teapot-web" {
		t.Errorf("Names() = %s", got)
	}
	if c.Project("nope") != nil {
		t.Error("unknown project found")
	}
}

func TestTransitionResolution(t *testing.T) {
	c := load(t)
	cases := []struct {
		name      string
		key       string
		issueType string
		qa        string
		done      string
	}{
		{name: "global", key: "OTHER-1", qa: "31", done: "41"},
		{name: "board (string id)", key: "KETL-7", qa: "21", done: "22"},
		{name: "board, case-insensitive", key: "ketl-7", qa: "21", done: "22"},
		{name: "issue type on board", key: "KETL-7", issueType: "bug", qa: "23", done: "22"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.QAState(tc.key, tc.issueType); got != tc.qa {
				t.Errorf("QAState = %s, want %s", got, tc.qa)
			}
			if got := c.DoneState(tc.key, tc.issueType); got != tc.done {
				t.Errorf("DoneState = %s, want %s", got, tc.done)
			}
		})
	}
	if !c.BoardHasIssueTypes("KETL-1") || c.BoardHasIssueTypes("OTHER-1") {
		t.Error("BoardHasIssueTypes wrong")
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{name: "empty", doc: "", want: "empty"},
		{name: "no projects", doc: "github: {}\nprojects: []\n", want: "no projects"},
		{name: "duplicate", doc: "projects:\n- {name: a, repo: r, organization: o, defaultbranch: d, releasebranch: r}\n- {name: A, repo: r, organization: o, defaultbranch: d, releasebranch: r}\n", want: "twice"},
		{name: "missing branches", doc: "projects:\n- {name: a, repo: r, organization: o}\n", want: "defaultbranch"},
		{name: "bad regexp", doc: "projects:\n- {name: a, repo: r, organization: o, defaultbranch: d, releasebranch: r, jira: {issueIDFormat: '(['}}\n", want: "does not compile"},
		{name: "bad int", doc: "jira: {qaStateId: soon}\nprojects:\n- {name: a, repo: r, organization: o, defaultbranch: d, releasebranch: r}\n", want: "not an integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestJSONConfigParses(t *testing.T) {
	c, err := Parse([]byte(`{"projects":[{"name":"x","repo":"x","organization":"o","defaultbranch":"d","releasebranch":"r"}]}`))
	if err != nil || c.Project("X") == nil {
		t.Fatalf("json: %v", err)
	}
}

// The GitHub token goes to GitHub's raw host only, never to any other https URL.
func TestTokenOnlyToGitHubRaw(t *testing.T) {
	var gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("projects: []"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/config/test.yml")
	if _, err := fetchHTTPS(context.Background(), srv.Client(), u, "ghp_secret"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Fatalf("token sent to a non-GitHub host: %q", gotAuth)
	}
	for host, want := range map[string]bool{
		"raw.githubusercontent.com": true, "RAW.GITHUB.COM": true,
		"example.com": false, "raw.githubusercontent.com.evil.test": false,
	} {
		if IsGitHubRawHost(host) != want {
			t.Errorf("IsGitHubRawHost(%q) != %v", host, want)
		}
	}
}

func TestLoaderBackupAndRetry(t *testing.T) {
	raw, _ := os.ReadFile("testdata/rarc-example.yml")
	calls := map[string]int{}
	primaryUp := false
	fetch := func(_ context.Context, u string) ([]byte, error) {
		calls[u]++
		if u == "https://primary" && !primaryUp {
			return nil, errors.New("down")
		}
		if u == "https://backup" {
			return raw, nil
		}
		return raw, nil
	}

	t.Run("backup used when primary fails", func(t *testing.T) {
		l := &Loader{Primary: "https://primary", Backup: "https://backup", Fetch: fetch}
		_, src, err := l.Get(context.Background())
		if err != nil || !src.Backup || src.URL != "https://backup" || src.Projects != 2 {
			t.Fatalf("src=%+v err=%v", src, err)
		}
	})
	t.Run("a failed load is retried, a good one cached", func(t *testing.T) {
		primaryUp = false
		l := &Loader{Primary: "https://primary", Fetch: fetch}
		if _, _, err := l.Get(context.Background()); err == nil {
			t.Fatal("expected failure")
		}
		primaryUp = true
		if _, _, err := l.Get(context.Background()); err != nil {
			t.Fatalf("retry: %v", err)
		}
		before := calls["https://primary"]
		l.Get(context.Background())
		if calls["https://primary"] != before {
			t.Fatal("config fetched again after a successful load")
		}
	})
	t.Run("unset URL", func(t *testing.T) {
		if _, _, err := (&Loader{}).Get(context.Background()); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestFileFetcher(t *testing.T) {
	b, err := DefaultFetcher("")(context.Background(), "testdata/rarc-example.yml")
	if err != nil || len(b) == 0 {
		t.Fatal(err)
	}
	if _, err := DefaultFetcher("")(context.Background(), "ftp://x/y"); err == nil {
		t.Fatal("ftp accepted")
	}
}
