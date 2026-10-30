package githubclient

// The calls the config-driven release flow makes: list and merge labelled PRs,
// merge the default branch into the release branch, bump version files, tag,
// publish a GitHub release, and open the mergeback PRs.
//
// Every write honours DryRun and returns an obviously synthetic answer, so a
// dry run exercises the whole flow without touching a repository.

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// PRInfo is an open pull request as the release flow sees it.
type PRInfo struct {
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	HTMLURL string   `json:"html_url"`
	Author  string   `json:"-"`
	Labels  []string `json:"-"`
}

// Commit is one commit of a comparison.
type Commit struct {
	SHA     string
	Title   string // first line of the message
	Message string
	Author  string // GitHub login when known, else the git author name
}

type Tag struct {
	Name string
	SHA  string
}

type Release struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

func fakeSHA(parts ...string) string {
	h := sha1.Sum([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])
}

// ListOpenPRs lists open PRs whose base is base, oldest first.
func (c *Client) ListOpenPRs(ctx context.Context, repo, base string) ([]PRInfo, error) {
	if c.DryRun {
		return nil, nil
	}
	var raw []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	path := fmt.Sprintf("/repos/%s/pulls?state=open&base=%s&sort=created&direction=asc&per_page=100",
		repo, url.QueryEscape(base))
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]PRInfo, 0, len(raw))
	for _, r := range raw {
		p := PRInfo{Number: r.Number, Title: r.Title, HTMLURL: r.HTMLURL, Author: r.User.Login}
		for _, l := range r.Labels {
			p.Labels = append(p.Labels, l.Name)
		}
		out = append(out, p)
	}
	return out, nil
}

// Approvals counts reviewers whose LATEST review is an approval. A reviewer who
// approved and then requested changes does not count.
func (c *Client) Approvals(ctx context.Context, repo string, number int) (int, error) {
	if c.DryRun {
		return 0, nil
	}
	var reviews []struct {
		State string `json:"state"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", repo, number), nil, &reviews); err != nil {
		return 0, err
	}
	latest := map[string]string{}
	for _, r := range reviews {
		if r.State == "COMMENTED" {
			continue
		}
		latest[r.User.Login] = r.State
	}
	n := 0
	for _, s := range latest {
		if s == "APPROVED" {
			n++
		}
	}
	return n, nil
}

// PRMergeable reports GitHub's mergeable flag and mergeable_state for a PR.
// mergeable is false while GitHub is still computing it.
func (c *Client) PRMergeable(ctx context.Context, repo string, number int) (bool, string, error) {
	if c.DryRun {
		return true, "clean", nil
	}
	var pr struct {
		Mergeable      *bool  `json:"mergeable"`
		MergeableState string `json:"mergeable_state"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &pr); err != nil {
		return false, "", err
	}
	return pr.Mergeable != nil && *pr.Mergeable, pr.MergeableState, nil
}

// MergePR merges a PR with the given method (merge | squash | rebase).
func (c *Client) MergePR(ctx context.Context, repo string, number int, method, title string) error {
	if c.DryRun {
		return nil
	}
	body := map[string]string{"merge_method": method}
	if title != "" {
		body["commit_title"] = title
	}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, number), body, nil)
}

// AddLabels adds labels to an issue or PR.
func (c *Client) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	if c.DryRun || len(labels) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number),
		map[string][]string{"labels": labels}, nil)
}

// MergeBranches merges head INTO base with a merge commit (POST /merges).
// merged is false when base already contains head (GitHub answers 204).
func (c *Client) MergeBranches(ctx context.Context, repo, base, head, message string) (sha string, merged bool, err error) {
	if c.DryRun {
		return fakeSHA(repo, base, head), true, nil
	}
	var out struct {
		SHA string `json:"sha"`
	}
	body := map[string]string{"base": base, "head": head}
	if message != "" {
		body["commit_message"] = message
	}
	status, err := c.doStatus(ctx, http.MethodPost, "/repos/"+repo+"/merges", body, &out)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNoContent {
		return "", false, nil
	}
	return out.SHA, true, nil
}

// CompareCommits lists the commits in base...head, oldest first.
func (c *Client) CompareCommits(ctx context.Context, repo, base, head string) ([]Commit, error) {
	if c.DryRun {
		return []Commit{{SHA: fakeSHA(repo, base, head), Title: "dry run: no changeset was read from GitHub",
			Author: "releasebot"}}, nil
	}
	var out struct {
		Commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"commit"`
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"commits"`
	}
	path := "/repos/" + repo + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(head) + "?per_page=250"
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	res := make([]Commit, 0, len(out.Commits))
	for _, cm := range out.Commits {
		title := cm.Commit.Message
		if i := strings.IndexByte(title, '\n'); i >= 0 {
			title = title[:i]
		}
		author := cm.Commit.Author.Name
		if cm.Author != nil && cm.Author.Login != "" {
			author = cm.Author.Login
		}
		res = append(res, Commit{SHA: cm.SHA, Title: title, Message: cm.Commit.Message, Author: author})
	}
	return res, nil
}

// GetFile returns a file's decoded content at ref.
func (c *Client) GetFile(ctx context.Context, repo, path, ref string) (string, error) {
	if c.DryRun {
		return `{"name":"dry-run","version":"0.0.0"}`, nil
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	p := "/repos/" + repo + "/contents/" + escapePath(path) + "?ref=" + url.QueryEscape(ref)
	if err := c.do(ctx, http.MethodGet, p, nil, &out); err != nil {
		return "", err
	}
	if out.Encoding != "base64" {
		return out.Content, nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("github: %s is not valid base64: %w", path, err)
	}
	return string(b), nil
}

// CommitFiles writes files onto branch as ONE commit (Git data API) and moves
// the branch to it without force. Returns the new commit SHA.
func (c *Client) CommitFiles(ctx context.Context, repo, branch string, files map[string]string, message string) (string, error) {
	if c.DryRun {
		return fakeSHA(repo, branch, message), nil
	}
	parent, err := c.HeadSHA(ctx, repo, branch)
	if err != nil {
		return "", err
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/git/commits/"+parent, nil, &commit); err != nil {
		return "", err
	}
	type entry struct {
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	entries := make([]entry, 0, len(files))
	for p, content := range files {
		entries = append(entries, entry{Path: p, Mode: "100644", Type: "blob", Content: content})
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/git/trees",
		map[string]any{"base_tree": commit.Tree.SHA, "tree": entries}, &tree); err != nil {
		return "", err
	}
	var created struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/git/commits",
		map[string]any{"message": message, "tree": tree.SHA, "parents": []string{parent}}, &created); err != nil {
		return "", err
	}
	if err := c.do(ctx, http.MethodPatch, "/repos/"+repo+"/git/refs/heads/"+escapePath(branch),
		map[string]any{"sha": created.SHA, "force": false}, nil); err != nil {
		return "", err
	}
	return created.SHA, nil
}

// CreateTag creates an annotated tag object at sha and its refs/tags ref.
func (c *Client) CreateTag(ctx context.Context, repo, tag, sha, message string) error {
	if c.DryRun {
		return nil
	}
	var obj struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/git/tags", map[string]string{
		"tag": tag, "message": message, "object": sha, "type": "commit",
	}, &obj); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/repos/"+repo+"/git/refs",
		map[string]string{"ref": "refs/tags/" + tag, "sha": obj.SHA}, nil)
}

// CreateRelease publishes a (non-draft) GitHub release at tag.
func (c *Client) CreateRelease(ctx context.Context, repo, tag, sha, body string) (Release, error) {
	if c.DryRun {
		return Release{TagName: tag, Name: tag, Body: body,
			HTMLURL: "https://github.com/" + repo + "/releases/tag/" + tag + "#dry-run"}, nil
	}
	var r Release
	err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/releases", map[string]any{
		"tag_name": tag, "target_commitish": sha, "name": tag, "body": body,
		"draft": false, "prerelease": false,
	}, &r)
	return r, err
}

// LatestRelease returns the latest published release.
func (c *Client) LatestRelease(ctx context.Context, repo string) (Release, error) {
	if c.DryRun {
		return Release{TagName: "v0.0.0", Name: "v0.0.0", Body: "dry run"}, nil
	}
	var r Release
	err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/releases/latest", nil, &r)
	return r, err
}

// ListTags lists tags, newest first as GitHub returns them (first 100).
func (c *Client) ListTags(ctx context.Context, repo string) ([]Tag, error) {
	if c.DryRun {
		return nil, nil
	}
	var raw []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/tags?per_page=100", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Tag, 0, len(raw))
	for _, t := range raw {
		out = append(out, Tag{Name: t.Name, SHA: t.Commit.SHA})
	}
	return out, nil
}

// IsNotFound reports whether err is a GitHub 404.
func IsNotFound(err error) bool {
	he, ok := err.(*HTTPError)
	return ok && he.StatusCode == http.StatusNotFound
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
