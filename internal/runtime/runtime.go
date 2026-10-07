// Package runtime implements the AWS Lambda Runtime API loop for a
// provided.al2 / provided.al2023 custom runtime.
//
// This is what `bootstrap` IS. On a managed runtime AWS supplies this loop and
// you hand it a handler; on provided.* the zip's root-level executable named
// `bootstrap` is PID 1 inside the sandbox and owns the loop itself:
//
//	GET  /2018-06-01/runtime/invocation/next        (long-poll; blocks, no timeout)
//	POST /2018-06-01/runtime/invocation/{id}/response
//	POST /2018-06-01/runtime/invocation/{id}/error
//	POST /2018-06-01/runtime/init/error
//
// Two things here are not obvious and are worth knowing before you own one:
//
//  1. The /next poll must have NO client timeout. Lambda freezes the execution
//     environment between invokes; a 30s http.Client timeout on that call turns
//     an idle function into a crash loop the moment traffic is sparse.
//  2. Anything that panics must be reported to /error before the process dies,
//     otherwise the caller waits out the full function timeout for what was an
//     instant failure, and the CloudWatch record says "Runtime exited" with no
//     cause.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const apiVersion = "2018-06-01"

// Invocation carries the per-request headers Lambda sets on /next.
type Invocation struct {
	RequestID          string
	DeadlineMS         int64
	InvokedFunctionARN string
	TraceID            string
}

// Deadline is when Lambda will kill this invoke. Handlers should derive their
// own HTTP timeouts from it rather than hardcoding, so a GitHub call cannot
// outlive the function and lose the chance to report a useful error.
func (i Invocation) Deadline() time.Time {
	return time.UnixMilli(i.DeadlineMS)
}

// Alias returns the alias the caller invoked through, parsed from the
// InvokedFunctionArn, or "" when the caller used $LATEST or a bare ARN.
//
// This is the escape hatch that makes one function + two aliases workable.
// Lambda environment variables are pinned to a VERSION, not to an alias, so
// `prod` and `test` aliases on the same published version see byte-identical
// env vars. The invoked ARN is the only in-band signal of which door the
// request came through:
//
//	arn:aws:lambda:us-east-1:123456789012:function:cutRelease:prod
//	                                                          ^^^^
// See docs/DESIGN-PROPOSALS.md #1.
func (i Invocation) Alias() string {
	// ARN has 7 colon-separated fields; an 8th field is the qualifier.
	n := 0
	last := -1
	for idx := 0; idx < len(i.InvokedFunctionARN); idx++ {
		if i.InvokedFunctionARN[idx] == ':' {
			n++
			last = idx
		}
	}
	if n < 7 {
		return ""
	}
	q := i.InvokedFunctionARN[last+1:]
	if q == "" || q == "$LATEST" {
		return ""
	}
	return q
}

type Handler func(ctx context.Context, inv Invocation, payload []byte) ([]byte, error)

// Start runs the invocation loop until the process is killed. It never returns
// under normal operation.
func Start(h Handler) {
	api := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if api == "" {
		fmt.Fprintln(os.Stderr, "AWS_LAMBDA_RUNTIME_API not set: not running under Lambda")
		os.Exit(1)
	}
	base := "http://" + api + "/" + apiVersion + "/runtime"

	// No Timeout on purpose -- see the package comment.
	client := &http.Client{}

	for {
		inv, payload, err := next(client, base)
		if err != nil {
			// A failure to even fetch work is fatal and unrecoverable; let the
			// sandbox be replaced rather than spin.
			fmt.Fprintf(os.Stderr, "runtime: fetching next invocation: %v\n", err)
			os.Exit(1)
		}
		respond(client, base, h, inv, payload)
	}
}

func next(c *http.Client, base string) (Invocation, []byte, error) {
	resp, err := c.Get(base + "/invocation/next")
	if err != nil {
		return Invocation{}, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Invocation{}, nil, err
	}
	inv := Invocation{
		RequestID:          resp.Header.Get("Lambda-Runtime-Aws-Request-Id"),
		InvokedFunctionARN: resp.Header.Get("Lambda-Runtime-Invoked-Function-Arn"),
		TraceID:            resp.Header.Get("Lambda-Runtime-Trace-Id"),
	}
	fmt.Sscanf(resp.Header.Get("Lambda-Runtime-Deadline-Ms"), "%d", &inv.DeadlineMS)
	// X-Ray and the Datadog tracer both read this from the environment.
	if inv.TraceID != "" {
		os.Setenv("_X_AMZN_TRACE_ID", inv.TraceID)
	}
	return inv, body, nil
}

func respond(c *http.Client, base string, h Handler, inv Invocation, payload []byte) {
	defer func() {
		if r := recover(); r != nil {
			postError(c, base+"/invocation/"+inv.RequestID+"/error",
				"HandlerPanic", fmt.Sprint(r))
		}
	}()

	ctx, cancel := context.WithDeadline(context.Background(), inv.Deadline())
	defer cancel()

	out, err := h(ctx, inv, payload)
	if err != nil {
		postError(c, base+"/invocation/"+inv.RequestID+"/error", "HandlerError", err.Error())
		return
	}
	req, _ := http.NewRequest(http.MethodPost,
		base+"/invocation/"+inv.RequestID+"/response", bytes.NewReader(out))
	if resp, err := c.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func postError(c *http.Client, url, errType, msg string) {
	body, _ := json.Marshal(map[string]string{
		"errorType":    errType,
		"errorMessage": msg,
	})
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Lambda-Runtime-Function-Error-Type", "Unhandled")
	if resp, err := c.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
