// Package slackclient posts ReleaseBot's results back to Slack.
//
// Slash commands get an immediate ephemeral ACK from the handler; the outcome
// of the actual release work arrives later on the response_url, which Slack
// keeps valid for 30 minutes and 5 uses. That split is what keeps the handler
// inside Slack's 3-second rule. See docs/DESIGN-PROPOSALS.md #3.
package slackclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, DryRun: dryRun,
		HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Respond posts to a slash command's response_url.
func (c *Client) Respond(ctx context.Context, responseURL, text string, inChannel bool) error {
	if c.DryRun || responseURL == "" {
		return nil
	}
	visibility := "ephemeral"
	if inChannel {
		visibility = "in_channel"
	}
	body, _ := json.Marshal(map[string]any{
		"response_type":    visibility,
		"replace_original": false,
		"text":             text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack: response_url: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack: response_url returned %d", resp.StatusCode)
	}
	return nil
}

// PostMessage posts to a channel via chat.postMessage.
func (c *Client) PostMessage(ctx context.Context, channel, text string) error {
	if c.DryRun {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"channel": channel, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat.postMessage",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack: chat.postMessage: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	// Slack answers 200 with {"ok":false,"error":"..."} for application errors.
	// Checking only the status code is how a bot goes silent without alerting.
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("slack: unreadable chat.postMessage response: %w", err)
	}
	if !r.OK {
		return fmt.Errorf("slack: chat.postMessage failed: %s", r.Error)
	}
	return nil
}
