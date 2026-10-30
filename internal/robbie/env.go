// Package robbie is the config-driven release bot: the Slack commands
// (precut, cut, recut, release, jiraticket, mergeback, ...), the Jira
// mergeback webhook, and the async SQS path, running under the environment
// contract of the migration Terraform (ROBBIE_* variables, `live` alias, API
// Gateway proxy events).
//
// It is this repository's own implementation, written to behave like the
// production release bot closely enough to stand in for it in an end-to-end
// rehearsal. See docs/E2E-REHEARSAL.md for what matches and what does not.
package robbie

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/vamsiatluri/releasebot-migration/internal/secretref"
)

// Kind is which function this binary is.
type Kind string

const (
	KindCut       Kind = "cut"
	KindMergeback Kind = "mergeback"
)

// Enabled reports whether the function runs in this mode: the Terraform
// contract always sets ROBBIE_CONFIG_URL. Without it the binaries keep the
// demo's original behaviour, so the existing harness and tests are unchanged.
func Enabled() bool { return strings.TrimSpace(os.Getenv("ROBBIE_CONFIG_URL")) != "" }

// Env is everything read from the process environment.
type Env struct {
	Stage           string // ROBBIE_ENV (fallback RARC_ENV): prod | test
	ConfigURL       string // ROBBIE_CONFIG_URL
	ConfigURLBackup string // ROBBIE_CONFIG_URL_BACKUP
	SSMRoot         string // ROBBIE_SSM_ROOT (reported; secrets name their own parameters)

	GitHubToken    string
	SlackToken     string
	SigningSecret  string
	SigningVersion string // SLACK_SIGNING_SECRET_VERSION_NUMBER, "v0"
	JiraToken      string
	DatadogAPIKey  string

	JiraEmail     string // JIRA_EMAIL (fallback JIRA_USER)
	JiraAccountID string // JIRA_ACCOUNT_ID

	DDEnv          string // DD_ENV
	DDTraceEnabled string // DD_TRACE_ENABLED (reported only)

	AsyncMode string // ASYNC_ENQUEUE_MODE: "sqs" or empty
	QueueURL  string // ASYNC_QUEUE_URL

	// Overrides for local mocks only. Unset in AWS.
	GitHubAPI string // RELEASEBOT_GITHUB_API_URL
	SlackAPI  string // SLACK_API_URL
	JiraAPI   string // JIRA_API_URL: replaces jira.baseURL for API calls

	AccountLabel string // RELEASEBOT_ACCOUNT_LABEL
	DryRun       bool   // RELEASEBOT_DRY_RUN=1

	// Where each secret came from: "env", "ssm-param", "ssm-ref", "unset".
	SecretSources map[string]string
}

// Async reports whether Slack work is acknowledged and queued.
func (e Env) Async() bool { return strings.EqualFold(e.AsyncMode, "sqs") }

// LoadEnv reads the environment and resolves secrets.
//
// Each secret NAME is read in this order:
//
//	NAME              a literal (secret_mode = "env"), or an ssm:/path reference
//	NAME_SSM_PARAM    a Parameter Store name (secret_mode = "ssm"), read with decryption
//
// ⚠️ A missing REQUIRED secret is an INIT error, not an empty string: an empty
// signing secret rejects every request and reads as a Slack outage.
func LoadEnv(ctx context.Context, kind Kind) (Env, error) {
	e := Env{
		Stage:           firstEnv("ROBBIE_ENV", "RARC_ENV"),
		ConfigURL:       strings.TrimSpace(os.Getenv("ROBBIE_CONFIG_URL")),
		ConfigURLBackup: strings.TrimSpace(os.Getenv("ROBBIE_CONFIG_URL_BACKUP")),
		SSMRoot:         os.Getenv("ROBBIE_SSM_ROOT"),
		JiraEmail:       firstEnv("JIRA_EMAIL", "JIRA_USER"),
		JiraAccountID:   os.Getenv("JIRA_ACCOUNT_ID"),
		DDEnv:           os.Getenv("DD_ENV"),
		DDTraceEnabled:  os.Getenv("DD_TRACE_ENABLED"),
		AsyncMode:       os.Getenv("ASYNC_ENQUEUE_MODE"),
		QueueURL:        os.Getenv("ASYNC_QUEUE_URL"),
		GitHubAPI:       envOr("RELEASEBOT_GITHUB_API_URL", "https://api.github.com"),
		SlackAPI:        envOr("SLACK_API_URL", "https://slack.com/api"),
		JiraAPI:         os.Getenv("JIRA_API_URL"),
		AccountLabel:    envOr("RELEASEBOT_ACCOUNT_LABEL", "unlabelled"),
		DryRun:          os.Getenv("RELEASEBOT_DRY_RUN") == "1",
		SecretSources:   map[string]string{},
	}
	if e.Stage == "" {
		e.Stage = "test"
	}
	if e.Stage != "prod" && e.Stage != "test" {
		return e, fmt.Errorf("env: ROBBIE_ENV must be prod or test, got %q", e.Stage)
	}

	required := map[string]bool{"GITHUB_TOKEN": true, "SLACK_TOKEN": true}
	if kind == KindCut {
		required["SLACK_SIGNING_SECRET"] = true
	}
	for _, s := range []struct {
		name string
		dst  *string
	}{
		{"GITHUB_TOKEN", &e.GitHubToken},
		{"SLACK_TOKEN", &e.SlackToken},
		{"SLACK_SIGNING_SECRET", &e.SigningSecret},
		{"SLACK_SIGNING_SECRET_VERSION_NUMBER", &e.SigningVersion},
		{"JIRA_TOKEN", &e.JiraToken},
		{"DD_API_KEY", &e.DatadogAPIKey},
	} {
		v, src, err := resolveSecret(ctx, s.name)
		if err != nil {
			return e, err
		}
		*s.dst, e.SecretSources[s.name] = v, src
		if v == "" && required[s.name] && !e.DryRun {
			return e, fmt.Errorf("env: %s (or %s_SSM_PARAM) is required", s.name, s.name)
		}
	}
	if e.SigningVersion == "" {
		e.SigningVersion = "v0"
	}
	if e.Async() && e.QueueURL == "" {
		return e, fmt.Errorf("env: ASYNC_ENQUEUE_MODE=sqs needs ASYNC_QUEUE_URL")
	}
	return e, nil
}

// Resolver is swappable in tests.
var Resolver = secretref.Resolve

func resolveSecret(ctx context.Context, name string) (string, string, error) {
	if v := os.Getenv(name); v != "" {
		src := "env"
		if secretref.IsReference(v) {
			src = "ssm-ref"
		}
		out, err := Resolver(ctx, v)
		if err != nil {
			return "", src, fmt.Errorf("env: resolving %s: %w", name, err)
		}
		return out, src, nil
	}
	if p := strings.TrimSpace(os.Getenv(name + "_SSM_PARAM")); p != "" {
		out, err := Resolver(ctx, "ssm:"+p)
		if err != nil {
			return "", "ssm-param", fmt.Errorf("env: reading %s_SSM_PARAM: %w", name, err)
		}
		return out, "ssm-param", nil
	}
	return "", "unset", nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
