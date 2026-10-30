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

// Post sends text to a channel via chat.postMessage, with an optional context
// line (small grey text under the message) rendered as a Block Kit context
// block. The plain text is always sent too, for notifications and clients
// that do not render blocks.
func (c *Client) Post(ctx context.Context, channel, text, contextLine string) error {
	if c.DryRun {
		return nil
	}
	if strings.TrimSpace(channel) == "" {
		return fmt.Errorf("slack: no channel to post to")
	}
	payload := map[string]any{"channel": channel, "text": text}
	if contextLine != "" {
		payload["blocks"] = Blocks(text, contextLine)
	}
	return c.call(ctx, "chat.postMessage", payload)
}

// RespondBlocks posts to a slash command's response_url, in channel, with an
// optional context line.
func (c *Client) RespondBlocks(ctx context.Context, responseURL, text, contextLine string) error {
	if c.DryRun || responseURL == "" {
		return nil
	}
	payload := map[string]any{"response_type": "in_channel", "replace_original": false, "text": text}
	if contextLine != "" {
		payload["blocks"] = Blocks(text, contextLine)
	}
	body, _ := json.Marshal(payload)
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

// SetTopic sets a channel's topic (conversations.setTopic).
func (c *Client) SetTopic(ctx context.Context, channel, topic string) error {
	if c.DryRun {
		return nil
	}
	return c.call(ctx, "conversations.setTopic", map[string]any{"channel": channel, "topic": topic})
}

// Blocks renders a section with the text and a context line under it.
func Blocks(text, contextLine string) []any {
	if len(text) > 2900 {
		text = text[:2900] + "…"
	}
	return []any{
		map[string]any{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": text}},
		map[string]any{"type": "context", "elements": []any{map[string]string{"type": "mrkdwn", "text": contextLine}}},
	}
}

func (c *Client) call(ctx context.Context, method string, payload map[string]any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("slack: unreadable %s response (status %d)", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("slack: %s failed: %s", method, r.Error)
	}
	return nil
}
