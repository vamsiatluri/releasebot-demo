// Package secretref turns a REFERENCE into a secret value at INIT.
//
// WHY THIS EXISTS. The signing secret and the GitHub token were plaintext
// Lambda environment variables. Three consequences, and the first is the one
// that matters:
//
//  1. ⛔ Anyone with `lambda:GetFunctionConfiguration` reads both, in the
//     console, in plaintext. That is a far wider set of people than anyone
//     intended to hold a token that can push to every repository the owner can
//     reach. No network control touches this.
//  2. A published version freezes CONFIGURATION as well as code, so every
//     secret is frozen into every version — and rotating one silently
//     invalidates every older version as a rollback target, which is exactly
//     the day you need one.
//  3. The value appears in a CloudFormation parameter, and therefore in a
//     change set, and therefore in anything that archives change sets.
//
// Holding a REFERENCE instead fixes all three at once: the version freezes a
// pointer, the console shows a path, and rotation is a Parameter Store write
// with no deploy and no stranded rollback target.
//
// ⚠️ A bare value with no scheme is returned UNCHANGED. That is deliberate and
// load-bearing: the local harness, the contract tests and the dev account keep
// passing literals, and the handler code is identical in both worlds. A
// resolver that refused literals would force every test to stand up SSM.
package secretref

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/awssig"
)

// Scheme prefixes a reference may carry.
const (
	ssmPrefix = "ssm:"
	smPrefix  = "secretsmanager:"
)

// Resolve reads the value behind a reference.
//
//	ssm:/releasebot/prod/github-token          a SecureString parameter
//	secretsmanager:releasebot/prod/github      a Secrets Manager secret
//	ghp_xxxxxxxx                               a literal, returned as-is
//
// ⚠️ Errors NEVER include the resolved value, and never the response body: an
// SSM or Secrets Manager error names the parameter ARN and the calling
// identity, and an error string ends up in CloudWatch.
func Resolve(ctx context.Context, ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, ssmPrefix):
		return fromSSM(ctx, strings.TrimPrefix(ref, ssmPrefix))
	case strings.HasPrefix(ref, smPrefix):
		return fromSecretsManager(ctx, strings.TrimPrefix(ref, smPrefix))
	default:
		return ref, nil
	}
}

// IsReference reports whether a value is a pointer rather than a secret. Used
// by the health endpoint to say WHICH mechanism is in force without ever
// reporting the value — "the token is a reference" is safe to publish, and is
// the only way an operator can tell a hardened function from a plaintext one
// without reading the secret itself.
func IsReference(ref string) bool {
	return strings.HasPrefix(ref, ssmPrefix) || strings.HasPrefix(ref, smPrefix)
}

func fromSSM(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("secretref: empty ssm parameter name")
	}
	var out struct {
		Parameter struct {
			Value string `json:"Value"`
			Type  string `json:"Type"`
		} `json:"Parameter"`
	}
	if err := post(ctx, "ssm", "AmazonSSM.GetParameter",
		map[string]any{"Name": name, "WithDecryption": true}, &out); err != nil {
		return "", err
	}
	if out.Parameter.Value == "" {
		return "", fmt.Errorf("secretref: %s resolved to an empty value", name)
	}
	// ⚠️ A String parameter holding a secret is the bug this package exists to
	// fix, so refuse it rather than quietly succeeding. Succeeding here would
	// mean the hardening looked done while the value sat unencrypted.
	if out.Parameter.Type != "SecureString" {
		return "", fmt.Errorf("secretref: %s is type %s, not SecureString", name, out.Parameter.Type)
	}
	return out.Parameter.Value, nil
}

func fromSecretsManager(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("secretref: empty secret id")
	}
	var out struct {
		SecretString string `json:"SecretString"`
	}
	if err := post(ctx, "secretsmanager", "secretsmanager.GetSecretValue",
		map[string]any{"SecretId": id}, &out); err != nil {
		return "", err
	}
	if out.SecretString == "" {
		return "", fmt.Errorf("secretref: %s resolved to an empty value", id)
	}
	// A JSON secret with a single key is the common shape; unwrap it so callers
	// need not care. Anything else is returned verbatim.
	var kv map[string]string
	if json.Unmarshal([]byte(out.SecretString), &kv) == nil && len(kv) == 1 {
		for _, v := range kv {
			return v, nil
		}
	}
	return out.SecretString, nil
}

// post signs and sends one AWS-JSON request. Shared so the two services differ
// only in their target, which is the only thing that actually differs.
func post(ctx context.Context, service, target string, body map[string]any, out any) error {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		return fmt.Errorf("secretref: no AWS_REGION")
	}
	creds, err := awssig.FromEnvironment()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://"+service+"."+region+".amazonaws.com/", strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", target)
	awssig.Sign(req, raw, creds, service, region, time.Now())

	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if resp.StatusCode != 200 {
		// ⚠️ Status and error TYPE only. The body names the parameter ARN and
		// the caller identity, and this string reaches CloudWatch.
		return fmt.Errorf("secretref: %s returned %d (%s)", service, resp.StatusCode,
			resp.Header.Get("X-Amzn-Errortype"))
	}
	return json.Unmarshal(payload, out)
}
