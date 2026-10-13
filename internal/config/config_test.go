package config

import "testing"

// The alias has to win over RARC_ENV, because env vars are pinned to a
// published VERSION -- two aliases on one version see identical env vars.
// See docs/DESIGN-PROPOSALS.md #1.
func TestWithAliasOverridesRarcEnv(t *testing.T) {
	base := Config{Env: "test"}
	if got := base.WithAlias("prod").Env; got != "prod" {
		t.Fatalf("prod alias did not override RARC_ENV: got %q", got)
	}
	if got := (Config{Env: "prod"}).WithAlias("test").Env; got != "test" {
		t.Fatalf("test alias did not override RARC_ENV: got %q", got)
	}
	// An unqualified invoke, or a numeric version, leaves the env var in charge.
	if got := base.WithAlias("").Env; got != "test" {
		t.Fatalf("empty alias changed the env: got %q", got)
	}
	if got := base.WithAlias("41").Env; got != "test" {
		t.Fatalf("numeric version changed the env: got %q", got)
	}
	// `live` is a deployment pointer, not an environment. Mapping it to prod
	// made every test-stage function report itself as prod, because every
	// function is fronted by a `live` alias. Regression guard.
	if got := base.WithAlias("live").Env; got != "test" {
		t.Fatalf("the `live` alias overrode RARC_ENV: got %q, want test", got)
	}
	if got := (Config{Env: "prod"}).WithAlias("live").Env; got != "prod" {
		t.Fatalf("the `live` alias changed a prod config: got %q", got)
	}
}

func TestRedactedNeverCarriesASecretValue(t *testing.T) {
	c := Config{Env: "prod", GitHubToken: "ghp_realtokenvalue", SlackSigningSecret: "s3cr3t"}
	for k, v := range c.Redacted() {
		if s, ok := v.(string); ok && (s == "ghp_realtokenvalue" || s == "s3cr3t") {
			t.Fatalf("Redacted() leaked a secret under %q", k)
		}
	}
}

// ⛔ THE ASYMMETRY IS DELIBERATE AND MUST STAY PINNED.
//
// An EMPTY cut allowlist admits everyone; an empty APPROVAL allowlist admits
// nobody. Someone will eventually read one of those and "fix" the other to
// match. These tests say which is which and why, so that change has to be
// deliberate.
func TestPermittedToCut(t *testing.T) {
	// Empty = unrestricted. Asking for a release BRANCH is low-risk, the
	// production gate is the approval, and defaulting to "nobody" would break
	// an existing workflow the moment it ships.
	open := Config{}
	for _, id := range []string{"U0ANYONE", "", "whatever"} {
		if !open.PermittedToCut(id) {
			t.Fatalf("empty allowlist must admit %q", id)
		}
	}
	if got := open.CutAllowlistState(); got != "unrestricted" {
		t.Fatalf("an empty list must REPORT itself as unrestricted, got %q", got)
	}

	c := Config{CutAllowlist: []string{"U0ALLOWED", " u0spaced "}}
	if !c.PermittedToCut("U0ALLOWED") {
		t.Fatal("a listed id must be admitted")
	}
	// Slack ids are case-stable, but a human retyping one is not. Trim and fold.
	if !c.PermittedToCut("u0allowed") || !c.PermittedToCut("U0SPACED") {
		t.Fatal("matching must trim and fold case")
	}
	if c.PermittedToCut("U0DENIED") {
		t.Fatal("an unlisted id must be refused")
	}
	// ⛔ The case that routes around the control: a command with NO Slack user
	// (Jira, API) must not be admitted by a non-empty allowlist.
	if c.PermittedToCut("") {
		t.Fatal("an empty user id must be refused when a list exists")
	}
	if got := c.CutAllowlistState(); got != "2 permitted" {
		t.Fatalf("state should count the list, got %q", got)
	}
}

// A list a human edited in a console is the input that actually arrives.
func TestSplitListDropsBlanks(t *testing.T) {
	got := splitList("U1,,U2 , ,U3,")
	if len(got) != 3 || got[1] != "U2" {
		t.Fatalf("want 3 clean entries, got %v", got)
	}
	// ⛔ A blank entry would match an empty user id — the caller with no Slack
	// user at all.
	for _, g := range got {
		if g == "" {
			t.Fatal("a blank entry would admit a command with no Slack user")
		}
	}
	if len(splitList("")) != 0 || len(splitList("  ,  ")) != 0 {
		t.Fatal("an effectively empty string must yield no entries, not one blank")
	}
}
