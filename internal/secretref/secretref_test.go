package secretref

import (
	"context"
	"strings"
	"testing"
)

// ⚠️ THE LITERAL PASSTHROUGH IS LOAD-BEARING. The local harness, the contract
// tests and the dev account all pass real values. If Resolve ever started
// requiring a scheme, every one of them would need SSM standing up, and the
// pressure would be to skip the tests instead.
func TestLiteralsPassThroughUnchanged(t *testing.T) {
	for _, v := range []string{
		"ghp_notarealtoken", "", "a/b/c", "arn:aws:iam::1:role/x",
		"ssm", "ssmsomething", // prefix-LIKE but not the scheme
	} {
		got, err := Resolve(context.Background(), v)
		if err != nil {
			t.Fatalf("literal %q errored: %v", v, err)
		}
		if got != v {
			t.Fatalf("literal %q was rewritten to %q", v, got)
		}
	}
}

func TestIsReference(t *testing.T) {
	refs := []string{"ssm:/releasebot/prod/github-token", "secretsmanager:releasebot/prod/gh"}
	for _, r := range refs {
		if !IsReference(r) {
			t.Fatalf("%q should be a reference", r)
		}
	}
	// A literal must NOT read as a reference — /health reports this, and
	// reporting "it is a reference" about a plaintext secret would describe a
	// function as hardened when it is not.
	for _, r := range []string{"ghp_x", "", "ssmish", "/releasebot/prod/x"} {
		if IsReference(r) {
			t.Fatalf("%q must not read as a reference", r)
		}
	}
}

// An empty name must fail rather than resolve to "", because an empty secret
// reaching the HMAC comparison would verify nothing and reject everything —
// which looks like a Slack problem, not a configuration problem.
func TestEmptyReferenceIsAnError(t *testing.T) {
	for _, ref := range []string{"ssm:", "secretsmanager:"} {
		if _, err := Resolve(context.Background(), ref); err == nil {
			t.Fatalf("%q should error", ref)
		}
	}
}

// Whatever goes wrong, the message must not be able to carry a secret or the
// identity that read it. This pins the shape of the only error a caller can
// trigger without AWS.
func TestErrorsNeverEchoAValue(t *testing.T) {
	_, err := Resolve(context.Background(), "ssm:")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "arn:") {
		t.Fatalf("error mentions an ARN: %v", err)
	}
}
