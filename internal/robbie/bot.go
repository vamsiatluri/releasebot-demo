package robbie

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/githubclient"
	"github.com/vamsiatluri/releasebot-migration/internal/jiraclient"
	"github.com/vamsiatluri/releasebot-migration/internal/obs"
	"github.com/vamsiatluri/releasebot-migration/internal/rarc"
)

// GitHub is the slice of GitHub the flows use (*githubclient.Client).
type GitHub interface {
	ListOpenPRs(ctx context.Context, repo, base string) ([]githubclient.PRInfo, error)
	Approvals(ctx context.Context, repo string, number int) (int, error)
	PRMergeable(ctx context.Context, repo string, number int) (bool, string, error)
	MergePR(ctx context.Context, repo string, number int, method, title string) error
	AddLabels(ctx context.Context, repo string, number int, labels []string) error
	MergeBranches(ctx context.Context, repo, base, head, message string) (string, bool, error)
	CompareCommits(ctx context.Context, repo, base, head string) ([]githubclient.Commit, error)
	GetFile(ctx context.Context, repo, path, ref string) (string, error)
	CommitFiles(ctx context.Context, repo, branch string, files map[string]string, message string) (string, error)
	CreateTag(ctx context.Context, repo, tag, sha, message string) error
	CreateRelease(ctx context.Context, repo, tag, sha, body string) (githubclient.Release, error)
	LatestRelease(ctx context.Context, repo string) (githubclient.Release, error)
	ListTags(ctx context.Context, repo string) ([]githubclient.Tag, error)
	OpenPR(ctx context.Context, repo, head, base, title, body string) (*githubclient.PullRequest, bool, error)
}

// Jira is the slice of Jira the flows use (*jiraclient.Client).
type Jira interface {
	CreateIssue(ctx context.Context, fields map[string]any) (string, error)
	Transition(ctx context.Context, key, transitionID string) error
	GetIssue(ctx context.Context, key string, fields []string) (jiraclient.Issue, error)
	SearchJQL(ctx context.Context, jql string, fields []string) ([]jiraclient.Issue, error)
	EnsureVersion(ctx context.Context, projectID, name string) (string, error)
	AddFixVersion(ctx context.Context, key, versionName string) error
	BrowseURL(key string) string
	Base() string
}

// Slack is the slice of Slack the flows use (*slackclient.Client).
type Slack interface {
	Post(ctx context.Context, channel, text, contextLine string) error
	RespondBlocks(ctx context.Context, responseURL, text, contextLine string) error
	SetTopic(ctx context.Context, channel, topic string) error
}

// Bot runs one command at a time against the config.
type Bot struct {
	Cfg   *rarc.Config
	Env   Env
	GH    GitHub
	Jira  Jira
	Slack Slack
	Log   *obs.Logger
	// Version is what `ping` / `version` report (the build commit).
	Version string
	// Sleep is used between mergeable-state polls; tests make it a no-op.
	Sleep func(time.Duration)
}

// Request is who asked, and where to answer.
type Request struct {
	UserID      string
	ChannelID   string
	ResponseURL string // slash commands answer here instead of chat.postMessage
	IsDM        bool
	Text        string // "<@BOT> cut my-app" or "cut my-app"
	RequestID   string
	Source      string // slack | jira | sqs
}

func (b *Bot) sleep(d time.Duration) {
	if b.Sleep != nil {
		b.Sleep(d)
		return
	}
	time.Sleep(d)
}

// out posts on behalf of one request.
type out struct {
	b   *Bot
	ctx context.Context
	req Request
	p   *rarc.Project
}

// tag prefixes the requesting user's mention, except in a DM (where it is the
// only other person in the conversation) or when there is no user.
func (o out) tag(text string) string {
	if o.req.UserID == "" || strings.HasPrefix(o.req.ChannelID, "D") || o.req.IsDM {
		return text
	}
	m := "<@" + o.req.UserID + ">"
	if strings.Contains(text, m) {
		return text
	}
	return m + " " + text
}

func (o out) contextLine() string {
	parts := []string{"account `" + o.b.Env.AccountLabel + "`", "env `" + o.b.Env.Stage + "`"}
	if o.req.RequestID != "" {
		parts = append(parts, "request `"+o.req.RequestID+"`")
	}
	if o.b.Env.DryRun {
		parts = append(parts, "*dry run*")
	}
	return strings.Join(parts, " · ")
}

func (o out) post(channel, text, ctxLine string, toOriginal bool) {
	var err error
	if toOriginal && o.req.ResponseURL != "" {
		err = o.b.Slack.RespondBlocks(o.ctx, o.req.ResponseURL, text, ctxLine)
	} else {
		err = o.b.Slack.Post(o.ctx, channel, text, ctxLine)
	}
	if err != nil {
		o.b.Log.With("channel", channel).With("error", err.Error()).Warn("slack post failed")
	}
}

// Original answers where the command came from (falls back to the project's
// team channel when the request has nowhere to answer, e.g. a Jira webhook).
func (o out) Original(format string, a ...any) {
	ch := o.req.ChannelID
	if ch == "" && o.req.ResponseURL == "" && o.p != nil {
		ch = o.p.Slack.TeamChannel
	}
	o.post(ch, o.tag(fmt.Sprintf(format, a...)), "", true)
}

// OriginalNoTag is Original without the user mention.
func (o out) OriginalNoTag(format string, a ...any) {
	ch := o.req.ChannelID
	if ch == "" && o.req.ResponseURL == "" && o.p != nil {
		ch = o.p.Slack.TeamChannel
	}
	o.post(ch, fmt.Sprintf(format, a...), "", true)
}

// Done is Original with the context line (account, env, request id).
func (o out) Done(format string, a ...any) {
	ch := o.req.ChannelID
	if ch == "" && o.req.ResponseURL == "" && o.p != nil {
		ch = o.p.Slack.TeamChannel
	}
	o.post(ch, o.tag(fmt.Sprintf(format, a...)), o.contextLine(), true)
}

// Team posts to the project's team channel.
func (o out) Team(text string) {
	if o.p == nil || o.p.Slack.TeamChannel == "" {
		o.b.Log.Warn("no team channel configured; message not posted")
		return
	}
	o.post(o.p.Slack.TeamChannel, o.tag(text), "", false)
}

// Release posts to the global release channel.
func (o out) Release(text string) {
	if o.b.Cfg.Slack.ReleaseChannel == "" {
		o.b.Log.Warn("no release channel configured; message not posted")
		return
	}
	o.post(o.b.Cfg.Slack.ReleaseChannel, o.tag(text), "", false)
}

// Run parses and executes one command. It never returns an error: every
// outcome is a Slack message, as for a human typing in a channel.
func (b *Bot) Run(ctx context.Context, req Request) {
	log := b.Log.With("user", req.UserID).With("channel", req.ChannelID).With("request_id", req.RequestID)
	o := out{b: b, ctx: ctx, req: req}

	if req.ChannelID == "" && req.ResponseURL == "" {
		log.Error("request has neither a channel nor a response_url; nowhere to answer")
		return
	}
	args, err := ParseArgs(req.Text)
	if err != nil {
		if req.IsDM {
			o.Original("%s", dmParseError(req.Text))
			return
		}
		o.Done("%s", err.Error())
		log.With("error", err.Error()).Info("could not parse command")
		return
	}
	log = log.With("command", args.Command).With("project", args.Project)
	if req.IsDM && !IsInformational(args.Command) {
		o.Original("%s", dmReleaseCommand(args.Command))
		return
	}
	switch args.Command {
	case CmdPing:
		o.Original("Pong! Release bot version *`%s`*", b.Version)
		return
	case CmdVersion:
		o.Original("Release bot version *`%s`*", b.Version)
		return
	case CmdHelp:
		o.Original("%s", helpText)
		return
	case CmdReleasePlease:
		o.Original("%s", releasePleaseText)
		return
	case CmdProjects:
		o.Original("%s", projectsText(b.Cfg))
		return
	}

	p := b.Cfg.Project(args.Project)
	if p == nil {
		log.Info("no such project")
		o.Original("Could not find project named `%s`", args.Project)
		return
	}
	o.p = p
	log = log.With("project", p.Name)
	log.Info("running command")
	f := &flow{b: b, o: o, p: p, log: log, ctx: ctx}
	switch args.Command {
	case CmdPrecut:
		f.precut()
	case CmdCut:
		f.cut(false)
	case CmdRecut:
		f.cut(true)
	case CmdRelease:
		f.release(args.Bump, args.EnvVarChange)
	case CmdJiraTicket:
		f.jiraTicket(args.EnvVarChange)
	case CmdMergeback:
		_ = f.mergeback("")
	default:
		o.Original("`%s` is not supported by this bot.", args.Command)
	}
}

const dmCommands = "In a DM I can help with `help`, `ping`, `version`, `projects`, and `release-please`."

func dmReleaseCommand(cmd string) string {
	return fmt.Sprintf("`%s` can't be run in a DM — release commands run in a channel so the team can see them.\n%s", cmd, dmCommands)
}

func dmParseError(text string) string {
	t := strings.TrimSpace(mention.ReplaceAllString(text, ""))
	fields := strings.Fields(t)
	if len(fields) == 0 {
		return "I didn't catch a command there. " + dmCommands
	}
	cmd := strings.ToLower(strings.TrimSuffix(fields[0], "."))
	if IsKnown(cmd) && !IsInformational(cmd) {
		return dmReleaseCommand(cmd)
	}
	return fmt.Sprintf("Sorry, I don't know the command `%s`. %s", cmd, dmCommands)
}

func projectsText(c *rarc.Config) string {
	names := c.Names()
	if len(names) == 0 {
		return "No configured projects found."
	}
	var s strings.Builder
	fmt.Fprintf(&s, "*Configured projects (%d):*\n", len(names))
	for _, n := range names {
		fmt.Fprintf(&s, "• `%s`\n", n)
	}
	return strings.TrimSpace(s.String())
}

const helpText = "*Release commands:*\n" +
	"• `precut [project]` — PRs ready to merge, and what is on the default branch but not the release branch\n" +
	"• `cut [project]` — merge PRs labelled ready-to-merge into the default branch, then the default branch into the release branch\n" +
	"• `recut [project]` — the same with the recut label\n" +
	"• `release [project] [patch|minor|major] [hasEnvChanges]` — bump the version, tag, publish a GitHub release, file the Jira release ticket\n" +
	"• `jiraticket [project] [hasEnvChanges]` — file the Jira release ticket for the latest GitHub release\n" +
	"\n*Mergeback:*\n" +
	"• `mergeback [project]` — merge the release branch back (also run by Jira automation)\n" +
	"\n*Bot commands:*\n" +
	"• `ping` · `version` · `projects` · `release-please` · `help`\n" +
	"\n_Informational commands also work in a DM. Release commands must be run in a channel._"

const releasePleaseText = "*release-please* is an open-source release tool from Google, unrelated to this bot. " +
	"It reads Conventional Commit messages, keeps a release PR with the next version and a changelog up to date, " +
	"and tags + publishes a GitHub release when that PR is merged. It does not publish packages or deploy; " +
	"that stays in your CI.\n" +
	"This bot instead cuts releases on demand from Slack using the default/release branch model and files the Jira ticket.\n" +
	"<https://github.com/googleapis/release-please|googleapis/release-please>"
