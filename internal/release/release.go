// Package release holds ReleaseBot's actual behaviour: cut a release branch,
// and merge a release branch back to the trunk.
//
// The HTTP/Lambda plumbing lives elsewhere on purpose. The migration SOW says
// "preserve existing function names and application behavior unless a change is
// required and approved" -- so behaviour is the thing that must be provably
// unchanged across the account move. Keeping it in a package with no AWS
// imports means it can be pinned by tests that run identically against the dev
// account's artifact and the production account's artifact.
package release

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/vamsiatluri/releasebot-migration/internal/githubclient"
	"github.com/vamsiatluri/releasebot-migration/internal/ledger"
	"github.com/vamsiatluri/releasebot-migration/internal/obs"
)

// semver is deliberately strict. "cut news-app 5.4" creating a branch called
// release/5.4 that nobody expects is worse than an error message.
var (
	semver   = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	repoName = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

type Command struct {
	Repo    string // owner/name
	Version string // 5.4.0
	Actor   string // slack user who asked
	Channel string
	// Which door this arrived through. Recorded on the ledger row, because
	// "a human typed this" and "a Jira transition fired this" are different
	// facts about a release and only one of them had a person look at it.
	Source string // slack | jira | api
}

type CutResult struct {
	Repo       string
	Branch     string
	SHA        string
	Created    bool // false means the branch already existed at the same SHA
	BaseBranch string
}

type MergebackResult struct {
	Repo    string
	Branch  string
	PRURL   string
	PRState string
	Opened  bool
}

type Service struct {
	GH            *githubclient.Client
	Log           *obs.Logger
	DefaultBranch string
	BranchPrefix  string

	// ── the release record ──────────────────────────────────────────────────
	Env        string // prod | test, for the ledger partition
	Properties string // JSON repo->brand map, from Parameter Store
	DryRun     bool   // recorded on the row; a row that hides it is a lie

	// Record is injected so the ledger is a dependency, not a hard-wired call.
	// Nil means "do not record", which is what every unit test and the local
	// harness want -- and what a function whose role lacks PutItem gets
	// without the release failing.
	Record func(ctx context.Context, e ledger.Entry) error
}

func NewService(gh *githubclient.Client, log *obs.Logger, defaultBranch string) *Service {
	return &Service{GH: gh, Log: log, DefaultBranch: defaultBranch, BranchPrefix: "release/"}
}

// ParseCommand reads the `text` field of a Slack slash command:
//
//	/cut news-app 5.4.0          -> owner defaults from DEFAULT_OWNER
//	/cut msnbc/news-app 5.4.0
func ParseCommand(text, defaultOwner string) (Command, error) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) < 2 {
		return Command{}, fmt.Errorf("usage: <repo> <version>  (e.g. news-app 5.4.0)")
	}
	repo := fields[0]
	if !strings.Contains(repo, "/") {
		if defaultOwner == "" {
			return Command{}, fmt.Errorf("repo %q has no owner and DEFAULT_OWNER is unset", repo)
		}
		repo = defaultOwner + "/" + repo
	}
	if !repoName.MatchString(repo) {
		return Command{}, fmt.Errorf("%q is not a valid owner/repo", repo)
	}
	version := strings.TrimPrefix(fields[1], "v")
	if !semver.MatchString(version) {
		return Command{}, fmt.Errorf("%q is not a valid semantic version (want 5.4.0)", fields[1])
	}
	return Command{Repo: repo, Version: version}, nil
}

func (s *Service) branchFor(version string) string { return s.BranchPrefix + version }

// Cut creates the release branch at the current tip of the trunk.
func (s *Service) Cut(ctx context.Context, cmd Command) (CutResult, error) {
	branch := s.branchFor(cmd.Version)
	log := s.Log.With("repo", cmd.Repo).With("branch", branch).With("actor", cmd.Actor)

	sha, err := s.GH.HeadSHA(ctx, cmd.Repo, s.DefaultBranch)
	if err != nil {
		log.Error("cut: could not resolve trunk head")
		return CutResult{}, fmt.Errorf("resolving %s@%s: %w", cmd.Repo, s.DefaultBranch, err)
	}

	created, err := s.GH.CreateBranch(ctx, cmd.Repo, branch, sha)
	if err != nil {
		log.With("sha", sha).Error("cut: branch creation failed")
		return CutResult{}, err
	}
	if !created {
		log.With("sha", sha).Warn("cut: branch already existed at the same commit, treating as success")
	}
	// ⚠️ dry_run ON THE ACTION LINE, not only on the INIT line. A dry run
	// logged "cut: release branch ready  created=true" identically to a real
	// cut, and the only way to tell them apart was to correlate with a
	// separate startup line in the same stream. During an incident that log
	// says a branch exists when none does.
	log.With("sha", sha).With("created", created).
		With("dry_run", fmt.Sprintf("%t", s.DryRun)).
		Info("cut: release branch ready")
	log.Count("releasebot.cut.success", 1, "repo:"+cmd.Repo)

	s.record(ctx, log, cmd, "cut", branch, sha, "")

	return CutResult{Repo: cmd.Repo, Branch: branch, SHA: sha, Created: created,
		BaseBranch: s.DefaultBranch}, nil
}

// Mergeback opens the release/<version> -> trunk pull request.
func (s *Service) Mergeback(ctx context.Context, cmd Command) (MergebackResult, error) {
	branch := s.branchFor(cmd.Version)
	log := s.Log.With("repo", cmd.Repo).With("branch", branch).With("actor", cmd.Actor)

	// Fail loudly if the branch is not there. Opening a PR from a missing head
	// yields a 422 whose message does not say "that branch does not exist".
	if _, err := s.GH.HeadSHA(ctx, cmd.Repo, branch); err != nil {
		log.Error("mergeback: release branch not found")
		return MergebackResult{}, fmt.Errorf("release branch %s does not exist in %s", branch, cmd.Repo)
	}

	title := fmt.Sprintf("Mergeback: %s into %s", branch, s.DefaultBranch)
	body := fmt.Sprintf(
		"Opened by ReleaseBot on behalf of %s.\n\nMerges the %s release branch back into `%s`.",
		orUnknown(cmd.Actor), cmd.Version, s.DefaultBranch)

	pr, opened, err := s.GH.OpenPR(ctx, cmd.Repo, branch, s.DefaultBranch, title, body)
	if err != nil {
		log.Error("mergeback: opening pull request failed")
		return MergebackResult{}, err
	}
	// Same reason as Cut: a dry run must not log identically to a real one.
	log.With("pr", pr.HTMLURL).With("opened", opened).
		With("dry_run", fmt.Sprintf("%t", s.DryRun)).
		Info("mergeback: pull request ready")

	s.record(ctx, log, cmd, "mergeback", branch, "", pr.HTMLURL)
	log.Count("releasebot.mergeback.success", 1, "repo:"+cmd.Repo)

	return MergebackResult{Repo: cmd.Repo, Branch: branch, PRURL: pr.HTMLURL,
		PRState: pr.State, Opened: opened}, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown user"
	}
	return s
}

// SlackSummary renders the message posted back on the response_url.
func (r CutResult) SlackSummary(env string) string {
	verb := "Cut"
	if !r.Created {
		verb = "Confirmed (already existed)"
	}
	return fmt.Sprintf("%s `%s` in `%s` at `%s` off `%s`  _[%s]_",
		verb, r.Branch, r.Repo, shortSHA(r.SHA), r.BaseBranch, env)
}

func (r MergebackResult) SlackSummary(env string) string {
	verb := "Opened"
	if !r.Opened {
		verb = "Reused open"
	}
	return fmt.Sprintf("%s mergeback PR for `%s` in `%s`: %s  _[%s]_",
		verb, r.Branch, r.Repo, r.PRURL, env)
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// record appends one row to the release ledger.
//
// ⚠️ IT MUST NEVER FAIL THE RELEASE. The branch is already cut, the PR is
// already open; failing here would report failure for work that succeeded, and
// the operator's next move is to run it again. Every error is logged and
// swallowed -- but logged, because a ledger that silently stops recording is
// worse than one that was never built.
func (s *Service) record(ctx context.Context, log *obs.Logger,
	cmd Command, action, branch, sha, prURL string) {

	if s.Record == nil {
		return
	}

	e := ledger.Entry{
		Env: s.Env, Action: action, Repo: cmd.Repo,
		Property: ledger.Property(s.Properties, cmd.Repo),
		Version:  cmd.Version, Branch: branch, SHA: sha,
		Actor: cmd.Actor, Source: cmd.Source, URL: prURL, DryRun: s.DryRun,
	}

	// What is IN this release? Asking GitHub for trunk..release-branch is the
	// only honest answer -- a ticket list assembled from anything else is a
	// guess. A failure to answer leaves the fields empty rather than
	// fabricating: an unknown changeset must not render as an empty one.
	if ch, err := s.GH.Compare(ctx, cmd.Repo, s.DefaultBranch, branch); err == nil {
		e.Commits = ch.Commits
		e.Issues = ledger.Issues(append(ch.Messages, cmd.Version)...)
	} else {
		log.With("error", err.Error()).Warn("ledger: could not read the changeset")
	}

	if err := s.Record(ctx, e); err != nil {
		log.With("error", err.Error()).Warn("ledger: row not written")
		log.Count("releasebot.ledger.write_failed", 1, "action:"+action)
	}
}
