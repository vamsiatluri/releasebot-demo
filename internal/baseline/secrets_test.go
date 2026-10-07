package baseline

import "testing"

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
		"RARC_ENV":     "prod",
		"GITHUB_TOKEN": "ghp_thisMustNeverAppearInOutput",
		"SECRETS_PREFIX": "releasebot/prod",
	}
	keys, safe, secretLooking := SplitEnv(env)
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
