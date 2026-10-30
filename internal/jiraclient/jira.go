// Package jiraclient is a small Jira Cloud REST v3 client: create the release
// ticket, transition issues, search, read an issue, and apply a fixVersion.
//
// ⛔ Issue search goes ONLY through POST /rest/api/3/search/jql (token
// paginated). The older GET/POST /rest/api/2/search and /rest/api/3/search
// were removed by Atlassian and answer HTTP 410 Gone. The request path is
// checked in one place (do) so no future caller can reach them by accident.
package jiraclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrRemovedEndpoint is returned instead of calling a removed search endpoint.
var ErrRemovedEndpoint = fmt.Errorf("jira: /rest/api/{2,3}/search was removed (HTTP 410); use /rest/api/3/search/jql")

type Client struct {
	BaseURL string // https://<site>.atlassian.net
	Email   string
	Token   string
	HTTP    *http.Client
	DryRun  bool
}

func New(baseURL, email, token string, dryRun bool) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Email: email, Token: token, DryRun: dryRun,
		HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Issue is a search or GET result: the key plus raw fields.
type Issue struct {
	ID     string                     `json:"id"`
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
}

// FieldString returns a field as plain text: a string as-is, an ADF document
// flattened to its text, an object's "name"/"value". Empty if absent.
func (i Issue) FieldString(name string) string {
	raw, ok := i.Fields[name]
	if !ok {
		return ""
	}
	return RawToText(raw)
}

type HTTPError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("jira: %s %s -> %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// BrowseURL is the human link for an issue key.
func (c *Client) BrowseURL(key string) string { return c.BaseURL + "/browse/" + key }

// Base returns the site URL.
func (c *Client) Base() string { return c.BaseURL }

// CreateIssue creates an issue from a fields object and returns its key.
func (c *Client) CreateIssue(ctx context.Context, fields map[string]any) (string, error) {
	if c.DryRun {
		return "DRYRUN-1", nil
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := c.do(ctx, http.MethodPost, "/rest/api/3/issue", map[string]any{"fields": fields}, &out); err != nil {
		return "", err
	}
	return out.Key, nil
}

// Transition moves an issue with a transition id.
func (c *Client) Transition(ctx context.Context, key, transitionID string) error {
	if c.DryRun {
		return nil
	}
	return c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/transitions",
		map[string]any{"transition": map[string]string{"id": transitionID}}, nil)
}

// GetIssue reads one issue with the listed fields.
func (c *Client) GetIssue(ctx context.Context, key string, fields []string) (Issue, error) {
	if c.DryRun {
		return Issue{Key: key}, nil
	}
	var out Issue
	p := "/rest/api/3/issue/" + url.PathEscape(key)
	if len(fields) > 0 {
		p += "?fields=" + url.QueryEscape(strings.Join(fields, ","))
	}
	err := c.do(ctx, http.MethodGet, p, nil, &out)
	return out, err
}

// SearchJQL runs a JQL search through POST /rest/api/3/search/jql, following
// nextPageToken until isLast.
func (c *Client) SearchJQL(ctx context.Context, jql string, fields []string) ([]Issue, error) {
	if c.DryRun {
		return nil, nil
	}
	var all []Issue
	token := ""
	for page := 0; page < 50; page++ {
		req := map[string]any{"jql": jql, "maxResults": 100}
		if len(fields) > 0 {
			req["fields"] = fields
		}
		if token != "" {
			req["nextPageToken"] = token
		}
		var out struct {
			Issues        []Issue `json:"issues"`
			NextPageToken string  `json:"nextPageToken"`
			IsLast        bool    `json:"isLast"`
		}
		if err := c.do(ctx, http.MethodPost, "/rest/api/3/search/jql", req, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Issues...)
		if out.IsLast || out.NextPageToken == "" {
			return all, nil
		}
		token = out.NextPageToken
	}
	return all, fmt.Errorf("jira: search did not finish after 50 pages")
}

// EnsureVersion returns the id of the named version in the project, creating
// it when missing.
func (c *Client) EnsureVersion(ctx context.Context, projectID, name string) (string, error) {
	if c.DryRun {
		return "0", nil
	}
	var versions []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/project/"+url.PathEscape(projectID)+"/versions", nil, &versions); err != nil {
		return "", err
	}
	for _, v := range versions {
		if v.Name == name {
			return v.ID, nil
		}
	}
	pid := any(projectID)
	var n int
	if _, err := fmt.Sscanf(projectID, "%d", &n); err == nil {
		pid = n
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/rest/api/3/version",
		map[string]any{"name": name, "projectId": pid, "released": false}, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// AddFixVersion adds a fixVersion (by name) to an issue.
func (c *Client) AddFixVersion(ctx context.Context, key, versionName string) error {
	if c.DryRun {
		return nil
	}
	return c.do(ctx, http.MethodPut, "/rest/api/3/issue/"+url.PathEscape(key), map[string]any{
		"update": map[string]any{"fixVersions": []any{map[string]any{"add": map[string]string{"name": versionName}}}},
	}, nil)
}

// IsRemovedSearchPath reports whether path (without query) is one of the
// removed search endpoints.
func IsRemovedSearchPath(path string) bool {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimRight(path, "/")
	return path == "/rest/api/2/search" || path == "/rest/api/3/search" || path == "/rest/api/latest/search"
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if IsRemovedSearchPath(path) {
		return ErrRemovedEndpoint
	}
	if c.BaseURL == "" {
		return fmt.Errorf("jira: no base URL (set jira.baseURL in the config)")
	}
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
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Email != "" || c.Token != "" {
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(c.Email+":"+c.Token)))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("jira: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		b := string(body)
		if len(b) > 400 {
			b = b[:400] + "..."
		}
		return &HTTPError{Status: resp.StatusCode, Method: method, Path: path, Body: b}
	}
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}

// ── Atlassian Document Format ───────────────────────────────────────────────

// TextToADF wraps plain text as an ADF document, one paragraph per line, with
// bare URLs turned into links. v3 requires ADF for description and for
// multi-line text custom fields.
func TextToADF(text string) map[string]any {
	var paras []any
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		paras = append(paras, map[string]any{"type": "paragraph", "content": inline(line)})
	}
	if len(paras) == 0 {
		paras = []any{map[string]any{"type": "paragraph", "content": []any{}}}
	}
	return map[string]any{"type": "doc", "version": 1, "content": paras}
}

func inline(line string) []any {
	var out []any
	for i, word := range strings.Split(line, " ") {
		text := word
		if i > 0 {
			text = " " + word
		}
		node := map[string]any{"type": "text", "text": text}
		if strings.HasPrefix(word, "https://") || strings.HasPrefix(word, "http://") {
			if i > 0 {
				out = append(out, map[string]any{"type": "text", "text": " "})
			}
			node = map[string]any{"type": "text", "text": word,
				"marks": []any{map[string]any{"type": "link", "attrs": map[string]string{"href": word}}}}
		}
		out = append(out, node)
	}
	return out
}

// RawToText flattens a field value to text: a JSON string, an ADF document
// (paragraphs become lines), or an option object's value/name.
func RawToText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var node map[string]any
	if json.Unmarshal(raw, &node) != nil || node == nil {
		return ""
	}
	if node["type"] == "doc" {
		var b strings.Builder
		adfText(&b, node)
		return strings.TrimSpace(b.String())
	}
	for _, k := range []string{"value", "name"} {
		if v, ok := node[k].(string); ok {
			return v
		}
	}
	return ""
}

func adfText(b *strings.Builder, n map[string]any) {
	if t, ok := n["text"].(string); ok {
		b.WriteString(t)
	}
	if kids, ok := n["content"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				adfText(b, m)
			}
		}
	}
	switch n["type"] {
	case "paragraph", "heading", "codeBlock", "listItem":
		b.WriteString("\n")
	case "hardBreak":
		b.WriteString("\n")
	}
}
