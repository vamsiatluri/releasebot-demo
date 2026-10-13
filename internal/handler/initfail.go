package handler

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/vamsiatluri/releasebot-migration/internal/events"
)

// UnreadyResponse answers an invocation that arrived while INIT had failed.
//
// WHY THIS EXISTS. The functions resolve configuration during INIT and refuse
// to serve without it -- correct, and the direction you want to fail in. But
// returning an error from the handler makes API Gateway emit a bare
// `502 {"message": "Internal server error"}`, and Lambda writes NOTHING to the
// log: the operator sees START, END, 1.25 ms, no error, and a 502. Three
// separate things then mislead at once:
//
//   - 502 is indistinguishable from a crash, so the first hour goes on the
//     network, the VPC and the IAM policy.
//   - the actual reason ("GITHUB_TOKEN is required") exists ONLY in the
//     response body of a direct Invoke, which nobody runs until much later.
//   - /beta/health, the one endpoint whose entire job is to report readiness,
//     is itself unavailable -- so the thing that would have answered the
//     question is the thing that cannot.
//
// So: health reports unreadiness as DATA, with 503 and the missing key named.
// Everything else still refuses, because a function with no credentials must
// not pretend it can cut a release.
//
// ⚠️ The message names the missing KEY, never a value. `config: GITHUB_TOKEN is
// required` is safe to return; the token itself never is.
func UnreadyResponse(req events.APIGatewayProxyRequest, initErr error) ([]byte, bool) {
	if !strings.HasSuffix(req.Path, "/health") {
		return nil, false
	}
	body, _ := json.Marshal(map[string]any{
		"ok":     false,
		"ready":  false,
		"reason": initErr.Error(),
		// ⚠️ THE ACCOUNT, on the UNREADY path too. This response is built by
		// hand rather than by the normal health handler, so it silently lacked
		// the one field that says WHICH environment is unready -- and an
		// unready environment is exactly when someone is looking at two
		// dashboards trying to work out which one is broken.
		"account": os.Getenv("RELEASEBOT_ACCOUNT_LABEL"),
		"env":     os.Getenv("RARC_ENV"),
		"commit":  os.Getenv("RELEASEBOT_COMMIT"),
		"hint": "configuration resolved at INIT is incomplete. This is not a crash: " +
			"the function is refusing to serve rather than running without credentials.",
	})
	resp, _ := json.Marshal(events.APIGatewayProxyResponse{
		StatusCode: 503,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	})
	return resp, true
}

// LogInitFailure writes the INIT failure where an operator will actually look.
// Without this the log shows a clean START/END and the reason is invisible.
func LogInitFailure(action string, err error) {
	fmt.Fprintf(os.Stderr,
		"INIT FAILED action=%s env=%s: %v — every route except /health will return an error\n",
		action, os.Getenv("RARC_ENV"), err)
}
