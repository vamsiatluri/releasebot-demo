// Package config resolves ReleaseBot's runtime configuration.
//
// The migration SOW lists these as Lambda environment variables:
//   GITHUB_TOKEN, SLACK_TOKEN, JIRA creds, RARC_ENV, RARC_CONFIG_URL,
//   the Slack signing secret, and the Datadog variables.
//
// Non-secret values stay as env vars. Secrets are resolved through a
// SecretResolver so the production account can hold them in Secrets Manager
// while dev and the local harness keep using plain env vars -- the handler code
// does not change between the two. See docs/DESIGN-PROPOSALS.md #2 for why the
// production account should not carry a GitHub admin token in plaintext
// function configuration.
package config

import (
	"context"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Env           string // RARC_ENV: "prod" | "test"
	ConfigURL     string // RARC_CONFIG_URL: repo/branch policy document
	GitHubAPI     string // override for the local harness
	SlackAPI      string
	JiraAPI       string
	DefaultBranch string

	GitHubToken       string
	SlackToken        string
	SlackSigningSecret string
	JiraUser          string
	JiraToken         string

	DatadogSite    string
	DatadogAPIKey  string
	DatadogService string
	DryRun         bool
}

// SecretResolver turns a reference into a secret value. In production the
// reference is a Secrets Manager ARN; locally it is the literal value.
type SecretResolver func(ctx context.Context, ref string) (string, error)

// EnvResolver treats the reference as the value itself. Used by the local
// harness and by the dev account during the parallel-run window.
func EnvResolver(_ context.Context, ref string) (string, error) { return ref, nil }

func Load(ctx context.Context, resolve SecretResolver) (Config, error) {
	if resolve == nil {
		resolve = EnvResolver
	}
	c := Config{
		Env:            envOr("RARC_ENV", "test"),
		ConfigURL:      os.Getenv("RARC_CONFIG_URL"),
		GitHubAPI:      envOr("GITHUB_API_URL", "https://api.github.com"),
		SlackAPI:       envOr("SLACK_API_URL", "https://slack.com/api"),
		JiraAPI:        envOr("JIRA_API_URL", ""),
		DefaultBranch:  envOr("DEFAULT_BRANCH", "main"),
		DatadogSite:    envOr("DD_SITE", "datadoghq.com"),
		DatadogService: envOr("DD_SERVICE", "releasebot"),
		DryRun:         os.Getenv("RELEASEBOT_DRY_RUN") == "1",
	}

	var err error
	for _, f := range []struct {
		key string
		dst *string
		req bool
	}{
		{"GITHUB_TOKEN", &c.GitHubToken, true},
		{"SLACK_TOKEN", &c.SlackToken, true},
		{"SLACK_SIGNING_SECRET", &c.SlackSigningSecret, true},
		{"JIRA_USER", &c.JiraUser, false},
		{"JIRA_TOKEN", &c.JiraToken, false},
		{"DD_API_KEY", &c.DatadogAPIKey, false},
	} {
		raw := os.Getenv(f.key)
		if raw == "" {
			if f.req && !c.DryRun {
				return c, fmt.Errorf("config: %s is required", f.key)
			}
			continue
		}
		if *f.dst, err = resolve(ctx, raw); err != nil {
			return c, fmt.Errorf("config: resolving %s: %w", f.key, err)
		}
	}

	if c.Env != "prod" && c.Env != "test" {
		return c, fmt.Errorf("config: RARC_ENV must be prod or test, got %q", c.Env)
	}
	return c, nil
}

// WithAlias lets the invoked alias win over RARC_ENV.
//
// Env vars are pinned to a published VERSION, so if one function serves both a
// `prod` and a `test` alias, RARC_ENV cannot differ between them. The alias from
// the invoked ARN can. Keeping the env var as the fallback means this is a
// no-op in the lift-and-shift topology (four functions, no aliases) and becomes
// load-bearing only if the alias proposal is approved.
func (c Config) WithAlias(alias string) Config {
	switch strings.ToLower(alias) {
	case "prod", "production", "live":
		c.Env = "prod"
	case "test", "beta", "staging":
		c.Env = "test"
	}
	return c
}

// Redacted renders the config for logging. Every secret field is omitted
// entirely rather than masked -- a masked value still tells you the length.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"env":            c.Env,
		"config_url":     c.ConfigURL,
		"github_api":     c.GitHubAPI,
		"slack_api":      c.SlackAPI,
		"default_branch": c.DefaultBranch,
		"dd_service":     c.DatadogService,
		"dry_run":        c.DryRun,
		"github_token":   present(c.GitHubToken),
		"slack_token":    present(c.SlackToken),
		"signing_secret": present(c.SlackSigningSecret),
	}
}

func present(s string) string {
	if s == "" {
		return "absent"
	}
	return "present"
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
