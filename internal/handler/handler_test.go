package handler

import (
	"strings"
	"testing"
)

// secretSource must describe the MECHANISM accurately and never overstate it.
// A "reference" claim about a plaintext secret would make a half-finished
// rollout read as finished, which is the whole reason the field exists.
func TestSecretSource(t *testing.T) {
	keys := []string{"GITHUB_TOKEN", "SLACK_TOKEN", "SLACK_SIGNING_SECRET",
		"JIRA_TOKEN", "JIRA_WEBHOOK_SECRET", "DD_API_KEY"}
	clear := func(t *testing.T) {
		for _, k := range keys {
			t.Setenv(k, "")
		}
	}

	t.Run("nothing set is not a finding", func(t *testing.T) {
		clear(t)
		if got := secretSource(); got != "unset" {
			t.Fatalf("want unset, got %q", got)
		}
	})
	t.Run("all references", func(t *testing.T) {
		clear(t)
		t.Setenv("GITHUB_TOKEN", "ssm:/releasebot/prod/github-token")
		t.Setenv("SLACK_SIGNING_SECRET", "secretsmanager:releasebot/prod/slack")
		if got := secretSource(); got != "reference" {
			t.Fatalf("want reference, got %q", got)
		}
	})
	t.Run("a literal is reported as plaintext", func(t *testing.T) {
		clear(t)
		t.Setenv("GITHUB_TOKEN", "ghp_literal")
		if got := secretSource(); got != "plaintext" {
			t.Fatalf("want plaintext, got %q", got)
		}
	})
	// ⛔ The case that matters. One migrated, one not, is NOT "reference".
	t.Run("mixed must not read as hardened", func(t *testing.T) {
		clear(t)
		t.Setenv("GITHUB_TOKEN", "ssm:/releasebot/prod/github-token")
		t.Setenv("SLACK_SIGNING_SECRET", "literal-secret")
		got := secretSource()
		if got == "reference" {
			t.Fatal("a partly-migrated function must never report itself as hardened")
		}
		if !strings.Contains(got, "1 of 2") {
			t.Fatalf("want a count, got %q", got)
		}
	})
}
