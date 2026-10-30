package robbie

import (
	"fmt"
	"regexp"
	"strings"
)

// Args is one parsed command.
type Args struct {
	Command      string // lower case
	Project      string // as typed
	Bump         string // patch | minor | major (release only)
	EnvVarChange bool   // hasEnvChanges given (release, jiraticket)
}

const (
	CmdPing          = "ping"
	CmdVersion       = "version"
	CmdHelp          = "help"
	CmdProjects      = "projects"
	CmdReleasePlease = "release-please"
	CmdPrecut        = "precut"
	CmdCut           = "cut"
	CmdRecut         = "recut"
	CmdRelease       = "release"
	CmdJiraTicket    = "jiraticket"
	CmdMergeback     = "mergeback"
	CmdCherryPick    = "cherry-pick"
)

var (
	informational = map[string]bool{CmdPing: true, CmdVersion: true, CmdHelp: true, CmdProjects: true, CmdReleasePlease: true}
	withProject   = map[string]bool{CmdPrecut: true, CmdCut: true, CmdRecut: true, CmdRelease: true,
		CmdJiraTicket: true, CmdMergeback: true, CmdCherryPick: true}
	mention = regexp.MustCompile(`<@[A-Za-z0-9]+(\|[^>]*)?>`)
)

// IsInformational: read-only commands, the only ones allowed in a DM.
func IsInformational(cmd string) bool { return informational[cmd] }

// IsKnown reports whether cmd is a command name at all.
func IsKnown(cmd string) bool { return informational[cmd] || withProject[cmd] }

// UsageText is the reply to text that is not a command.
const UsageText = "Call me like this: `@release-automation [command] [project]`, e.g. `@release-automation cut my-app`.\n" +
	"`release` takes a bump and an optional env-var flag: `@release-automation release my-app patch hasEnvChanges`.\n" +
	"`jiraticket` takes the optional env-var flag: `@release-automation jiraticket my-app hasEnvChanges`.\n" +
	"`@release-automation help` lists every command."

// ParseArgs reads a command from message text. Everything up to and including
// the first user mention is dropped (so "hey <@BOT> cut app" works), as is a
// trailing period.
func ParseArgs(text string) (Args, error) {
	t := strings.TrimSpace(text)
	if loc := mention.FindStringIndex(t); loc != nil {
		t = t[loc[1]:]
	}
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "."))
	f := strings.Fields(t)
	if len(f) == 0 {
		return Args{}, fmt.Errorf("Missing arguments.\n%s", UsageText)
	}
	cmd := strings.ToLower(f[0])

	// release-please, releaseplease, or "release please" — but "release please
	// patch" is a release of a project called "please".
	if cmd == CmdReleasePlease || cmd == "releaseplease" ||
		(cmd == CmdRelease && len(f) >= 2 && strings.EqualFold(f[1], "please") && (len(f) < 3 || !isBump(f[2]))) {
		return Args{Command: CmdReleasePlease}, nil
	}
	if informational[cmd] {
		return Args{Command: cmd}, nil
	}
	if !withProject[cmd] {
		return Args{}, fmt.Errorf("Unsupported command: %s. %s", f[0], UsageText)
	}
	if len(f) < 2 {
		return Args{}, fmt.Errorf("Missing arguments: `%s` needs a project.\n%s", cmd, UsageText)
	}
	a := Args{Command: cmd, Project: cleanProject(f[1])}
	rest := f[2:]
	switch cmd {
	case CmdRelease:
		if len(rest) == 0 {
			return Args{}, fmt.Errorf("Release argument error: no version bump (patch, minor, major) provided")
		}
		if !isBump(rest[0]) {
			return Args{}, fmt.Errorf("Release argument %s is not valid. Must be patch, minor, or major", rest[0])
		}
		a.Bump = strings.ToLower(rest[0])
		rest = rest[1:]
		fallthrough
	case CmdJiraTicket:
		if len(rest) > 0 {
			if !isEnvFlag(rest[0]) {
				return Args{}, fmt.Errorf("the environment variable change flag must be 'hasEnvChanges' or 'hasEnvChange' (any case)")
			}
			a.EnvVarChange = true
		}
	}
	return a, nil
}

func isBump(s string) bool {
	switch strings.ToLower(s) {
	case "patch", "minor", "major":
		return true
	}
	return false
}

func isEnvFlag(s string) bool {
	s = strings.ToLower(s)
	return s == "hasenvchanges" || s == "hasenvchange"
}

// cleanProject undoes Slack's auto-linking of a project typed as a domain
// ("<http://app.example|app.example>" -> "app.example").
func cleanProject(p string) string {
	if strings.HasPrefix(p, "<") && strings.HasSuffix(p, ">") {
		inner := strings.Trim(p, "<>")
		if i := strings.IndexByte(inner, '|'); i >= 0 {
			return inner[i+1:]
		}
		inner = strings.TrimPrefix(strings.TrimPrefix(inner, "https://"), "http://")
		return inner
	}
	return p
}

// FromSlashCommand turns a slash command into command text. `/cut my-app` has
// command "/cut" and text "my-app"; a generic command (`/robbie cut my-app`)
// carries the whole command in its text.
func FromSlashCommand(command, text string) string {
	name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(command), "/"))
	text = strings.TrimSpace(text)
	if !IsKnown(name) {
		return text
	}
	if text == "" {
		return name
	}
	first := strings.ToLower(strings.Fields(text)[0])
	if first == name {
		return text
	}
	return name + " " + text
}
