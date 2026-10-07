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
