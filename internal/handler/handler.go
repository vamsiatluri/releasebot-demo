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
	"encoding/json"
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
	"github.com/vamsiatluri/releasebot-migration/internal/lambdainvoke"
	"github.com/vamsiatluri/releasebot-migration/internal/obs"
	"github.com/vamsiatluri/releasebot-migration/internal/release"
	"github.com/vamsiatluri/releasebot-migration/internal/secretref"
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
		// ⚠️ secretSource reports the MECHANISM, never a value. Without it
		// there is no way to tell a function whose secrets are SecureString
		// references from one holding them in plaintext env vars, short of
		// reading the secrets -- so "we hardened it" would be unverifiable
		// from outside, which is how a half-finished rollout looks finished.
		return events.JSON(200, fmt.Sprintf(
			`{"ok":true,"env":%q,"action":%q,"stage":%q,"commit":%q,"built":%q,`+
				`"configSource":%q,"configPath":%q,"secretSource":%q,"cutAllowlist":%q,`+
				`"defaultOwner":%q,"defaultBranch":%q,"slackChannel":%q,"datadogService":%q}`,
			h.Cfg.Env, h.Action, req.RequestContext.Stage, BuildCommit, BuildTime,
			h.Cfg.ConfigSource, h.Cfg.ConfigPath, secretSource(), h.Cfg.CutAllowlistState(),
			h.Cfg.DefaultOwner, h.Cfg.DefaultBranch, h.Cfg.SlackChannel, h.Cfg.DatadogService))
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
	cmd, err := release.ParseCommand(form.Get("text"), h.Cfg.DefaultOwner)
	if err != nil {
		// 200 with a message: a non-2xx makes Slack show its own generic
		// failure and the user never sees the actual usage error.
		return slackEphemeral(fmt.Sprintf(":warning: %v", err))
	}
	cmd.Actor = form.Get("user_name")
	cmd.ActorID = form.Get("user_id")
	cmd.Channel = form.Get("channel_id")
	cmd.Source = "slack"
	responseURL := form.Get("response_url")

	log = log.With("repo", cmd.Repo).With("version", cmd.Version).
		With("actor", cmd.Actor).With("actor_id", cmd.ActorID)

	// ── Who may ask for a release ───────────────────────────────────────────
	//
	// The signature proves the request came from our Slack app. It says nothing
	// about WHICH member of the workspace sent it -- so until now any user who
	// could type the slash command could cut a release branch, and the user name
	// was recorded rather than checked. Found by a test that sent a fabricated
	// `user_id=U0DEMO` and had the cut execute.
	//
	// ⚠️ MATCHED ON user_id, NOT user_name. A display name is chosen by its own
	// subject, so an allowlist of names is one rename away from admitting the
	// wrong person -- or locking out the right one.
	//
	// ⛔ EMPTY MEANS UNRESTRICTED HERE, which is the OPPOSITE of the approval
	// allowlist, and the asymmetry is deliberate:
	//   - approving a production deploy with an empty list must admit NOBODY,
	//     because a misconfiguration there is an open door on production;
	//   - asking for a release BRANCH is low-risk, the production gate is the
	//     approval, and defaulting to "nobody" would break an existing
	//     workflow the moment this ships -- on cutover day, in someone else's
	//     account.
	// The cost of that choice is that an unset list is invisible, which is how
	// an optional control becomes one nobody ever turns on. So the state is
	// REPORTED: /health carries `cutAllowlist`, and the dashboard shows it.
	if !h.Cfg.PermittedToCut(cmd.ActorID) {
		log.With("actor_id", cmd.ActorID).Warn("rejected: not on the cut allowlist")
		log.Count("releasebot.auth.rejected", 1, "source:slack-not-allowed")
		return slackEphemeral(fmt.Sprintf(
			":no_entry: You are not permitted to cut releases.\n"+
				"_To permit this person, add `%s` to RELEASEBOT_CUT_ALLOWLIST._", cmd.ActorID))
	}

	if h.AckOnly {
		// Slack hangs up at 3 seconds. Acknowledge now; the work runs in its
		// OWN invocation.
		//
		// ⚠️ This used to be `go h.finish(...)`, and that is a silent
		// data-loss bug on Lambda: the sandbox is frozen the instant the
		// response is returned, so the goroutine survives only if the
		// container happens to be reused. Warm containers finished the work;
		// cold ones dropped it and the user kept the "Working on it..."
		// message forever. See internal/lambdainvoke.
		if err := h.dispatch(ctx, cmd, responseURL); err != nil {
			// ⚠️ FALL BACK TO DOING IT INLINE rather than losing the request.
			// Slack may time out and show its own error, which is bad -- but a
			// release that silently never happens is worse, and the operator
			// has no way to tell the two apart from the Slack side.
			log.With("error", err.Error()).Error("async dispatch failed; running inline")
			log.Count("releasebot.dispatch.fallback", 1, "action:"+string(h.Action))
			text, rerr := h.run(ctx, cmd)
			if rerr != nil {
				return slackEphemeral(":x: " + rerr.Error())
			}
			return slackInChannel(text)
		}
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

	if err := jira.VerifySecret(h.Cfg.JiraWebhookSecret, req.Header(jira.HeaderSecret)); err != nil {
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
		Repo:    h.Cfg.DefaultOwner + "/" + strings.ToLower(ev.Issue.Fields.Project.Key),
		Version: version,
		// No ActorID: a Jira transition has no Slack user. Leaving it EMPTY is
		// the honest record -- filling it with the Jira account id would make
		// one field mean two different namespaces, and a later query for
		// "what did U0123ABCD release" would silently match nothing or, worse,
		// something.
		Actor:  ev.User.DisplayName,
		Source: "jira",
	}
	log = log.With("issue", ev.Issue.Key).With("version", version)

	text, err := h.run(ctx, cmd)
	if err != nil {
		log.With("error", err.Error()).Error("release action failed from jira webhook")
		return events.JSON(500, `{"ok":false}`)
	}
	if ch := h.Cfg.SlackChannel; ch != "" {
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

// ── Asynchronous follow-up work ─────────────────────────────────────────────

// AsyncJob is the payload this function sends to ITSELF to finish a command
// after Slack has been acknowledged.
//
// The `releasebotAsyncJob` marker is what distinguishes it from an API Gateway
// event at the entry point. A shape-based guess ("does it have a `path` field?")
// would be fragile in exactly the direction that matters: a malformed proxy
// event must never be mistaken for a job that performs a release.
type AsyncJob struct {
	Marker      bool            `json:"releasebotAsyncJob"`
	Action      Action          `json:"action"`
	Command     release.Command `json:"command"`
	ResponseURL string          `json:"responseUrl"`
	// The function version that ACCEPTED the command. The job is dispatched to
	// the `live` alias (see internal/lambdainvoke), so a deploy landing in the
	// few milliseconds in between would finish it on different code. Carrying
	// the version makes that detectable instead of invisible.
	AcceptedBy string `json:"acceptedBy,omitempty"`
}

// IsAsyncJob reports whether payload is a self-dispatched job.
func IsAsyncJob(payload []byte) (AsyncJob, bool) {
	var j AsyncJob
	if err := json.Unmarshal(payload, &j); err != nil || !j.Marker {
		return AsyncJob{}, false
	}
	return j, true
}

func (h *Handler) dispatch(ctx context.Context, cmd release.Command, responseURL string) error {
	body, err := json.Marshal(AsyncJob{
		Marker: true, Action: h.Action, Command: cmd, ResponseURL: responseURL,
		AcceptedBy: os.Getenv("AWS_LAMBDA_FUNCTION_VERSION"),
	})
	if err != nil {
		return err
	}
	return lambdainvoke.Self(ctx, body)
}

// HandleAsync runs the work for a self-dispatched job.
//
// ⚠️ The response_url is validated by the Slack client before anything is
// POSTed to it. This payload arrives over lambda:InvokeFunction rather than
// through the signature-checked HTTP path, so its contents are only as
// trustworthy as the IAM policy that permits the invoke -- which is scoped to
// this function invoking itself and nothing else.
func (h *Handler) HandleAsync(ctx context.Context, j AsyncJob) {
	log := h.Log.
		With("action", string(j.Action)).
		With("repo", j.Command.Repo).
		With("version", j.Command.Version).
		With("actor", j.Command.Actor).
		With("async", "true")
	if running := os.Getenv("AWS_LAMBDA_FUNCTION_VERSION"); j.AcceptedBy != "" && running != "" &&
		j.AcceptedBy != running {
		// Not an error -- the job is still valid and still gets done. But a
		// deploy landed between the acknowledgement and this invocation, and
		// that is worth being able to find in a log afterwards.
		log = log.With("accepted_by_version", j.AcceptedBy).With("running_version", running)
		log.Warn("async job is finishing on a different version than accepted it")
	}
	log.Info("async job received")
	h.finish(ctx, log, j.Command, j.ResponseURL)
}

// secretSource reports how the secret-bearing values were supplied, by reading
// the REFERENCES in the environment rather than the resolved values.
//
// ⚠️ It must never report the value, and must never claim more than it knows.
// "reference" means every secret-bearing variable that is set carries a scheme;
// "plaintext" means at least one holds a literal; "unset" means none is set,
// which is the dry-run and harness case and is not a finding.
func secretSource() string {
	keys := []string{"GITHUB_TOKEN", "SLACK_TOKEN", "SLACK_SIGNING_SECRET",
		"JIRA_TOKEN", "JIRA_WEBHOOK_SECRET", "DD_API_KEY"}
	var set, refs int
	for _, k := range keys {
		v := os.Getenv(k)
		if v == "" {
			continue
		}
		set++
		if secretref.IsReference(v) {
			refs++
		}
	}
	switch {
	case set == 0:
		return "unset"
	case refs == set:
		return "reference"
	case refs == 0:
		return "plaintext"
	default:
		return fmt.Sprintf("mixed (%d of %d by reference)", refs, set)
	}
}
