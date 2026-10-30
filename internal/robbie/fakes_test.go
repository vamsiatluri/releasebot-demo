package robbie

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/githubclient"
	"github.com/vamsiatluri/releasebot-migration/internal/jiraclient"
	"github.com/vamsiatluri/releasebot-migration/internal/obs"
	"github.com/vamsiatluri/releasebot-migration/internal/rarc"
)

type fakeGH struct {
	mu        sync.Mutex
	files     map[string]map[string]string // branch -> path -> content
	compares  map[string][]githubclient.Commit
	prs       []githubclient.PRInfo
	approvals map[int]int
	tags      []githubclient.Tag
	calls     []string
	commits   map[string]string // path -> content committed
	release   githubclient.Release
	openPRs   int
}

func (g *fakeGH) log(f string, a ...any) {
	g.mu.Lock()
	g.calls = append(g.calls, fmt.Sprintf(f, a...))
	g.mu.Unlock()
}

func (g *fakeGH) ListOpenPRs(_ context.Context, repo, base string) ([]githubclient.PRInfo, error) {
	g.log("list %s %s", repo, base)
	return g.prs, nil
}
func (g *fakeGH) Approvals(_ context.Context, _ string, n int) (int, error) {
	return g.approvals[n], nil
}
func (g *fakeGH) PRMergeable(context.Context, string, int) (bool, string, error) {
	return true, "clean", nil
}
func (g *fakeGH) MergePR(_ context.Context, repo string, n int, method, title string) error {
	g.log("mergepr %d %s %s", n, method, title)
	return nil
}
func (g *fakeGH) AddLabels(_ context.Context, _ string, n int, labels []string) error {
	g.log("labels %d %s", n, strings.Join(labels, ","))
	return nil
}
func (g *fakeGH) MergeBranches(_ context.Context, repo, base, head, msg string) (string, bool, error) {
	g.log("merge %s<-%s", base, head)
	return "mergesha", true, nil
}
func (g *fakeGH) CompareCommits(_ context.Context, repo, base, head string) ([]githubclient.Commit, error) {
	g.log("compare %s...%s", base, head)
	return g.compares[base+"..."+head], nil
}
func (g *fakeGH) GetFile(_ context.Context, repo, path, ref string) (string, error) {
	if c, ok := g.files[ref][path]; ok {
		return c, nil
	}
	return "", &githubclient.HTTPError{StatusCode: 404}
}
func (g *fakeGH) CommitFiles(_ context.Context, repo, branch string, files map[string]string, msg string) (string, error) {
	g.log("commit %s %s", branch, msg)
	g.commits = files
	return "bumpsha0000000", nil
}
func (g *fakeGH) CreateTag(_ context.Context, repo, tag, sha, msg string) error {
	g.log("tag %s %s", tag, sha)
	return nil
}
func (g *fakeGH) CreateRelease(_ context.Context, repo, tag, sha, body string) (githubclient.Release, error) {
	g.log("release %s", tag)
	g.release = githubclient.Release{TagName: tag, Body: body, HTMLURL: "https://github.com/" + repo + "/releases/tag/" + tag}
	return g.release, nil
}
func (g *fakeGH) LatestRelease(context.Context, string) (githubclient.Release, error) {
	return g.release, nil
}
func (g *fakeGH) ListTags(context.Context, string) ([]githubclient.Tag, error) { return g.tags, nil }
func (g *fakeGH) OpenPR(_ context.Context, repo, head, base, title, body string) (*githubclient.PullRequest, bool, error) {
	g.openPRs++
	g.log("openpr %s->%s", head, base)
	return &githubclient.PullRequest{Number: 100 + g.openPRs, HTMLURL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, 100+g.openPRs)}, true, nil
}

type fakeJira struct {
	created     []map[string]any
	transitions []string
	searches    []string
	versions    []string
	fixes       []string
	issue       jiraclient.Issue
}

func (j *fakeJira) CreateIssue(_ context.Context, f map[string]any) (string, error) {
	j.created = append(j.created, f)
	return "DEMOREL-9", nil
}
func (j *fakeJira) Transition(_ context.Context, key, id string) error {
	j.transitions = append(j.transitions, key+"="+id)
	return nil
}
func (j *fakeJira) GetIssue(_ context.Context, key string, _ []string) (jiraclient.Issue, error) {
	return j.issue, nil
}
func (j *fakeJira) SearchJQL(_ context.Context, jql string, _ []string) ([]jiraclient.Issue, error) {
	j.searches = append(j.searches, jql)
	// Echo every key in "key in (...)" back: everything passes the filter.
	var out []jiraclient.Issue
	if i := strings.Index(jql, "key in ("); i >= 0 {
		rest := jql[i+len("key in ("):]
		for _, k := range strings.Split(rest[:strings.Index(rest, ")")], ",") {
			out = append(out, jiraclient.Issue{Key: k})
		}
	}
	return out, nil
}
func (j *fakeJira) EnsureVersion(_ context.Context, pid, name string) (string, error) {
	j.versions = append(j.versions, pid+":"+name)
	return "1", nil
}
func (j *fakeJira) AddFixVersion(_ context.Context, key, name string) error {
	j.fixes = append(j.fixes, key+"="+name)
	return nil
}
func (j *fakeJira) BrowseURL(k string) string {
	return "https://example-demo.atlassian.net/browse/" + k
}
func (j *fakeJira) Base() string { return "https://example-demo.atlassian.net" }

type post struct{ channel, text, ctx string }

type fakeSlack struct {
	mu     sync.Mutex
	posts  []post
	topics []string
}

func (s *fakeSlack) Post(_ context.Context, ch, text, c string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts = append(s.posts, post{ch, text, c})
	return nil
}
func (s *fakeSlack) RespondBlocks(_ context.Context, url, text, c string) error {
	return s.Post(context.Background(), "response_url:"+url, text, c)
}
func (s *fakeSlack) SetTopic(_ context.Context, ch, topic string) error {
	s.topics = append(s.topics, ch+"="+topic)
	return nil
}

func (s *fakeSlack) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, p := range s.posts {
		fmt.Fprintf(&b, "[%s] %s\n", p.channel, p.text)
	}
	return b.String()
}

func fixture(t *testing.T) *rarc.Config {
	t.Helper()
	raw, err := os.ReadFile("../rarc/testdata/rarc-example.yml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := rarc.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type rig struct {
	bot   *Bot
	gh    *fakeGH
	jira  *fakeJira
	slack *fakeSlack
}

func newRig(t *testing.T) *rig {
	gh := &fakeGH{
		files: map[string]map[string]string{"releases": {
			"package.json":      "{\n  \"name\": \"kettle\",\n  \"version\": \"1.4.0\"\n}\n",
			"package-lock.json": "{\n  \"version\": \"1.4.0\",\n  \"lockfileVersion\": 3,\n  \"packages\": {\"\": {\"version\": \"1.4.0\"}}\n}\n",
		}},
		compares: map[string][]githubclient.Commit{
			"v1.4.0...releases": {
				{SHA: "aaaaaaa1111", Title: "KETL-7: whistle on boil (#41)", Message: "KETL-7: whistle on boil (#41)", Author: "alice"},
				{SHA: "bbbbbbb2222", Title: "Merge develop into releases", Message: "Merge develop into releases", Author: "bot"},
				{SHA: "ccccccc3333", Title: "[skip-changelog] tidy", Message: "[skip-changelog] tidy", Author: "bob"},
				{SHA: "ddddddd4444", Title: "OTHER-3 copper base", Message: "OTHER-3 copper base\n\nalso OTHER-4", Author: "carol"},
			},
			"releases...develop": {{SHA: "eeeeeee5555", Title: "KETL-9 lid", Author: "dan"}},
			"v1.3.9...v1.4.0":    {{SHA: "fffffff6666", Title: "KETL-5 spout", Message: "KETL-5 spout", Author: "erin"}},
		},
		prs: []githubclient.PRInfo{
			{Number: 41, Title: "whistle", Labels: []string{"Ready To Merge"}, Author: "alice"},
			{Number: 42, Title: "wip", Labels: []string{"ready to merge", "do not merge"}, Author: "bob"},
			{Number: 43, Title: "unapproved", Labels: []string{"ready to merge"}, Author: "carol"},
			{Number: 44, Title: "unlabelled", Author: "dan"},
		},
		approvals: map[int]int{41: 1, 42: 2, 43: 0},
		tags:      []githubclient.Tag{{Name: "v1.4.0"}, {Name: "v1.3.9"}, {Name: "v1.3.10-rc.1"}, {Name: "nightly"}},
	}
	j, s := &fakeJira{}, &fakeSlack{}
	return &rig{gh: gh, jira: j, slack: s, bot: &Bot{
		Cfg: fixture(t), GH: gh, Jira: j, Slack: s, Version: "abc1234",
		Env:   Env{Stage: "test", AccountLabel: "rehearsal", JiraAccountID: "acct-1"},
		Log:   obs.New("test", "test"),
		Sleep: func(time.Duration) {},
	}}
}

func (r *rig) run(t *testing.T, text string) string {
	t.Helper()
	r.bot.Run(context.Background(), Request{UserID: "U1", ChannelID: "C0ORIGIN", Text: "<@U0DEMOBOT> " + text, RequestID: "req-1"})
	return r.slack.all()
}
