// Package lambdainvoke performs an asynchronous Lambda self-invocation.
//
// WHY THIS EXISTS — the bug it fixes:
//
// Slack hangs up on a slash command after 3 seconds, so the handler returns an
// acknowledgement immediately and finishes the real work afterwards. The
// obvious way to write that in Go is `go doTheWork()`.
//
// It does not work on Lambda, and it fails NON-DETERMINISTICALLY, which is
// worse than failing every time. Lambda FREEZES the execution environment the
// moment the handler's response is returned. A goroutine still running at that
// instant is suspended mid-flight; it resumes only if that same sandbox is
// reused for another invocation, and by then its deadlines have expired.
// Observed in this account: four `cut` commands recorded correctly because the
// container was warm, while the `mergeback` on a COLD container logged
// "releasebot starting" and then nothing at all. The work was silently
// dropped, and the user saw only "Working on it..." — forever.
//
// The fix is to make the follow-up work a REAL INVOCATION with its own
// lifetime: acknowledge Slack, then ask Lambda to invoke this same function
// again with InvocationType=Event. The second invocation gets its own timeout,
// its own log stream, its own automatic retries, and cannot be frozen by the
// first one returning.
//
// ⚠️ It invokes its OWN PUBLISHED VERSION, not $LATEST and not the alias. A
// deploy that moved the `live` alias between the acknowledgement and the async
// invocation would otherwise finish the job on different code than the code
// that accepted it — rare, and exactly the kind of thing nobody can reproduce
// afterwards.
package lambdainvoke

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/awssig"
)

// Self asynchronously invokes the currently-running function with payload.
//
// It returns as soon as Lambda has ACCEPTED the invocation (HTTP 202), not when
// the work is done — that is the entire point.
func Self(ctx context.Context, payload []byte) error {
	name := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	region := os.Getenv("AWS_REGION")
	if name == "" || region == "" {
		return fmt.Errorf("not running under Lambda")
	}
	creds, err := awssig.FromEnvironment()
	if err != nil {
		return err
	}

	// AWS_LAMBDA_FUNCTION_VERSION is the numeric version when the function was
	// invoked through an alias, which is how every call here arrives.
	//
	// ⚠️ THE VERSION GOES IN THE QUERY STRING, NOT THE PATH. Lambda accepts
	// `/functions/<name>:<version>/invocations`, but the colon has to be
	// percent-encoded in the path and the encoding does not survive the
	// canonical-request step -- the call comes back 403
	// InvalidSignatureException, which reads exactly like a broken credential
	// and sent me looking at IAM first. `?Qualifier=` avoids the colon
	// altogether and is the form already proven by the dashboard's reader.
	endpoint := "https://lambda." + region + ".amazonaws.com/2015-03-31/functions/" +
		url.PathEscape(name) + "/invocations"
	if v := os.Getenv("AWS_LAMBDA_FUNCTION_VERSION"); v != "" && v != "$LATEST" {
		endpoint += "?Qualifier=" + url.QueryEscape(v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	// Event = fire and forget. WITHOUT this header the call is synchronous, and
	// the function would block waiting for itself — a deadlock against its own
	// reserved concurrency, which times out in a genuinely baffling way.
	req.Header.Set("X-Amz-Invocation-Type", "Event")
	req.Header.Set("Content-Type", "application/json")
	awssig.Sign(req, payload, creds, "lambda", region, time.Now())

	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	// 202 Accepted is the ONLY success for an Event invocation. A 200 means it
	// ran synchronously, i.e. the header did not take effect — which would pass
	// a naive 2xx check while reintroducing the exact bug this replaces.
	if resp.StatusCode != http.StatusAccepted {
		// ⚠️ The error TYPE, not the body. The body of an AWS error names the
		// role ARN and the caller identity and must not reach a log; the type
		// is a bare string like "AccessDeniedException" and is the single most
		// useful fact here, because a 403 alone cannot distinguish "the policy
		// does not allow this" from "the request was signed wrong" — and those
		// two have completely different fixes. The first version of this
		// swallowed it and cost a debugging round.
		kind := resp.Header.Get("X-Amzn-Errortype")
		if i := strings.IndexByte(kind, ':'); i > 0 {
			kind = kind[:i]
		}
		if kind == "" {
			kind = "unknown"
		}
		return fmt.Errorf("lambda returned %d (%s) for an async invoke", resp.StatusCode, kind)
	}
	return nil
}
