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
	log.With("sha", sha).With("created", created).Info("cut: release branch ready")
	log.Count("releasebot.cut.success", 1, "repo:"+cmd.Repo)

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
	log.With("pr", pr.HTMLURL).With("opened", opened).Info("mergeback: pull request ready")
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
