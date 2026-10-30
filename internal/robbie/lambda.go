package robbie

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/events"
	"github.com/vamsiatluri/releasebot-migration/internal/githubclient"
	"github.com/vamsiatluri/releasebot-migration/internal/jiraclient"
	"github.com/vamsiatluri/releasebot-migration/internal/obs"
	"github.com/vamsiatluri/releasebot-migration/internal/rarc"
	"github.com/vamsiatluri/releasebot-migration/internal/runtime"
	"github.com/vamsiatluri/releasebot-migration/internal/slackclient"
	"github.com/vamsiatluri/releasebot-migration/internal/slackverify"
	"github.com/vamsiatluri/releasebot-migration/internal/sqsq"
)

// App is one function's process: environment, cached config, and how to build
// a Bot for an invocation.
type App struct {
	Kind    Kind
	Env     Env
	InitErr error // a bad environment: every request but /health is refused
	Loader  *rarc.Loader
	Queue   sqsq.Sender
	Log     *obs.Logger
	Version string
	Now     func() time.Time
	// NewBot builds the bot for a config. Tests replace it with fakes.
	NewBot func(cfg *rarc.Config) *Bot
}

// NewApp reads the environment and tries the config once (a failure is retried
// on the next request, not cached).
func NewApp(ctx context.Context, kind Kind, version string) *App {
	env, err := LoadEnv(ctx, kind)
	log := obs.New("releasebot-"+string(kind), env.Stage).With("mode", "robbie")
	a := &App{Kind: kind, Env: env, InitErr: err, Log: log, Version: version,
		Loader: &rarc.Loader{Primary: env.ConfigURL, Backup: env.ConfigURLBackup,
			Fetch: rarc.DefaultFetcher(env.GitHubToken)}}
	if env.Async() {
		a.Queue = sqsq.New(env.QueueURL)
	}
	a.NewBot = a.defaultBot
	if err != nil {
		log.With("error", err.Error()).Error("INIT FAILED: environment is incomplete; refusing requests")
		return a
	}
	if _, src, err := a.Loader.Get(ctx); err != nil {
		log.With("error", err.Error()).Warn("config not loaded at INIT; will retry on the first request")
	} else {
		log.With("config_url", redactURL(src.URL)).With("backup", fmt.Sprint(src.Backup)).
			With("projects", fmt.Sprint(src.Projects)).With("async", env.AsyncMode).
			With("dry_run", fmt.Sprint(env.DryRun)).Info("release bot ready")
	}
	return a
}

func (a *App) defaultBot(cfg *rarc.Config) *Bot {
	jiraBase := a.Env.JiraAPI
	if jiraBase == "" {
		jiraBase = cfg.Jira.BaseURL
	}
	return &Bot{
		Cfg:     cfg,
		Env:     a.Env,
		GH:      githubclient.New(a.Env.GitHubAPI, a.Env.GitHubToken, a.Env.DryRun),
		Jira:    jiraclient.New(jiraBase, a.Env.JiraEmail, a.Env.JiraToken, a.Env.DryRun),
		Slack:   slackclient.New(a.Env.SlackAPI, a.Env.SlackToken, a.Env.DryRun),
		Log:     a.Log,
		Version: a.Version,
	}
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Main runs the Lambda runtime loop for this function.
func Main(kind Kind, version string) {
	app := NewApp(context.Background(), kind, version)
	runtime.Start(func(ctx context.Context, _ runtime.Invocation, payload []byte) ([]byte, error) {
		return app.Invoke(ctx, payload)
	})
}

// Invoke handles one raw Lambda payload: an SQS event or an API Gateway proxy event.
func (a *App) Invoke(ctx context.Context, payload []byte) ([]byte, error) {
	if recs, ok := sqsq.Parse(payload); ok {
		a.HandleSQS(ctx, recs)
		// Never ask SQS to redeliver: a half-finished cut or release must not
		// run again on its own. A failure is logged and reported in Slack.
		return []byte(`{"batchItemFailures":[]}`), nil
	}
	var req events.APIGatewayProxyRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("payload is neither an SQS event nor an API Gateway proxy event: %w", err)
	}
	return json.Marshal(a.HandleHTTP(ctx, req))
}

// HandleHTTP answers one API Gateway proxy request.
func (a *App) HandleHTTP(ctx context.Context, req events.APIGatewayProxyRequest) events.APIGatewayProxyResponse {
	if strings.HasSuffix(req.Path, "/health") || req.QueryStringParameters["health"] == "1" {
		return a.health(ctx)
	}
	if a.InitErr != nil {
		return events.Text(503, "release bot is not configured; see the function log")
	}
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		b, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return events.Text(400, "bad request body")
		}
		body = b
	}
	log := a.Log.WithRequestID(req.RequestContext.RequestID).With("path", req.Path).With("stage", req.RequestContext.Stage)
	if a.Kind == KindMergeback {
		return a.handleJira(ctx, log, body)
	}
	return a.handleSlack(ctx, log, req, body)
}

func (a *App) handleSlack(ctx context.Context, log *obs.Logger, req events.APIGatewayProxyRequest, body []byte) events.APIGatewayProxyResponse {
	// Slack retries anything not answered in 3 seconds. Re-running a cut or a
	// release because the first one was slow would duplicate it, so retries
	// are acknowledged and dropped.
	if n := req.Header("X-Slack-Retry-Num"); n != "" {
		log.With("retry", n).Info("ignoring Slack retry")
		r := events.Text(200, "Ignoring retry request from slack")
		r.Headers["X-Slack-No-Retry"] = "1"
		return r
	}
	if err := slackverify.VerifyVersion(a.Env.SigningSecret, a.Env.SigningVersion,
		req.Header(slackverify.HeaderSignature), req.Header(slackverify.HeaderTimestamp), body, a.now()); err != nil {
		log.With("reason", err.Error()).Warn("rejected unsigned or stale Slack request")
		return events.Text(401, "Could not validate request")
	}

	cfg, _, cfgErr := a.Loader.Get(ctx)
	botID := ""
	if cfg != nil {
		botID = cfg.Slack.SlackbotID
	}
	in, err := ParseSlack(body, botID)
	if err != nil {
		return events.Text(400, err.Error())
	}
	if in.Challenge != "" {
		return events.JSON(200, mustJSON(map[string]string{"challenge": in.Challenge}))
	}
	if in.Ignore != "" {
		log.With("reason", in.Ignore).Info("ignoring Slack event")
		return events.Text(200, in.Ignore)
	}
	if cfgErr != nil {
		log.With("error", cfgErr.Error()).Error("config unavailable")
		if in.Slash {
			return slackReply(":x: The release config could not be loaded; see the function log.")
		}
		return events.Text(200, "")
	}
	in.Req.RequestID = req.RequestContext.RequestID

	if a.Env.Async() && a.Queue != nil {
		job, _ := json.Marshal(Job{Marker: true, Request: in.Req})
		if err := a.Queue.Send(ctx, string(job)); err == nil {
			log.With("user", in.Req.UserID).Info("queued Slack command")
			if in.Slash {
				return slackReply(":hourglass_flowing_sand: Working on it…")
			}
			return events.Text(200, "")
		} else {
			// Doing it inline may blow Slack's 3 seconds; losing it silently is worse.
			log.With("error", err.Error()).Error("enqueue failed; running inline")
		}
	}
	a.NewBot(cfg).Run(ctx, in.Req)
	return events.Text(200, "")
}

func slackReply(text string) events.APIGatewayProxyResponse {
	return events.JSON(200, mustJSON(map[string]string{"response_type": "ephemeral", "text": text}))
}

// Job is a queued Slack command. The marker distinguishes it from anything else
// that could arrive on the queue.
type Job struct {
	Marker  bool    `json:"releasebotRobbieJob"`
	Request Request `json:"request"`
}

// HandleSQS runs queued commands.
func (a *App) HandleSQS(ctx context.Context, recs []sqsq.Record) {
	if a.InitErr != nil {
		a.Log.With("error", a.InitErr.Error()).Error("dropping queued work: environment incomplete")
		return
	}
	cfg, _, err := a.Loader.Get(ctx)
	if err != nil {
		a.Log.With("error", err.Error()).Error("dropping queued work: config unavailable")
		return
	}
	for _, r := range recs {
		var j Job
		if json.Unmarshal([]byte(r.Body), &j) != nil || !j.Marker {
			a.Log.With("message_id", r.MessageID).Warn("ignoring an SQS message that is not a release bot job")
			continue
		}
		j.Request.Source = "sqs"
		a.Log.With("message_id", r.MessageID).With("user", j.Request.UserID).Info("running queued command")
		a.NewBot(cfg).Run(ctx, j.Request)
	}
}

// ── Jira mergeback ──────────────────────────────────────────────────────────

// JiraWebhook is the part of the Jira automation payload used.
type JiraWebhook struct {
	Issue struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
		} `json:"fields"`
	} `json:"issue"`
}

var (
	ErrNoProjectMatch = errors.New("no project's releaseIssueTitleFormat matches the summary")
	ErrMergebackOff   = errors.New("the matching project has automergeback off")
	ErrMissingSummary = errors.New("the Jira payload has no issue.fields.summary")
)

// MatchMergebackProject finds the project whose release ticket title format
// matches a ticket summary: the summary must contain the format's fixed text
// (the format without its %s). The most specific (longest) match wins, so
// "RELEASE: App" cannot capture "RELEASE: App Admin".
func MatchMergebackProject(cfg *rarc.Config, summary string) (*rarc.Project, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, ErrMissingSummary
	}
	var best *rarc.Project
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		prefix := p.TitlePrefix()
		if strings.TrimSpace(prefix) == "" || !strings.Contains(summary, prefix) {
			continue
		}
		if best == nil || len(prefix) > len(best.TitlePrefix()) {
			best = p
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoProjectMatch, summary)
	}
	if !best.AutoMergeback {
		return best, fmt.Errorf("%w (project %s)", ErrMergebackOff, best.Name)
	}
	return best, nil
}

func (a *App) handleJira(ctx context.Context, log *obs.Logger, body []byte) events.APIGatewayProxyResponse {
	var hook JiraWebhook
	if err := json.Unmarshal(body, &hook); err != nil {
		return events.Text(400, "failed to read the Jira request body: not JSON")
	}
	cfg, _, err := a.Loader.Get(ctx)
	if err != nil {
		log.With("error", err.Error()).Error("config unavailable")
		return events.Text(503, "release config unavailable")
	}
	p, err := MatchMergebackProject(cfg, hook.Issue.Fields.Summary)
	if err != nil {
		log.With("summary", hook.Issue.Fields.Summary).With("error", err.Error()).Warn("mergeback: no project")
		return events.JSON(400, mustJSON(map[string]any{"ok": false, "error": err.Error()}))
	}
	log = log.With("project", p.Name).With("ticket", hook.Issue.Key)
	log.Info("mergeback requested by Jira")
	bot := a.NewBot(cfg)
	if err := bot.Mergeback(ctx, Request{ChannelID: p.Slack.TeamChannel, Source: "jira"}, p, hook.Issue.Key); err != nil {
		log.With("error", err.Error()).Error("mergeback failed")
		return events.JSON(500, mustJSON(map[string]any{"ok": false, "project": p.Name, "error": "failed to create a mergeback: " + err.Error()}))
	}
	return events.JSON(200, mustJSON(map[string]any{"ok": true, "project": p.Name, "ticket": hook.Issue.Key}))
}

// Mergeback runs the mergeback for p (the Jira path; Slack goes through Run).
func (b *Bot) Mergeback(ctx context.Context, req Request, p *rarc.Project, ticket string) error {
	o := out{b: b, ctx: ctx, req: req, p: p}
	f := &flow{b: b, o: o, p: p, log: b.Log.With("project", p.Name), ctx: ctx}
	return f.mergeback(ticket)
}

// ── health ──────────────────────────────────────────────────────────────────

func (a *App) health(ctx context.Context) events.APIGatewayProxyResponse {
	h := map[string]any{
		"ok": a.InitErr == nil, "mode": "robbie", "kind": a.Kind, "env": a.Env.Stage,
		"account": a.Env.AccountLabel, "commit": a.Version, "async": a.Env.AsyncMode,
		"dryRun": a.Env.DryRun, "ssmRoot": a.Env.SSMRoot, "secretSources": a.Env.SecretSources,
		"function": os.Getenv("AWS_LAMBDA_FUNCTION_NAME"), "functionVersion": os.Getenv("AWS_LAMBDA_FUNCTION_VERSION"),
	}
	if a.InitErr != nil {
		h["error"] = a.InitErr.Error()
	} else if _, src, err := a.Loader.Get(ctx); err != nil {
		h["ok"], h["configError"] = false, err.Error()
	} else {
		h["configUrl"], h["configBackupUsed"], h["projects"] = redactURL(src.URL), src.Backup, src.Projects
	}
	status := 200
	if h["ok"] != true {
		status = 503
	}
	return events.JSON(status, mustJSON(h))
}

func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "(unparseable)"
	}
	u.RawQuery, u.User = "", nil
	return u.String()
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
