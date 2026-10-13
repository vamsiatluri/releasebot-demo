package baseline

import (
	"strings"
	"testing"
)

func TestIsSecretish(t *testing.T) {
	secret := []string{
		"GITHUB_TOKEN", "SLACK_TOKEN", "SLACK_SIGNING_SECRET", "JIRA_PASSWORD",
		"DD_API_KEY", "github_token", "DATABASE_PASSWORD", "WEBHOOK_SECRET",
		"PRIVATE_KEY", "SESSION_SECRET",
	}
	for _, k := range secret {
		if !IsSecretish(k) {
			t.Errorf("%q should be withheld", k)
		}
	}
	open := []string{
		"RARC_ENV", "RARC_CONFIG_URL", "DEFAULT_OWNER", "SLACK_CHANNEL",
		"DD_SERVICE", "DD_ENV", "DD_SITE", "AWS_REGION", "DEFAULT_BRANCH",
		// References are not values -- capturing these is the point of the
		// "hold a pointer, not a secret" design.
		"SECRETS_PREFIX", "SLACK_SECRET_ARN", "GithubTokenArn",
	}
	for _, k := range open {
		if IsSecretish(k) {
			t.Errorf("%q should be recorded; over-redaction makes the diff useless", k)
		}
	}
}

func TestSplitEnvNeverReturnsASecretValue(t *testing.T) {
	env := map[string]string{
		"RARC_ENV":       "prod",
		"GITHUB_TOKEN":   "ghp_thisMustNeverAppearInOutput",
		"SECRETS_PREFIX": "releasebot/prod",
	}
	keys, safe, secretLooking, _ := SplitEnv(env)
	if len(keys) != 3 {
		t.Fatalf("expected all 3 key names, got %v", keys)
	}
	for k, v := range safe {
		if v == "ghp_thisMustNeverAppearInOutput" {
			t.Fatalf("SplitEnv leaked a secret value under %q", k)
		}
	}
	if _, present := safe["GITHUB_TOKEN"]; present {
		t.Fatal("GITHUB_TOKEN's value was recorded")
	}
	if safe["SECRETS_PREFIX"] != "releasebot/prod" {
		t.Fatal("a pointer to a secret should be recorded, it is not the secret")
	}
	if len(secretLooking) != 1 || secretLooking[0] != "GITHUB_TOKEN" {
		t.Fatalf("expected GITHUB_TOKEN flagged, got %v", secretLooking)
	}
}

// ⛔ THE TOOL MUST NOT REPORT THE FIX AS THE DEFECT.
//
// After the secrets moved to SecureString references the review still said
// "holds secret values in environment variables" about every function, because
// it judged by NAME alone. A tool that reads identically before and after the
// work cannot show that a control is in place — and worse, it tells the next
// person to undo it.
func TestAReferenceIsNotAPlaintextSecret(t *testing.T) {
	env := map[string]string{
		"GITHUB_TOKEN":         "ssm:/releasebot/prod/secrets/github-token",
		"SLACK_SIGNING_SECRET": "secretsmanager:releasebot/prod/slack",
		"JIRA_TOKEN":           "ghp_an_actual_literal_value",
		"SLACK_TOKEN":          "", // set but empty: neither finding nor fix
		"CONFIG_GENERATION":    "20261013T221500Z",
	}
	_, safe, literal, byRef := SplitEnv(env)

	if len(literal) != 1 || literal[0] != "JIRA_TOKEN" {
		t.Fatalf("only the literal should be a finding, got %v", literal)
	}
	if len(byRef) != 2 {
		t.Fatalf("both references should be recorded as references, got %v", byRef)
	}
	// ⚠️ A reference's value must STILL not be recorded. It is a pointer, but
	// the rule is "never store a secret-looking variable's value", and a
	// parameter PATH in a captured JSON file is a map of where the secrets are.
	for k, v := range safe {
		if strings.Contains(v, "releasebot/prod/secrets") {
			t.Fatalf("a secret's PATH was recorded under %q", k)
		}
	}
	// The rotation marker must not read as a credential.
	if _, bad := safe["CONFIG_GENERATION"]; !bad {
		t.Fatal("CONFIG_GENERATION is a timestamp and should be captured as ordinary config")
	}
}
