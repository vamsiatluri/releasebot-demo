package robbie

import (
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		want    Args
		wantErr string
	}{
		{name: "mention + cut", text: "<@U0DEMOBOT> cut kettle", want: Args{Command: "cut", Project: "kettle"}},
		{name: "mention with name", text: "<@U0DEMOBOT|release-automation> precut Kettle", want: Args{Command: "precut", Project: "Kettle"}},
		{name: "text before mention, trailing period", text: "hey <@U0DEMOBOT> recut kettle.", want: Args{Command: "recut", Project: "kettle"}},
		{name: "no mention (DM / slash)", text: "projects", want: Args{Command: "projects"}},
		{name: "command case", text: "<@U0DEMOBOT> CUT kettle", want: Args{Command: "cut", Project: "kettle"}},
		{name: "release patch", text: "<@U0DEMOBOT> release kettle patch", want: Args{Command: "release", Project: "kettle", Bump: "patch"}},
		{name: "release MINOR hasEnvChanges", text: "<@U0DEMOBOT> release kettle MINOR hasEnvChanges",
			want: Args{Command: "release", Project: "kettle", Bump: "minor", EnvVarChange: true}},
		{name: "jiraticket flag", text: "<@U0DEMOBOT> jiraticket kettle hasenvchange",
			want: Args{Command: "jiraticket", Project: "kettle", EnvVarChange: true}},
		{name: "mergeback", text: "<@U0DEMOBOT> mergeback kettle", want: Args{Command: "mergeback", Project: "kettle"}},
		{name: "release-please", text: "<@U0DEMOBOT> release-please", want: Args{Command: "release-please"}},
		{name: "release please", text: "<@U0DEMOBOT> release please", want: Args{Command: "release-please"}},
		{name: "project called please", text: "<@U0DEMOBOT> release please patch", want: Args{Command: "release", Project: "please", Bump: "patch"}},
		{name: "slack-linked project", text: "<@U0DEMOBOT> cut <http://kettle.example|kettle.example>", want: Args{Command: "cut", Project: "kettle.example"}},
		{name: "ping", text: "<@U0DEMOBOT> ping", want: Args{Command: "ping"}},
		{name: "empty", text: "<@U0DEMOBOT>", wantErr: "Missing arguments"},
		{name: "unknown", text: "<@U0DEMOBOT> deploy kettle", wantErr: "Unsupported command: deploy"},
		{name: "cut without project", text: "<@U0DEMOBOT> cut", wantErr: "needs a project"},
		{name: "release without bump", text: "<@U0DEMOBOT> release kettle", wantErr: "no version bump"},
		{name: "release bad bump", text: "<@U0DEMOBOT> release kettle huge", wantErr: "is not valid"},
		{name: "release bad flag", text: "<@U0DEMOBOT> release kettle patch yes", wantErr: "hasEnvChanges"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseArgs(tc.text)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestFromSlashCommand(t *testing.T) {
	cases := []struct {
		name, command, text, want string
	}{
		{name: "command named slash", command: "/cut", text: "kettle", want: "cut kettle"},
		{name: "bare slash", command: "/projects", text: "", want: "projects"},
		{name: "text repeats command", command: "/release", text: "release kettle patch", want: "release kettle patch"},
		{name: "generic slash", command: "/release-bot", text: "cut kettle", want: "cut kettle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromSlashCommand(tc.command, tc.text); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestVersionJump(t *testing.T) {
	cases := []struct {
		name, from, bump, want, jump string
	}{
		{name: "patch", from: "1.4.0", bump: "patch", want: "1.4.1", jump: "1.4.0 → 1.4.1 (patch)"},
		{name: "minor", from: "v1.4.7", bump: "minor", want: "1.5.0", jump: "1.4.7 → 1.5.0 (minor)"},
		{name: "major", from: "1.4.7", bump: "major", want: "2.0.0", jump: "1.4.7 → 2.0.0 (major)"},
		{name: "pre-release patch", from: "1.4.1-rc.1", bump: "patch", want: "1.4.1", jump: "1.4.1-rc.1 → 1.4.1 (patch)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := ParseSemver(tc.from)
			if err != nil {
				t.Fatal(err)
			}
			n, err := v.Bump(tc.bump)
			if err != nil || n.String() != tc.want || n.Tag() != "v"+tc.want {
				t.Fatalf("bump = %v %v", n, err)
			}
			if got := VersionJump(v, n, tc.bump); got != tc.jump {
				t.Fatalf("jump %q want %q", got, tc.jump)
			}
		})
	}
	if _, err := ParseSemver("5.4"); err == nil {
		t.Fatal("5.4 accepted")
	}
}

func TestReplaceVersion(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{name: "package.json", in: "{\n  \"name\": \"kettle\",\n  \"version\": \"1.4.0\",\n  \"x\": 1\n}\n",
			want: "{\n  \"name\": \"kettle\",\n  \"version\": \"1.4.1\",\n  \"x\": 1\n}\n"},
		{name: "lockfile v3 replaces two", in: "{\n  \"version\": \"1.4.0\",\n  \"lockfileVersion\": 3,\n  \"packages\": {\"\": {\"version\": \"1.4.0\"}, \"dep\": {\"version\": \"9.9.9\"}}\n}",
			want: "{\n  \"version\": \"1.4.1\",\n  \"lockfileVersion\": 3,\n  \"packages\": {\"\": {\"version\": \"1.4.1\"}, \"dep\": {\"version\": \"9.9.9\"}}\n}"},
		{name: "lockfile v1 replaces one", in: "{\"version\": \"1.4.0\", \"lockfileVersion\": 1, \"d\": {\"version\": \"1.4.0\"}}",
			want: "{\"version\": \"1.4.1\", \"lockfileVersion\": 1, \"d\": {\"version\": \"1.4.0\"}}"},
		{name: "yaml quoted", in: "name: kettle\nversion: \"1.4.0\"\n", want: "name: kettle\nversion: \"1.4.1\"\n"},
		{name: "yaml bare", in: "version: 1.4.0\nother: 2\n", want: "version: 1.4.1\nother: 2\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceVersion(tc.in, "1.4.1")
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v\nwant %q", got, err, tc.want)
			}
			v, err := ReadVersion(got)
			if err != nil || v != "1.4.1" {
				t.Fatalf("ReadVersion = %q %v", v, err)
			}
		})
	}
	if _, err := ReplaceVersion("no version here", "1.0.0"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestIssueKeys(t *testing.T) {
	cases := []struct {
		name   string
		format string
		texts  []string
		want   string
	}{
		{name: "default", texts: []string{"DEMO-12: fix", "also DEMO-12 and KETL-3"}, want: "DEMO-12,KETL-3"},
		{name: "case-insensitive with spaces", format: `(?i)demo\s*-?\s*\d{1,5}`, texts: []string{"demo 7 and Demo-8"}, want: "DEMO-7,DEMO-8"},
		{name: "digit inside key", format: `[A-Z][A-Z0-9]+-\d+`, texts: []string{"AB2-12"}, want: "AB2-12"},
		{name: "none", texts: []string{"chore: tidy"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IssueKeys(tc.format, tc.texts...)
			if err != nil || strings.Join(got, ",") != tc.want {
				t.Fatalf("got %v %v want %s", got, err, tc.want)
			}
		})
	}
}

func TestParseSlack(t *testing.T) {
	const bot = "U0DEMOBOT"
	cases := []struct {
		name      string
		body      string
		challenge string
		ignore    string
		req       Request
		slash     bool
	}{
		{name: "url_verification", body: `{"type":"url_verification","challenge":"abc123"}`, challenge: "abc123"},
		{name: "app_mention", body: `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@U0DEMOBOT> cut kettle","channel":"C1"}}`,
			req: Request{UserID: "U1", ChannelID: "C1", Text: "<@U0DEMOBOT> cut kettle", Source: "slack"}},
		{name: "mention of someone else first", body: `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@U9> fyi <@U0DEMOBOT> cut kettle","channel":"C1"}}`,
			req: Request{UserID: "U1", ChannelID: "C1", Text: "<@U0DEMOBOT> cut kettle", Source: "slack"}},
		{name: "DM", body: `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"projects","channel":"D1"}}`,
			req: Request{UserID: "U1", ChannelID: "D1", IsDM: true, Text: "projects", Source: "slack"}},
		{name: "edit", body: `{"type":"event_callback","event":{"type":"message","subtype":"message_changed","channel":"C1"}}`, ignore: "edit"},
		{name: "edited mention", body: `{"type":"event_callback","event":{"type":"app_mention","user":"U1","edited":{"ts":"1"},"channel":"C1"}}`, ignore: "edit"},
		{name: "bot", body: `{"type":"event_callback","event":{"type":"app_mention","bot_id":"B1","channel":"C1"}}`, ignore: "bot"},
		{name: "self", body: `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U0DEMOBOT","channel":"D1"}}`, ignore: "bot"},
		{name: "channel message", body: `{"type":"event_callback","event":{"type":"message","channel_type":"channel","user":"U1","channel":"C1"}}`, ignore: "non-DM"},
		{name: "slash", body: "command=%2Fcut&text=kettle&user_id=U1&channel_id=C1&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2Fx",
			slash: true, req: Request{UserID: "U1", ChannelID: "C1", ResponseURL: "https://hooks.slack.com/commands/x", Text: "cut kettle", Source: "slack"}},
		{name: "interactive", body: "payload=%7B%7D", ignore: "interact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := ParseSlack([]byte(tc.body), bot)
			if err != nil {
				t.Fatal(err)
			}
			if in.Challenge != tc.challenge || in.Slash != tc.slash {
				t.Fatalf("got %+v", in)
			}
			if tc.ignore != "" {
				if !strings.Contains(strings.ToLower(in.Ignore), strings.ToLower(tc.ignore)) {
					t.Fatalf("ignore = %q, want %q", in.Ignore, tc.ignore)
				}
				return
			}
			if in.Ignore != "" && tc.challenge == "" {
				t.Fatalf("unexpectedly ignored: %s", in.Ignore)
			}
			if tc.challenge == "" && in.Req != tc.req {
				t.Fatalf("req = %+v\nwant %+v", in.Req, tc.req)
			}
		})
	}
}
