// Package awssig signs AWS API requests with Signature Version 4.
//
// Why this exists rather than the AWS SDK: this project is deliberately
// stdlib-only -- the Lambda Runtime API loop is hand-written, because that is
// what `provided.*` actually is -- and pulling the SDK in for two GET requests
// would be the only dependency in the tree.
//
// ⚠️ It signs GET requests with an empty body and nothing else. That is all the
// dashboard needs, and a signer that quietly half-supports POST is worse than
// one that does not claim to.
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// emptyBodySHA256 is sha256("") — the payload hash every request here uses.
const emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credentials come from the Lambda execution environment. Lambda injects the
// execution role's temporary credentials as environment variables; there is no
// instance metadata call to make and no profile to resolve.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func FromEnvironment() (Credentials, error) {
	c := Credentials{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return c, fmt.Errorf("no AWS credentials in the environment")
	}
	return c, nil
}

// SignGET adds the Authorization header to req. service is e.g. "lambda".
func SignGET(req *http.Request, c Credentials, service, region string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateOnly := now.UTC().Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	// Host is not in req.Header for an outbound request; it lives on req.Host /
	// req.URL.Host, and it MUST be part of the signature.
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	signed := []string{"host", "x-amz-date"}
	canonical := "host:" + host + "\n" + "x-amz-date:" + amzDate + "\n"
	if c.SessionToken != "" {
		signed = append(signed, "x-amz-security-token")
		canonical += "x-amz-security-token:" + c.SessionToken + "\n"
	}
	sort.Strings(signed)
	signedHeaders := strings.Join(signed, ";")

	// ⚠️ The canonical URI must be the ENCODED path. A Lambda function name is
	// safe, but a qualifier or an ARN carries ':' — encoded by EscapedPath,
	// and a mismatch here fails with SignatureDoesNotMatch, which reads like a
	// wrong secret key rather than a path-encoding bug.
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonical,
		signedHeaders,
		emptyBodySHA256,
	}, "\n")

	scope := strings.Join([]string{dateOnly, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hashHex(canonicalRequest),
	}, "\n")

	key := hmacBytes([]byte("AWS4"+c.SecretAccessKey), dateOnly)
	key = hmacBytes(key, region)
	key = hmacBytes(key, service)
	key = hmacBytes(key, "aws4_request")
	signature := hex.EncodeToString(hmacBytes(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKeyID, scope, signedHeaders, signature))
}

func hmacBytes(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
