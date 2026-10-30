package robbie

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Inbound is what a verified Slack request asks for.
type Inbound struct {
	Challenge string // url_verification: echo this back
	Ignore    string // non-empty: acknowledge and do nothing, with this reason
	Slash     bool   // a slash command (answers on response_url)
	Req       Request
}

type slackEvent struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Event     struct {
		Type        string          `json:"type"`
		Subtype     string          `json:"subtype"`
		User        string          `json:"user"`
		BotID       string          `json:"bot_id"`
		Text        string          `json:"text"`
		Channel     string          `json:"channel"`
		ChannelType string          `json:"channel_type"`
		Edited      json.RawMessage `json:"edited"`
	} `json:"event"`
}

// ParseSlack reads a signature-verified Slack request body: an Events API JSON
// envelope or a slash-command form. botID is the configured slackbotid.
func ParseSlack(body []byte, botID string) (Inbound, error) {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		var ev slackEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return Inbound{}, fmt.Errorf("unreadable Slack event: %w", err)
		}
		switch ev.Type {
		case "url_verification":
			return Inbound{Challenge: ev.Challenge}, nil
		case "event_callback":
		default:
			return Inbound{Ignore: "Ignoring Slack request of type " + ev.Type}, nil
		}
		e := ev.Event
		switch {
		case e.Subtype == "message_changed" || e.Subtype == "message_deleted" || len(e.Edited) > 0 && string(e.Edited) != "null":
			return Inbound{Ignore: "Ignoring Slack message edit event"}, nil
		case e.BotID != "" || e.Subtype == "bot_message" || (botID != "" && e.User == botID):
			return Inbound{Ignore: "Ignoring bot message"}, nil
		case e.Type == "message" && e.ChannelType != "im":
			return Inbound{Ignore: "Ignoring non-DM message event"}, nil
		case e.Type != "message" && e.Type != "app_mention":
			return Inbound{Ignore: "Ignoring event " + e.Type}, nil
		}
		return Inbound{Req: Request{
			UserID: e.User, ChannelID: e.Channel, IsDM: e.ChannelType == "im",
			Text: focusOnBot(e.Text, botID), Source: "slack",
		}}, nil
	}

	form, err := url.ParseQuery(trimmed)
	if err != nil {
		return Inbound{}, fmt.Errorf("unreadable Slack form: %w", err)
	}
	if form.Get("payload") != "" {
		return Inbound{Ignore: "Interactive payloads go to /interact"}, nil
	}
	if form.Get("command") == "" {
		return Inbound{Ignore: "Not a slash command"}, nil
	}
	ch := form.Get("channel_id")
	return Inbound{Slash: true, Req: Request{
		UserID: form.Get("user_id"), ChannelID: ch, ResponseURL: form.Get("response_url"),
		IsDM: strings.HasPrefix(ch, "D") || form.Get("channel_name") == "directmessage",
		Text: FromSlashCommand(form.Get("command"), form.Get("text")), Source: "slack",
	}}, nil
}

// focusOnBot drops anything before the bot's own mention, so "<@SOMEONE> fyi
// <@BOT> cut app" is read as a command to the bot.
func focusOnBot(text, botID string) string {
	if botID == "" {
		return text
	}
	if i := strings.Index(text, "<@"+botID); i >= 0 {
		return text[i:]
	}
	return text
}
