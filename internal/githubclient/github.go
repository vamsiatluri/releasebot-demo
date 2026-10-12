// Package githubclient wraps the handful of GitHub REST calls ReleaseBot makes.
//
// Every write is idempotent, which is not decoration: during the parallel-run
// window the dev-account and production-account copies of ReleaseBot are both
// deployed, and the acceptance plan runs real release cycles through them. If
// "create the release branch" is not safe to run twice, a double-delivered
// Slack retry or a stray invoke of the wrong stack corrupts a real release.
package githubclient

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	DryRun  bool
}

func New(baseURL, token string, dryRun bool) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		DryRun:  dryRun,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

type Ref struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Title   string `json:"title"`
}

// HeadSHA resolves a branch to its tip commit.
//
// ⚠️ DRY RUN SHORT-CIRCUITS THE READ TOO, and that was a real bug: only the
// WRITES were dry-run aware, on the reasonable-sounding grounds that a read is
// harmless. It meant a dry run still required the target repository to exist
// and still spent a GitHub API call against it, so a dry run against a
// repository that is not there failed -- with a timeout, which reads as a
// network problem rather than as "dry run is not actually dry".
//
// A dry run that needs the real thing to be present is not a dry run. The
// fake SHA is derived from the inputs so it is stable across calls: a cut and
// the mergeback that follows it agree, which is what makes a dry run useful
// for exercising the whole path.
func (c *Client) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	if c.DryRun {
		h := sha1.Sum([]byte(repo + "@" + branch))
		return hex.EncodeToString(h[:]), nil
	}
	var r Ref
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/git/ref/heads/%s", repo, branch), nil, &r)
	if err != nil {
		return "", err
	}
	if r.Object.SHA == "" {
		return "", fmt.Errorf("github: %s@%s resolved to an empty SHA", repo, branch)
	}
	return r.Object.SHA, nil
}

// CreateBranch creates refs/heads/<name> at sha.
//
// Idempotent by design: GitHub answers 422 "Reference already exists". That is
// treated as success ONLY when the existing ref points at the same SHA. A
// branch of the same name at a different commit is a genuine conflict -- two
// cuts racing, or a re-cut after main moved -- and must not be swallowed.
func (c *Client) CreateBranch(ctx context.Context, repo, name, sha string) (created bool, err error) {
	if c.DryRun {
		return true, nil
	}
	body := map[string]string{"ref": "refs/heads/" + name, "sha": sha}
	err = c.do(ctx, http.MethodPost, "/repos/"+repo+"/git/refs", body, nil)
	if err == nil {
		return true, nil
	}
	var he *HTTPError
	if !asHTTPError(err, &he) || he.StatusCode != http.StatusUnprocessableEntity {
		return false, err
	}
	existing, lookupErr := c.HeadSHA(ctx, repo, name)
	if lookupErr != nil {
		return false, err
	}
	if existing != sha {
		return false, fmt.Errorf(
			"github: branch %s already exists at %s, refusing to re-point it to %s",
			name, short(existing), short(sha))
	}
	return false, nil
}

// FindOpenPR returns an existing open PR for head->base, or nil.
func (c *Client) FindOpenPR(ctx context.Context, repo, head, base string) (*PullRequest, error) {
	// Same rule as HeadSHA: a dry run makes NO call to GitHub, read or write.
	// Leaving the reads live meant "dry run" still needed the repository to
	// exist -- the mergeback path failed here while the cut path (already
	// fixed) succeeded, which is a confusing split to debug.
	//
	// nil means "no open PR", which is the answer that makes the caller go on
	// to the dry-run OpenPR rather than reporting a reused one that is not
	// there.
	if c.DryRun {
		return nil, nil
	}
	owner := repo
	if i := strings.Index(repo, "/"); i > 0 {
		owner = repo[:i]
	}
	var prs []PullRequest
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/pulls?state=open&head=%s:%s&base=%s", repo, owner, head, base),
		nil, &prs)
	if err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

// OpenPR opens a pull request, reusing an open one if it already exists.
func (c *Client) OpenPR(ctx context.Context, repo, head, base, title, body string) (*PullRequest, bool, error) {
	if existing, err := c.FindOpenPR(ctx, repo, head, base); err != nil {
		return nil, false, err
	} else if existing != nil {
		return existing, false, nil
	}
	if c.DryRun {
		return &PullRequest{Number: 0, HTMLURL: "(dry-run)", State: "open", Title: title}, true, nil
	}
	var pr PullRequest
	err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/pulls", map[string]string{
		"title": title, "head": head, "base": base, "body": body,
	}, &pr)
	if err != nil {
		return nil, false, err
	}
	return &pr, true, nil
}

type HTTPError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("github: %s %s -> %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func asHTTPError(err error, dst **HTTPError) bool {
	he, ok := err.(*HTTPError)
	if ok {
		*dst = he
	}
	return ok
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var rdr io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "releasebot")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 300 {
		return &HTTPError{StatusCode: resp.StatusCode, Method: method, Path: path,
			Body: truncate(string(body), 400)}
	}
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Change is what a release actually contains.
type Change struct {
	Commits  int      // how many commits are in the range
	Messages []string // their first lines, for extracting ticket references
}

// Compare asks GitHub what is in base..head.
//
// This is how a release learns its own contents. Without it a cut announces a
// branch name and nothing else, and the question every stakeholder actually
// asks -- "what is IN this release?" -- is answered by somebody reading the
// commit log by hand.
//
// ⚠️ GitHub caps the compare response at 250 commits. `total_commits` is the
// true count and `commits` is the truncated list, so the count is read from
// the former. Reporting len(commits) would silently understate every large
// release as exactly 250.
func (c *Client) Compare(ctx context.Context, repo, base, head string) (Change, error) {
	if c.DryRun {
		// Shaped like a real answer, and obviously synthetic when it is read
		// back off a ledger row -- the row is flagged dryRun beside it.
		return Change{Commits: 3, Messages: []string{
			"dry run: no changeset was read from GitHub",
		}}, nil
	}
	var out struct {
		TotalCommits int `json:"total_commits"`
		Commits      []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
	}
	path := "/repos/" + repo + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(head)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return Change{}, err
	}
	ch := Change{Commits: out.TotalCommits}
	for _, cm := range out.Commits {
		m := cm.Commit.Message
		if i := strings.IndexByte(m, '\n'); i >= 0 {
			m = m[:i]
		}
		ch.Messages = append(ch.Messages, m)
	}
	return ch, nil
}
