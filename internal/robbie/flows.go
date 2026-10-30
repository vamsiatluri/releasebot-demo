package robbie

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/githubclient"
	"github.com/vamsiatluri/releasebot-migration/internal/jiraclient"
	"github.com/vamsiatluri/releasebot-migration/internal/obs"
	"github.com/vamsiatluri/releasebot-migration/internal/rarc"
)

// flow is one command against one project.
type flow struct {
	b   *Bot
	o   out
	p   *rarc.Project
	log *obs.Logger
	ctx context.Context
}

func (f *flow) repo() string { return f.p.FullRepo() }

// ── shared pieces ───────────────────────────────────────────────────────────

// currentVersion reads the version from the first version file on the release
// branch. That file, not the tag list, is the source of truth for "what is
// released now".
func (f *flow) currentVersion() (Semver, error) {
	if len(f.p.FilesWithVersion) == 0 {
		return Semver{}, errors.New("no files containing the project version are configured (filesWithVersion)")
	}
	file := f.p.FilesWithVersion[0]
	content, err := f.b.GH.GetFile(f.ctx, f.repo(), file, f.p.ReleaseBranch)
	if err != nil {
		return Semver{}, fmt.Errorf("reading %s on %s: %w", file, f.p.ReleaseBranch, err)
	}
	raw, err := ReadVersion(content)
	if err != nil {
		return Semver{}, fmt.Errorf("%s: %w", file, err)
	}
	return ParseSemver(raw)
}

// changesSince lists commits on the release branch after tag. A missing tag
// (first release, or a tag nobody pushed) is not fatal: there is just no
// changelog to show.
func (f *flow) changesSince(tag string) []githubclient.Commit {
	commits, err := f.b.GH.CompareCommits(f.ctx, f.repo(), tag, f.p.ReleaseBranch)
	if err != nil {
		f.log.With("tag", tag).With("error", err.Error()).Warn("could not compare the release branch with the last tag")
		return nil
	}
	return commits
}

// visible drops the bot's own merge commits and commits whose message starts
// with a configured filter prefix.
func (f *flow) visible(commits []githubclient.Commit) []githubclient.Commit {
	var out []githubclient.Commit
	for _, c := range commits {
		if strings.HasPrefix(c.Title, "Merge "+f.p.DefaultBranch) || strings.Contains(c.Title, "Merge develop") {
			continue
		}
		skip := false
		for _, pre := range f.p.FilterCommitMessagePrefixes {
			if pre != "" && strings.HasPrefix(c.Title, pre) {
				skip = true
			}
		}
		if !skip {
			out = append(out, c)
		}
	}
	return out
}

func changelog(commits []githubclient.Commit) string {
	var b strings.Builder
	for _, c := range commits {
		sha := c.SHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		fmt.Fprintf(&b, "%s -- %s -- %s\n", sha, c.Author, c.Title)
	}
	return b.String()
}

// issueKeys extracts Jira keys from commit messages, then (when the project
// has filterIssuesQuery) keeps only those Jira says match it.
func (f *flow) issueKeys(commits []githubclient.Commit) []string {
	var texts []string
	for _, c := range commits {
		texts = append(texts, c.Message, c.Title)
	}
	keys, err := IssueKeys(f.b.Cfg.IssueIDFormat(f.p), texts...)
	if err != nil {
		f.log.With("error", err.Error()).Error("bad issueIDFormat")
		return nil
	}
	if len(keys) == 0 || strings.TrimSpace(f.p.FilterIssuesQuery) == "" {
		return keys
	}
	jql := fmt.Sprintf("key in (%s) AND %s", strings.Join(keys, ","), f.p.FilterIssuesQuery)
	found, err := f.b.Jira.SearchJQL(f.ctx, jql, []string{"key"})
	if err != nil {
		f.log.With("error", err.Error()).Warn("filterIssuesQuery search failed; keeping every key found")
		return keys
	}
	keep := map[string]bool{}
	for _, i := range found {
		keep[strings.ToUpper(i.Key)] = true
	}
	var out []string
	for _, k := range keys {
		if keep[k] {
			out = append(out, k)
		}
	}
	return out
}

// issueTypes looks up issue types only when a board's transitions depend on them.
func (f *flow) issueTypes(keys []string) map[string]string {
	need := false
	for _, k := range keys {
		if f.b.Cfg.BoardHasIssueTypes(k) {
			need = true
		}
	}
	types := map[string]string{}
	if !need {
		return types
	}
	found, err := f.b.Jira.SearchJQL(f.ctx, "key in ("+strings.Join(keys, ",")+")", []string{"issuetype"})
	if err != nil {
		f.log.With("error", err.Error()).Warn("could not read issue types; using board defaults")
		return types
	}
	for _, i := range found {
		types[strings.ToUpper(i.Key)] = i.FieldString("issuetype")
	}
	return types
}

// transition moves each key with the transition id the config resolves for it.
func (f *flow) transition(keys []string, done bool) (moved []string) {
	if len(keys) == 0 {
		return nil
	}
	types := f.issueTypes(keys)
	for _, k := range keys {
		id := f.b.Cfg.QAState(k, types[k])
		what := "QA"
		if done {
			id, what = f.b.Cfg.DoneState(k, types[k]), "done"
		}
		if id == "" || id == "0" {
			f.log.With("issue", k).Warn("no " + what + " transition configured; issue left as is")
			continue
		}
		if err := f.b.Jira.Transition(f.ctx, k, id); err != nil {
			f.log.With("issue", k).With("transition", id).With("error", err.Error()).Warn("transition failed")
			continue
		}
		moved = append(moved, k)
	}
	if len(moved) > 0 {
		f.log.With("issues", strings.Join(moved, ",")).Info("moved issues to " + map[bool]string{true: "done", false: "QA"}[done])
	}
	return moved
}

func (f *flow) jiraLinkBase() string {
	if f.b.Cfg.Jira.BaseURL != "" {
		return strings.TrimRight(f.b.Cfg.Jira.BaseURL, "/")
	}
	return f.b.Jira.Base()
}

// ReleaseNotes is the GitHub release body and the Jira ticket's notes. Its FIRST
// LINE is a contract: the deploy workflow reads the previous version out of the
// /compare/vOLD...vNEW link.
func ReleaseNotes(repo string, old, new Semver, commits []githubclient.Commit, jiraBase string, keys []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s - https://github.com/%s/compare/%s...%s\n\n", new.Tag(), repo, old.Tag(), new.Tag())
	b.WriteString(strings.ReplaceAll(changelog(commits), "`", ""))
	if len(keys) > 0 {
		b.WriteString("\n" + JiraIDListURL(jiraBase, keys) + "\n")
	}
	return b.String()
}

// ── precut ──────────────────────────────────────────────────────────────────

func (f *flow) readyPRs(label string) ([]githubclient.PRInfo, error) {
	prs, err := f.b.GH.ListOpenPRs(f.ctx, f.repo(), f.p.DefaultBranch)
	if err != nil {
		return nil, err
	}
	var out []githubclient.PRInfo
	for _, pr := range prs {
		if !hasLabel(pr.Labels, label) && !strings.EqualFold(label, "none") {
			continue
		}
		excluded := false
		for _, ex := range f.p.ExcludeLabels {
			if hasLabel(pr.Labels, ex) {
				excluded = true
			}
		}
		if excluded {
			continue
		}
		if need := int(f.p.ApprovalsNeeded); need > 0 {
			n, err := f.b.GH.Approvals(f.ctx, f.repo(), pr.Number)
			if err != nil {
				f.log.With("pr", pr.Number).With("error", err.Error()).Warn("could not read reviews")
				continue
			}
			if n < need {
				continue
			}
		}
		out = append(out, pr)
	}
	return out, nil
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func (f *flow) precut() {
	f.o.Original("Starting precut")
	ready, err := f.readyPRs(f.b.Cfg.GitHub.MergeLabel)
	switch {
	case err != nil:
		f.o.Original("Could not determine which pull requests are ready to merge: %v", err)
	case len(ready) == 0:
		f.o.Original("No pull requests ready to merge")
	default:
		var b strings.Builder
		for _, pr := range ready {
			fmt.Fprintf(&b, "PR #%d by %s: %s\n", pr.Number, pr.Author, pr.Title)
		}
		f.o.Original("Pull requests ready to merge: ```%s```", b.String())
	}
	commits, err := f.b.GH.CompareCommits(f.ctx, f.repo(), f.p.ReleaseBranch, f.p.DefaultBranch)
	if err != nil {
		f.log.With("error", err.Error()).Warn("compare failed")
	}
	if c := f.visible(commits); len(c) == 0 {
		f.o.Original("Nothing new in the default branch")
	} else {
		f.o.Original("Changes in %s branch: ```%s```", f.p.DefaultBranch, changelog(c))
	}
	f.o.Done("Precut complete")
}

// ── cut / recut ─────────────────────────────────────────────────────────────

type mergeFailure struct {
	pr     githubclient.PRInfo
	reason string
}

// waitMergeable polls GitHub's mergeable flag, which is computed lazily and is
// briefly unknown after every merge into the same base.
func (f *flow) waitMergeable(number int, tries int) (bool, string) {
	state := ""
	for i := 0; i < tries; i++ {
		ok, st, err := f.b.GH.PRMergeable(f.ctx, f.repo(), number)
		if err == nil {
			state = st
			if ok {
				return true, st
			}
			if st == "dirty" || st == "blocked" {
				return false, st
			}
		}
		f.b.sleep(2 * time.Second)
	}
	return false, state
}

func (f *flow) mergeReady(label string) (merged int, failures []mergeFailure) {
	ready, err := f.readyPRs(label)
	if err != nil {
		f.o.Original("Could not list pull requests: %v", err)
		return 0, nil
	}
	for _, pr := range ready {
		if ok, st := f.waitMergeable(pr.Number, 5); !ok {
			failures = append(failures, mergeFailure{pr, "not mergeable (state: " + orDash(st) + ")"})
			continue
		}
		title := fmt.Sprintf("%s (#%d)", pr.Title, pr.Number)
		if err := f.b.GH.MergePR(f.ctx, f.repo(), pr.Number, "squash", title); err != nil {
			failures = append(failures, mergeFailure{pr, err.Error()})
			continue
		}
		merged++
		f.log.With("pr", pr.Number).Info("merged PR into the default branch")
	}
	return merged, failures
}

func orDash(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (f *flow) cut(recut bool) {
	name, label := "cut", f.b.Cfg.GitHub.MergeLabel
	if recut {
		name, label = "recut", f.b.Cfg.GitHub.RecutLabel
	}
	f.o.Original("Starting %s", name)

	merged, failures := f.mergeReady(label)
	if len(failures) > 0 {
		var b strings.Builder
		noun := "PRs"
		if len(failures) == 1 {
			noun = "PR"
		}
		fmt.Fprintf(&b, "<https://github.com/%s|%s> — *%d %s failed to merge:*\n", f.repo(), f.repo(), len(failures), noun)
		for _, m := range failures {
			fmt.Fprintf(&b, "• <%s|#%d> %s — by %s\n    ◦ %s\n", m.pr.HTMLURL, m.pr.Number, m.pr.Title, m.pr.Author, m.reason)
		}
		f.o.Team(b.String())
	}
	if merged == 0 {
		f.o.OriginalNoTag("Did not merge any PRs into the development branch, but merging the development branch into the release branch to cut any features already on it")
	}

	_, didMerge, err := f.b.GH.MergeBranches(f.ctx, f.repo(), f.p.ReleaseBranch, f.p.DefaultBranch,
		fmt.Sprintf("Merge %s into %s", f.p.DefaultBranch, f.p.ReleaseBranch))
	if err != nil {
		f.o.Done(":rotating_light: There was an error merging the default branch (`%s`) into the release branch (`%s`).\nError message: `%v`.",
			f.p.DefaultBranch, f.p.ReleaseBranch, err)
		return
	}
	if !didMerge {
		f.log.Info("release branch already contains the default branch")
	}

	var commits []githubclient.Commit
	if v, err := f.currentVersion(); err == nil {
		commits = f.changesSince(v.Tag())
	} else {
		f.log.With("error", err.Error()).Warn("no current version; changelog left empty")
	}
	keys := f.issueKeys(commits)

	var msg strings.Builder
	for _, m := range f.p.Slack.QAMentions {
		if m != "" {
			msg.WriteString("<@" + m + "> ")
		}
	}
	for _, m := range f.p.Slack.QATeamMention {
		if m != "" {
			msg.WriteString("<!subteam^" + m + "> ")
		}
	}
	fmt.Fprintf(&msg, "a new cut for the latest *%s* is building on staging now. changelog: ```%s```",
		f.p.Name, orNone(changelog(f.visible(commits))))
	if len(keys) > 0 {
		fmt.Fprintf(&msg, "\n<%s|View tickets in JIRA>", JiraFilterURL(f.jiraLinkBase(), keys))
	}
	f.o.Team(msg.String())
	f.transition(keys, false)

	f.o.Done("%s complete", name)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no changes since the last release)\n"
	}
	return s
}

// ── release ─────────────────────────────────────────────────────────────────

var releaseSteps = []struct{ ok, fail string }{
	{":white_check_mark: Created a GitHub tag", ":x: Failed to create a GitHub tag"},
	{":white_check_mark: Created a GitHub release", ":x: Failed to create a GitHub release"},
	{":white_check_mark: Created a JIRA release ticket", ":x: Failed to create a JIRA release ticket"},
}

func stepReport(cause error, done, failed int) string {
	var b strings.Builder
	b.WriteString(cause.Error() + "\n")
	for i := 0; i < done; i++ {
		b.WriteString(releaseSteps[i].ok + "\n")
	}
	b.WriteString(releaseSteps[failed].fail)
	return b.String()
}

func (f *flow) release(bump string, envChange bool) {
	old, err := f.currentVersion()
	if err != nil {
		f.o.Done("There was an error in the version workflow: %v", err)
		return
	}
	next, err := old.Bump(bump)
	if err != nil {
		f.o.Done("There was an error in the version workflow: %v", err)
		return
	}
	msg := fmt.Sprintf("Starting *%s* release for %s, old version: *%s* new version: *%s*", bump, f.p.Name, old, next)
	if envChange {
		msg += ", Environment Variable Change: *Yes*"
	}
	f.o.Original("%s\n`%s`", msg, VersionJump(old, next, bump))
	f.log = f.log.With("old", old.String()).With("new", next.String())

	commits := f.visible(f.changesSince(old.Tag()))
	keys := f.issueKeys(commits)

	fail := func(err error, done, failed int) {
		f.log.With("error", err.Error()).Error("release failed")
		f.o.Done("There was an error in the release tag workflow: %s", stepReport(err, done, failed))
	}

	sha, err := f.bumpVersionFiles(next)
	if err != nil {
		fail(err, 0, 0)
		return
	}
	if err := f.b.GH.CreateTag(f.ctx, f.repo(), next.Tag(), sha, "Release version "+next.String()); err != nil {
		fail(err, 0, 0)
		return
	}
	notes := ReleaseNotes(f.repo(), old, next, commits, f.jiraLinkBase(), keys)
	rel, err := f.b.GH.CreateRelease(f.ctx, f.repo(), next.Tag(), sha, notes)
	if err != nil {
		fail(err, 1, 1)
		return
	}
	env := envChange || f.detectEnvVarChange(keys)
	ticketKey, ticketURL, err := f.createTicket(next.Tag(), notes, env)
	if err != nil {
		fail(err, 2, 2)
		return
	}

	ready := fmt.Sprintf("We're ready for a *%s* release %s. Tagged changes at `%s`", f.p.Name, ticketURL, next.Tag())
	if m := f.b.Cfg.Slack.NosTeamMention; m != "" {
		ready = m + " " + ready
	}
	f.o.Release(ready)

	if topic := f.p.Slack.ChannelTopic; topic != "" && f.p.Slack.TeamChannel != "" {
		t := strings.ReplaceAll(topic, "{version}", next.String())
		if err := f.b.Slack.SetTopic(f.ctx, f.p.Slack.TeamChannel, t); err != nil {
			f.log.With("error", err.Error()).Warn("could not update the channel topic")
		}
	}
	compare := fmt.Sprintf("https://github.com/%s/compare/%s...%s", f.repo(), old.Tag(), next.Tag())
	f.o.Done("Release *%s* complete\n<%s|%s...%s> · <%s|GitHub release> · <%s|%s>",
		next, compare, old.Tag(), next.Tag(), rel.HTMLURL, ticketURL, ticketKey)
}

// bumpVersionFiles writes the new version into every configured version file
// that exists on the release branch, as one commit, and returns its SHA.
func (f *flow) bumpVersionFiles(v Semver) (string, error) {
	files := map[string]string{}
	for _, path := range f.p.FilesWithVersion {
		content, err := f.b.GH.GetFile(f.ctx, f.repo(), path, f.p.ReleaseBranch)
		if err != nil {
			if githubclient.IsNotFound(err) {
				f.log.With("file", path).Warn("version file not on the release branch; skipped")
				continue
			}
			return "", err
		}
		updated, err := ReplaceVersion(content, v.String())
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		files[path] = updated
	}
	if len(files) == 0 {
		return "", errors.New("none of the files in filesWithVersion exist on the release branch")
	}
	return f.b.GH.CommitFiles(f.ctx, f.repo(), f.p.ReleaseBranch, files,
		fmt.Sprintf("%s - version bump (created by release bot)", v.Tag()))
}

// detectEnvVarChange: with autoDetectEnvVarChange, a release is flagged when
// any of its issues has the env-var-change field set to Yes.
func (f *flow) detectEnvVarChange(keys []string) bool {
	field := f.b.Cfg.TicketFields(f.p).EnvVarChange
	if !f.p.Jira.AutoDetectEnvVarChange || field == "" || len(keys) == 0 {
		return false
	}
	found, err := f.b.Jira.SearchJQL(f.ctx, "key in ("+strings.Join(keys, ",")+")", []string{field})
	if err != nil {
		f.log.With("error", err.Error()).Warn("env var change auto-detect failed; using the manual flag")
		return false
	}
	for _, i := range found {
		switch strings.ToLower(strings.TrimSpace(i.FieldString(field))) {
		case "yes", "true":
			return true
		}
	}
	return false
}

// TicketFields builds the Jira create-issue fields for a release ticket.
// Exported for tests. skipped lists configured-but-missing field keys.
func TicketFields(c *rarc.Config, p *rarc.Project, accountID, tag, notes string, envChange bool) (fields map[string]any, skipped []string) {
	fields = map[string]any{
		"summary":     p.TitleFor(tag),
		"issuetype":   map[string]string{"id": p.Jira.ReleaseIssueTypeID},
		"description": jiraclient.TextToADF(notes),
	}
	switch {
	case p.Jira.ReleaseIssueProjectID != "":
		fields["project"] = map[string]string{"id": p.Jira.ReleaseIssueProjectID}
	default:
		fields["project"] = map[string]string{"key": p.Jira.ReleaseIssueProjectKey}
	}
	if accountID != "" {
		fields["reporter"] = map[string]string{"accountId": accountID}
	}
	keys := c.TicketFields(p)
	add := func(name, key string, ok bool, value any) {
		if key == "" {
			skipped = append(skipped, name)
			return
		}
		if ok {
			fields[key] = value
		}
	}
	add("risk", keys.Risk, p.Jira.ReleaseIssueDefaultRisk != "", map[string]string{"id": p.Jira.ReleaseIssueDefaultRisk})
	add("platforms", keys.Platforms, p.Jira.Platform != "", []map[string]string{{"id": p.Jira.Platform}})
	add("notes", keys.Notes, true, jiraclient.TextToADF(notes))
	env := "No"
	if envChange {
		env = "Yes"
	}
	add("envVarChange", keys.EnvVarChange, true, map[string]string{"value": env})
	return fields, skipped
}

func (f *flow) createTicket(tag, notes string, envChange bool) (key, link string, err error) {
	fields, skipped := TicketFields(f.b.Cfg, f.p, f.b.Env.JiraAccountID, tag, notes, envChange)
	for _, s := range skipped {
		f.log.With("field", s).Warn("jira.fields." + s + " is not configured; that field is left off the release ticket")
	}
	key, err = f.b.Jira.CreateIssue(f.ctx, fields)
	if err != nil {
		return "", "", err
	}
	link = f.jiraLinkBase() + "/browse/" + key
	f.log.With("ticket", key).Info("created the release ticket")
	return key, link, nil
}

// ── jiraticket ──────────────────────────────────────────────────────────────

func (f *flow) jiraTicket(envChange bool) {
	f.o.Original("Creating JIRA release ticket for the latest GitHub release of %s", f.p.Name)
	rel, err := f.b.GH.LatestRelease(f.ctx, f.repo())
	if err != nil {
		f.o.Done("There was an error creating the JIRA ticket: %v", err)
		return
	}
	notes := rel.Body
	keys, _ := IssueKeys(f.b.Cfg.IssueIDFormat(f.p), notes)
	env := envChange || f.detectEnvVarChange(keys)
	_, link, err := f.createTicket(rel.TagName, notes, env)
	if err != nil {
		f.o.Done("There was an error creating the JIRA ticket: %v", err)
		return
	}
	f.o.Done("Successfully created JIRA release ticket for the latest GitHub release! %s", link)
}

// ── mergeback ───────────────────────────────────────────────────────────────

// ErrInsufficientTags: the mergeback cannot tell which tickets shipped.
var ErrInsufficientTags = errors.New("there aren't two tags to compare")

// previousTag finds the highest v-semver release tag below cur.
func previousTag(tags []githubclient.Tag, cur Semver) (string, bool) {
	var vs []Semver
	hasCur := false
	for _, t := range tags {
		v, err := ParseSemver(t.Name)
		if err != nil || !strings.HasPrefix(t.Name, "v") {
			continue
		}
		if v == cur {
			hasCur = true
			continue
		}
		// Pre-release tags are not releases: skipping them keeps a stray
		// v1.3.10-rc.1 from hiding v1.3.9's tickets.
		if v.Less(cur) && v.Pre == "" {
			vs = append(vs, v)
		}
	}
	if !hasCur || len(vs) == 0 {
		return "", false
	}
	sort.Slice(vs, func(i, j int) bool { return vs[j].Less(vs[i]) })
	return vs[0].Tag(), true
}

// mergeback transitions the release's tickets to done, applies the fixVersion,
// and opens + merges the mergeback PRs. issueKey is the Jira release ticket
// when Jira automation triggered it.
func (f *flow) mergeback(issueKey string) error {
	f.o.Original("Starting mergeback for %s", f.p.Name)
	cur, err := f.currentVersion()
	if err != nil {
		f.o.Done("Mergeback failed: %v", err)
		return err
	}
	keys, err := f.mergebackKeys(issueKey, cur)
	if err != nil {
		if errors.Is(err, ErrInsufficientTags) {
			f.o.Original(":warning: Mergeback for *%s* could not run because %s (the latest version %s is not tagged, or it is the only tag).\n"+
				"• Push the missing tag and run `mergeback %s` again, or\n"+
				"• open the mergeback PRs by hand and move the tickets to Done.", f.p.Name, ErrInsufficientTags, cur.Tag(), f.p.Name)
		}
		return err
	}
	moved := f.transition(keys, true)
	if len(moved) > 0 && f.p.Jira.ProjectID != "" {
		name := fmt.Sprintf("%s %s", cur.Tag(), f.p.Name)
		if _, err := f.b.Jira.EnsureVersion(f.ctx, f.p.Jira.ProjectID, name); err != nil {
			f.log.With("error", err.Error()).Warn("could not create the fixVersion")
		} else {
			for _, k := range moved {
				if err := f.b.Jira.AddFixVersion(f.ctx, k, name); err != nil {
					f.log.With("issue", k).With("error", err.Error()).Warn("could not apply the fixVersion")
				}
			}
		}
	}
	if f.p.MasterBranch != "" {
		if err := f.mergebackPR(f.p.MasterBranch, "Master", f.p.MasterMergebackLabels); err != nil {
			f.o.Done("Mergeback to `%s` failed: %v", f.p.MasterBranch, err)
			return err
		}
	}
	if err := f.mergebackPR(f.p.DefaultBranch, "Develop", f.p.DevelopMergebackLabels); err != nil {
		f.o.Done("Mergeback to `%s` failed: %v", f.p.DefaultBranch, err)
		return err
	}
	f.o.Done("Mergeback complete")
	return nil
}

func (f *flow) mergebackKeys(issueKey string, cur Semver) ([]string, error) {
	if issueKey != "" {
		fieldsWanted := []string{"description"}
		notesField := f.b.Cfg.TicketFields(f.p).Notes
		if notesField != "" {
			fieldsWanted = append(fieldsWanted, notesField)
		}
		issue, err := f.b.Jira.GetIssue(f.ctx, issueKey, fieldsWanted)
		if err == nil {
			notes := issue.FieldString("description")
			if notesField != "" && issue.FieldString(notesField) != "" {
				notes = issue.FieldString(notesField)
			}
			if keys, err := IssueKeys(f.b.Cfg.IssueIDFormat(f.p), notes); err == nil {
				return dropKey(keys, issueKey), nil
			}
		}
		f.log.With("ticket", issueKey).Warn("could not read the release ticket notes; scanning commits instead")
	}
	tags, err := f.b.GH.ListTags(f.ctx, f.repo())
	if err != nil {
		return nil, err
	}
	prev, ok := previousTag(tags, cur)
	if !ok {
		return nil, ErrInsufficientTags
	}
	commits, err := f.b.GH.CompareCommits(f.ctx, f.repo(), prev, cur.Tag())
	if err != nil {
		return nil, err
	}
	return f.issueKeys(commits), nil
}

func dropKey(keys []string, k string) []string {
	var out []string
	for _, x := range keys {
		if !strings.EqualFold(x, k) {
			out = append(out, x)
		}
	}
	return out
}

func (f *flow) mergebackPR(base, label string, labels []string) error {
	title := fmt.Sprintf("Mergeback PR - %s to %s", f.p.ReleaseBranch, base)
	pr, _, err := f.b.GH.OpenPR(f.ctx, f.repo(), f.p.ReleaseBranch, base, title, label+" mergeback PR (opened by release bot)")
	if err != nil {
		var he *githubclient.HTTPError
		if errors.As(err, &he) && he.StatusCode == 422 && strings.Contains(he.Body, "No commits between") {
			f.o.Team(fmt.Sprintf("Nothing to merge back from `%s` to `%s`.", f.p.ReleaseBranch, base))
			return nil
		}
		return err
	}
	if err := f.b.GH.AddLabels(f.ctx, f.repo(), pr.Number, labels); err != nil {
		f.log.With("error", err.Error()).Warn("could not label the mergeback PR")
	}
	ok, st := f.waitMergeable(pr.Number, 10)
	merged := false
	// "blocked" is merged too: the bot's token may bypass branch protection
	// for its own mergebacks, as the production bot's does.
	if ok || st == "blocked" {
		if err := f.b.GH.MergePR(f.ctx, f.repo(), pr.Number, "merge", title); err != nil {
			f.log.With("error", err.Error()).Warn("could not merge the mergeback PR")
		} else {
			merged = true
		}
	}
	if merged {
		f.o.Team(fmt.Sprintf("%s mergeback was automerged, no further action necessary :robot_face: %s", label, pr.HTMLURL))
		return nil
	}
	mention := ""
	if m := f.p.Slack.DevTeamMention; m != "" {
		mention = "<!subteam^" + m + "> "
	}
	f.o.Team(fmt.Sprintf("%sPlease review today's mergebacks:\n%s", mention, pr.HTMLURL))
	return nil
}
