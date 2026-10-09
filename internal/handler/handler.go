// Package handler adapts API Gateway proxy events onto the release service.
//
// Both Lambdas share this code. The only thing that differs between
// cutRelease and releaseAutomationMergeback is which Action is wired in, and
// the only thing that differs between the prod and test variants is
// configuration -- which is the whole argument for collapsing four functions
// into one artifact (docs/DESIGN-PROPOSALS.md #1).
package handler

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/config"
	"github.com/vamsiatluri/releasebot-migration/internal/events"
	"github.com/vamsiatluri/releasebot-migration/internal/jira"
	"github.com/vamsiatluri/releasebot-migration/internal/obs"
	"github.com/vamsiatluri/releasebot-migration/internal/release"
	"github.com/vamsiatluri/releasebot-migration/internal/slackclient"
	"github.com/vamsiatluri/releasebot-migration/internal/slackverify"
)

// Stamped at build time via -ldflags. The defaults make an unstamped or local
// build obvious rather than letting it silently report someone else's commit.
var (
	BuildCommit = "unstamped"
	BuildTime   = "unknown"
)

type Action string

const (
	ActionCut       Action = "cut"
	ActionMergeback Action = "mergeback"
)

type Handler struct {
	Action Action
	Cfg    config.Config
	Svc    *release.Service
	Slack  *slackclient.Client
	Log    *obs.Logger
	Now    func() time.Time
	// AckOnly returns the Slack 3-second ACK and finishes the work
	// asynchronously. False keeps the original synchronous behaviour.
	AckOnly bool
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Handle is the entry point for one API Gateway proxy request.
func (h *Handler) Handle(ctx context.Context, req events.APIGatewayProxyRequest) events.APIGatewayProxyResponse {
	start := h.now()
	log := h.Log.WithRequestID(req.RequestContext.RequestID).
		With("action", string(h.Action)).
		With("stage", req.RequestContext.Stage).
		With("path", req.Path)
	defer log.Timed("releasebot.handler.duration_ms", start, "action:"+string(h.Action))

	body, err := rawBody(req)
	if err != nil {
		log.Error("request body could not be decoded")
		return events.Text(400, "bad request body")
	}

	// Health check: the deploy workflow and the parallel-run validation both
	// need a way to prove a function is alive that does not cut a release.
	//
	// It reports the BUILD it is running, not merely that it is up. "Is it
	// alive" and "is it running the code I just shipped" are different
	// questions, and a deploy pipeline needs the second one -- a green deploy
	// against a stale artifact looks identical to a good one otherwise.
	if strings.HasSuffix(req.Path, "/health") || req.QueryStringParameters["health"] == "1" {
		// configSource and configPath are reported on purpose. A fallback to
		// environment variables is survivable but must never be SILENT -- a
		// degraded config that looks healthy is worse than a loud failure.
		return events.JSON(200, fmt.Sprintf(
			`{"ok":true,"env":%q,"action":%q,"stage":%q,"commit":%q,"built":%q,`+
				`"configSource":%q,"configPath":%q,"defaultBranch":%q}`,
			h.Cfg.Env, h.Action, req.RequestContext.Stage, BuildCommit, BuildTime,
			h.Cfg.ConfigSource, h.Cfg.ConfigPath, h.Cfg.DefaultBranch))
	}

	switch {
	case req.Header(jira.HeaderSecret) != "":
		return h.handleJira(ctx, log, req, body)
	default:
		return h.handleSlack(ctx, log, req, body)
	}
}

func (h *Handler) handleSlack(ctx context.Context, log *obs.Logger,
	req events.APIGatewayProxyRequest, body []byte) events.APIGatewayProxyResponse {

	// Verify BEFORE parsing. The HMAC covers the exact bytes Slack sent.
	if err := slackverify.Verify(
		h.Cfg.SlackSigningSecret,
		req.Header(slackverify.HeaderSignature),
		req.Header(slackverify.HeaderTimestamp),
		body, h.now(),
	); err != nil {
		log.With("reason", err.Error()).Warn("rejected unsigned or stale Slack request")
		log.Count("releasebot.auth.rejected", 1, "source:slack")
		// 401 and nothing else. Echoing why is a free oracle for an attacker.
		return events.Text(401, "unauthorized")
	}

	form, err := url.ParseQuery(string(body))
	if err != nil {
		return events.Text(400, "could not parse slash command payload")
	}
	cmd, err := release.ParseCommand(form.Get("text"), os.Getenv("DEFAULT_OWNER"))
	if err != nil {
		// 200 with a message: a non-2xx makes Slack show its own generic
		// failure and the user never sees the actual usage error.
		return slackEphemeral(fmt.Sprintf(":warning: %v", err))
	}
	cmd.Actor = form.Get("user_name")
	cmd.Channel = form.Get("channel_id")
	responseURL := form.Get("response_url")

	log = log.With("repo", cmd.Repo).With("version", cmd.Version).With("actor", cmd.Actor)

	if h.AckOnly {
		// Slack hangs up at 3 seconds. Acknowledge now, finish on the
		// response_url. See DESIGN-PROPOSALS #3 for why the goroutine is a
		// placeholder for a real async invoke.
		go h.finish(context.WithoutCancel(ctx), log, cmd, responseURL)
		return slackEphemeral(fmt.Sprintf(":hourglass_flowing_sand: Working on `%s %s`...",
			cmd.Repo, cmd.Version))
	}

	text, err := h.run(ctx, cmd)
	if err != nil {
		log.With("error", err.Error()).Error("release action failed")
		log.Count("releasebot.action.failure", 1, "action:"+string(h.Action))
		return slackEphemeral(":x: " + err.Error())
	}
	return slackInChannel(text)
}

func (h *Handler) handleJira(ctx context.Context, log *obs.Logger,
	req events.APIGatewayProxyRequest, body []byte) events.APIGatewayProxyResponse {

	if err := jira.VerifySecret(os.Getenv("JIRA_WEBHOOK_SECRET"), req.Header(jira.HeaderSecret)); err != nil {
		log.With("reason", err.Error()).Warn("rejected Jira webhook")
		log.Count("releasebot.auth.rejected", 1, "source:jira")
		return events.Text(401, "unauthorized")
	}
	ev, err := jira.Parse(body)
	if err != nil {
		return events.Text(400, err.Error())
	}
	version := ev.FixVersion()
	if version == "" {
		// 200: a Jira webhook that gets a non-2xx is retried, and eventually
		// disabled by Jira. "Not for me" is not an error.
		log.With("issue", ev.Issue.Key).Info("jira webhook carried no fixVersion, ignoring")
		return events.JSON(200, `{"ignored":"no fixVersion"}`)
	}
	cmd := release.Command{
		Repo:    os.Getenv("DEFAULT_OWNER") + "/" + strings.ToLower(ev.Issue.Fields.Project.Key),
		Version: version,
		Actor:   ev.User.DisplayName,
	}
	log = log.With("issue", ev.Issue.Key).With("version", version)

	text, err := h.run(ctx, cmd)
	if err != nil {
		log.With("error", err.Error()).Error("release action failed from jira webhook")
		return events.JSON(500, `{"ok":false}`)
	}
	if ch := os.Getenv("SLACK_CHANNEL"); ch != "" {
		if err := h.Slack.PostMessage(ctx, ch, text); err != nil {
			log.With("error", err.Error()).Warn("could not announce to Slack")
		}
	}
	return events.JSON(200, `{"ok":true}`)
}

// finish runs the action and reports on the Slack response_url.
func (h *Handler) finish(ctx context.Context, log *obs.Logger, cmd release.Command, responseURL string) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	text, err := h.run(ctx, cmd)
	if err != nil {
		log.With("error", err.Error()).Error("async release action failed")
		log.Count("releasebot.action.failure", 1, "action:"+string(h.Action))
		_ = h.Slack.Respond(ctx, responseURL, ":x: "+err.Error(), false)
		return
	}
	if err := h.Slack.Respond(ctx, responseURL, text, true); err != nil {
		log.With("error", err.Error()).Error("could not deliver result to Slack")
	}
}

func (h *Handler) run(ctx context.Context, cmd release.Command) (string, error) {
	switch h.Action {
	case ActionCut:
		res, err := h.Svc.Cut(ctx, cmd)
		if err != nil {
			return "", err
		}
		return res.SlackSummary(h.Cfg.Env), nil
	case ActionMergeback:
		res, err := h.Svc.Mergeback(ctx, cmd)
		if err != nil {
			return "", err
		}
		return res.SlackSummary(h.Cfg.Env), nil
	default:
		return "", errors.New("handler: no action configured")
	}
}

func slackEphemeral(text string) events.APIGatewayProxyResponse {
	return events.JSON(200, jsonSlack("ephemeral", text))
}

func slackInChannel(text string) events.APIGatewayProxyResponse {
	return events.JSON(200, jsonSlack("in_channel", text))
}

func jsonSlack(kind, text string) string {
	return `{"response_type":"` + kind + `","text":` + strconv.Quote(text) + `}`
}

func rawBody(req events.APIGatewayProxyRequest) ([]byte, error) {
	if !req.IsBase64Encoded {
		return []byte(req.Body), nil
	}
	return base64.StdEncoding.DecodeString(req.Body)
}
