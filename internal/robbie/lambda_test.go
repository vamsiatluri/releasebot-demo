package robbie

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/events"
	"github.com/vamsiatluri/releasebot-migration/internal/obs"
	"github.com/vamsiatluri/releasebot-migration/internal/rarc"
	"github.com/vamsiatluri/releasebot-migration/internal/slackverify"
)

const testSecret = "test-signing-secret"

type fakeQueue struct {
	sent []string
	err  error
}

func (q *fakeQueue) Send(_ context.Context, body string) error {
	if q.err != nil {
		return q.err
	}
	q.sent = append(q.sent, body)
	return nil
}

func newApp(t *testing.T, kind Kind) (*App, *rig) {
	r := newRig(t)
	a := &App{
		Kind: kind,
		Env:  Env{Stage: "test", SigningSecret: testSecret, SigningVersion: "v0", AccountLabel: "rehearsal"},
		Loader: &rarc.Loader{Primary: "mem://fixture", Fetch: func(context.Context, string) ([]byte, error) {
			return fixtureBytes, nil
		}},
		Log:     obs.New("test", "test"),
		Version: "abc1234",
		NewBot:  func(*rarc.Config) *Bot { return r.bot },
	}
	return a, r
}

var fixtureBytes []byte

func mustRead(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return b
}

func signedReq(body string, now time.Time, headers map[string]string) events.APIGatewayProxyRequest {
	ts := strconv.FormatInt(now.Unix(), 10)
	h := map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         slackverify.Sign(testSecret, ts, []byte(body)),
	}
	for k, v := range headers {
		h[k] = v
	}
	return events.APIGatewayProxyRequest{Path: "/cut", Body: body, Headers: h,
		RequestContext: events.ProxyContext{RequestID: "req-9", Stage: "test"}}
}

func slash(text string) string {
	v := url.Values{}
	v.Set("command", "/release-bot")
	v.Set("text", text)
	v.Set("user_id", "U1")
	v.Set("channel_id", "C0ORIGIN")
	v.Set("response_url", "https://hooks.slack.com/commands/T1/1/x")
	return v.Encode()
}

func TestMain(m *testing.M) {
	fixtureBytes = mustRead("../rarc/testdata/rarc-example.yml")
	m.Run()
}

func TestSlackEndpoint(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		req        events.APIGatewayProxyRequest
		status     int
		bodyHas    string
		wantPosted bool
	}{
		{name: "unsigned", req: events.APIGatewayProxyRequest{Path: "/cut", Body: slash("projects")}, status: 401},
		{name: "forged", req: func() events.APIGatewayProxyRequest {
			r := signedReq(slash("projects"), now, nil)
			r.Body = slash("cut kettle")
			return r
		}(), status: 401},
		{name: "stale", req: signedReq(slash("projects"), now.Add(-10*time.Minute), nil), status: 401},
		{name: "url_verification", req: signedReq(`{"type":"url_verification","challenge":"chal-42"}`, now, nil),
			status: 200, bodyHas: `"challenge":"chal-42"`},
		{name: "retry ignored", req: signedReq(slash("cut kettle"), now, map[string]string{"X-Slack-Retry-Num": "1"}),
			status: 200, bodyHas: "Ignoring retry"},
		{name: "slash projects", req: signedReq(slash("projects"), now, nil), status: 200, wantPosted: true},
		{name: "lower-case headers", req: func() events.APIGatewayProxyRequest {
			r := signedReq(slash("projects"), now, nil)
			h := map[string]string{}
			for k, v := range r.Headers {
				h[strings.ToLower(k)] = v
			}
			r.Headers = h
			return r
		}(), status: 200, wantPosted: true},
		{name: "legacy test path", req: func() events.APIGatewayProxyRequest {
			r := signedReq(slash("projects"), now, nil)
			r.Path = "/cutReleaseTest"
			return r
		}(), status: 200, wantPosted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, r := newApp(t, KindCut)
			a.Now = func() time.Time { return now }
			res := a.HandleHTTP(context.Background(), tc.req)
			if res.StatusCode != tc.status || !strings.Contains(res.Body, tc.bodyHas) {
				t.Fatalf("got %d %q", res.StatusCode, res.Body)
			}
			if posted := len(r.slack.posts) > 0; posted != tc.wantPosted {
				t.Fatalf("posted=%v: %s", posted, r.slack.all())
			}
			if tc.wantPosted && !strings.Contains(r.slack.all(), "[response_url:https://hooks.slack.com/commands/T1/1/x]") {
				t.Fatalf("slash command not answered on its response_url: %s", r.slack.all())
			}
		})
	}
}

func TestAsyncEnqueueThenSQS(t *testing.T) {
	a, r := newApp(t, KindCut)
	q := &fakeQueue{}
	a.Env.AsyncMode, a.Queue = "sqs", q

	res := a.HandleHTTP(context.Background(), signedReq(slash("cut kettle"), time.Now(), nil))
	if res.StatusCode != 200 || !strings.Contains(res.Body, "Working on it") {
		t.Fatalf("ack = %d %q", res.StatusCode, res.Body)
	}
	if len(q.sent) != 1 || len(r.gh.calls) != 0 {
		t.Fatalf("not queued, or work ran inline: sent=%d calls=%v", len(q.sent), r.gh.calls)
	}

	ev, _ := json.Marshal(map[string]any{"Records": []map[string]string{
		{"messageId": "m1", "eventSource": "aws:sqs", "body": q.sent[0]},
		{"messageId": "m2", "eventSource": "aws:sqs", "body": `{"hello":"not a job"}`},
	}})
	out, err := a.Invoke(context.Background(), ev)
	if err != nil || string(out) != `{"batchItemFailures":[]}` {
		t.Fatalf("Invoke = %s %v", out, err)
	}
	mustContain(t, r.slack.all(), "cut complete")
	mustContain(t, strings.Join(r.gh.calls, "\n"), "merge releases<-develop")
}

func TestAsyncEnqueueFailureRunsInline(t *testing.T) {
	a, r := newApp(t, KindCut)
	a.Env.AsyncMode, a.Queue = "sqs", &fakeQueue{err: errors.New("AccessDenied")}
	a.HandleHTTP(context.Background(), signedReq(slash("projects"), time.Now(), nil))
	mustContain(t, r.slack.all(), "Configured projects")
}

func TestInitFailureRefusesButHealthAnswers(t *testing.T) {
	a, _ := newApp(t, KindCut)
	a.InitErr = errors.New("env: SLACK_SIGNING_SECRET (or SLACK_SIGNING_SECRET_SSM_PARAM) is required")
	if res := a.HandleHTTP(context.Background(), signedReq(slash("projects"), time.Now(), nil)); res.StatusCode != 503 {
		t.Fatalf("status %d", res.StatusCode)
	}
	res := a.HandleHTTP(context.Background(), events.APIGatewayProxyRequest{Path: "/cut/health"})
	if res.StatusCode != 503 || !strings.Contains(res.Body, "SLACK_SIGNING_SECRET") {
		t.Fatalf("health = %d %s", res.StatusCode, res.Body)
	}
}

func TestHealth(t *testing.T) {
	a, _ := newApp(t, KindMergeback)
	res := a.HandleHTTP(context.Background(), events.APIGatewayProxyRequest{Path: "/mergeback", QueryStringParameters: map[string]string{"health": "1"}})
	if res.StatusCode != 200 {
		t.Fatalf("health %d %s", res.StatusCode, res.Body)
	}
	mustContain(t, res.Body, `"mode":"robbie"`, `"projects":2`, `"account":"rehearsal"`)
}

func TestMatchMergebackProject(t *testing.T) {
	c := fixture(t)
	cases := []struct {
		name    string
		summary string
		want    string
		err     error
	}{
		{name: "match", summary: "RELEASE: Kettle release v1.4.1", want: "Kettle"},
		{name: "most specific wins, and it has automergeback off", summary: "RELEASE: Kettle release admin v2.0.0", want: "teapot-web", err: ErrMergebackOff},
		{name: "no match", summary: "RELEASE: Samovar release v1.0.0", err: ErrNoProjectMatch},
		{name: "empty", summary: "", err: ErrMissingSummary},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := MatchMergebackProject(c, tc.summary)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if tc.want != "" && (p == nil || p.Name != tc.want) {
				t.Fatalf("project = %+v", p)
			}
		})
	}
}

func TestJiraMergebackEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		bodyHas string
	}{
		{name: "not json", body: "{", status: 400, bodyHas: "not JSON"},
		{name: "no match is a clear 400", body: `{"issue":{"key":"DEMOREL-1","fields":{"summary":"RELEASE: Samovar release v1.0.0"}}}`,
			status: 400, bodyHas: "no project's releaseIssueTitleFormat matches"},
		{name: "automergeback off", body: `{"issue":{"key":"DEMOREL-2","fields":{"summary":"RELEASE: Kettle release admin v2.0.0"}}}`,
			status: 400, bodyHas: "automergeback off"},
		{name: "ok", body: `{"issue":{"key":"DEMOREL-3","fields":{"summary":"RELEASE: Kettle release v1.4.0"}}}`,
			status: 200, bodyHas: `"project":"Kettle"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, r := newApp(t, KindMergeback)
			res := a.HandleHTTP(context.Background(), events.APIGatewayProxyRequest{Path: "/mergeback", HTTPMethod: "POST", Body: tc.body})
			if res.StatusCode != tc.status || !strings.Contains(res.Body, tc.bodyHas) {
				t.Fatalf("got %d %s", res.StatusCode, res.Body)
			}
			if tc.status == 200 {
				// No Slack origin: the answer goes to the team channel.
				mustContain(t, r.slack.all(), "[C0DEMOTEAM] Starting mergeback for Kettle", "Mergeback complete")
			}
		})
	}
}

func TestLoadEnv(t *testing.T) {
	clear := func(t *testing.T) {
		for _, k := range []string{"ROBBIE_ENV", "RARC_ENV", "GITHUB_TOKEN", "SLACK_TOKEN", "SLACK_SIGNING_SECRET",
			"SLACK_SIGNING_SECRET_VERSION_NUMBER", "JIRA_TOKEN", "DD_API_KEY", "GITHUB_TOKEN_SSM_PARAM",
			"SLACK_TOKEN_SSM_PARAM", "SLACK_SIGNING_SECRET_SSM_PARAM", "JIRA_TOKEN_SSM_PARAM",
			"ASYNC_ENQUEUE_MODE", "ASYNC_QUEUE_URL", "RELEASEBOT_DRY_RUN", "JIRA_EMAIL", "JIRA_USER"} {
			t.Setenv(k, "")
		}
	}
	orig := Resolver
	defer func() { Resolver = orig }()
	Resolver = func(_ context.Context, ref string) (string, error) {
		if strings.HasPrefix(ref, "ssm:") {
			return "resolved(" + strings.TrimPrefix(ref, "ssm:") + ")", nil
		}
		return ref, nil
	}

	t.Run("terraform env mode", func(t *testing.T) {
		clear(t)
		t.Setenv("ROBBIE_ENV", "prod")
		t.Setenv("GITHUB_TOKEN", "gh")
		t.Setenv("SLACK_TOKEN", "xoxb")
		t.Setenv("SLACK_SIGNING_SECRET", "sig")
		t.Setenv("SLACK_SIGNING_SECRET_VERSION_NUMBER", "v0")
		t.Setenv("JIRA_EMAIL", "bot@example.test")
		e, err := LoadEnv(context.Background(), KindCut)
		if err != nil || e.Stage != "prod" || e.SigningSecret != "sig" || e.JiraEmail != "bot@example.test" || e.SecretSources["GITHUB_TOKEN"] != "env" {
			t.Fatalf("%+v %v", e, err)
		}
	})
	t.Run("terraform ssm mode", func(t *testing.T) {
		clear(t)
		t.Setenv("GITHUB_TOKEN_SSM_PARAM", "/robbie/test/secrets/github-token")
		t.Setenv("SLACK_TOKEN_SSM_PARAM", "/robbie/test/secrets/slack-token")
		t.Setenv("SLACK_SIGNING_SECRET_SSM_PARAM", "/robbie/test/secrets/slack-signing-secret")
		e, err := LoadEnv(context.Background(), KindCut)
		if err != nil || e.GitHubToken != "resolved(/robbie/test/secrets/github-token)" || e.SecretSources["SLACK_TOKEN"] != "ssm-param" || e.SigningVersion != "v0" {
			t.Fatalf("%+v %v", e, err)
		}
	})
	t.Run("mergeback needs no signing secret", func(t *testing.T) {
		clear(t)
		t.Setenv("GITHUB_TOKEN", "gh")
		t.Setenv("SLACK_TOKEN", "xoxb")
		if _, err := LoadEnv(context.Background(), KindMergeback); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadEnv(context.Background(), KindCut); err == nil || !strings.Contains(err.Error(), "SLACK_SIGNING_SECRET") {
			t.Fatalf("cut without signing secret: %v", err)
		}
	})
	t.Run("old demo name RARC_ENV still works", func(t *testing.T) {
		clear(t)
		t.Setenv("RARC_ENV", "prod")
		t.Setenv("RELEASEBOT_DRY_RUN", "1")
		e, err := LoadEnv(context.Background(), KindCut)
		if err != nil || e.Stage != "prod" {
			t.Fatalf("%+v %v", e, err)
		}
	})
	t.Run("bad stage", func(t *testing.T) {
		clear(t)
		t.Setenv("ROBBIE_ENV", "staging")
		t.Setenv("RELEASEBOT_DRY_RUN", "1")
		if _, err := LoadEnv(context.Background(), KindCut); err == nil {
			t.Fatal("accepted staging")
		}
	})
	t.Run("async needs a queue", func(t *testing.T) {
		clear(t)
		t.Setenv("RELEASEBOT_DRY_RUN", "1")
		t.Setenv("ASYNC_ENQUEUE_MODE", "sqs")
		if _, err := LoadEnv(context.Background(), KindCut); err == nil {
			t.Fatal("sqs mode without ASYNC_QUEUE_URL accepted")
		}
	})
}
