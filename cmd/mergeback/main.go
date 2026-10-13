// Command mergeback is the bootstrap executable for the releaseAutomationMergeback Lambda.
//
// provided.al2 requires the binary to be named `bootstrap` at the ROOT of the
// deployment zip. Not bin/bootstrap, not mergeback. See the Makefile.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/vamsiatluri/releasebot-migration/internal/app"
	"github.com/vamsiatluri/releasebot-migration/internal/events"
	"github.com/vamsiatluri/releasebot-migration/internal/handler"
	"github.com/vamsiatluri/releasebot-migration/internal/runtime"
)

func main() {
	// Build once, outside the invocation loop: configuration resolution and
	// secret fetches belong in the INIT phase, which gets a 10s budget and
	// (with provisioned concurrency) is not billed per request. Doing it
	// inside the loop pays for it on every invoke.
	var h *handler.Handler
	initErr := func() error {
		built, err := app.Build(context.Background(), handler.ActionMergeback, "", ackOnly())
		h = built
		return err
	}()

	// ⚠️ Logged at INIT, not at first invoke. Lambda writes nothing of its own
	// when a handler returns an error, so without this the CloudWatch stream
	// shows START / END / 1.25 ms and no reason at all behind a 502.
	if initErr != nil {
		handler.LogInitFailure("mergeback", initErr)
	}

	runtime.Start(func(ctx context.Context, inv runtime.Invocation, payload []byte) ([]byte, error) {
		if initErr != nil {
			// /health must still answer -- it is the endpoint that exists to
			// report exactly this, and a 502 from it tells the operator
			// nothing. Every other route refuses.
			var probe events.APIGatewayProxyRequest
			if json.Unmarshal(payload, &probe) == nil {
				if resp, ok := handler.UnreadyResponse(probe, initErr); ok {
					return resp, nil
				}
			}
			return nil, fmt.Errorf("initialisation failed: %w", initErr)
		}
		// ⚠️ CHECKED FIRST, and on an explicit marker rather than on shape.
		// A self-dispatched job is not an HTTP event and has no signature on
		// it -- its authority comes from the IAM policy that allowed the
		// invoke. Deciding by shape ("it has no `path`, so it must be a job")
		// would let a malformed proxy event fall through into the branch that
		// performs a release.
		if job, ok := handler.IsAsyncJob(payload); ok {
			if alias := inv.Alias(); alias != "" {
				h.Cfg = h.Cfg.WithAlias(alias)
			}
			h.HandleAsync(ctx, job)
			// An Event invocation's return value is discarded by Lambda; what
			// matters is that we do not return until the work is done, because
			// returning is what freezes the sandbox.
			return []byte(`{"ok":true}`), nil
		}

		var req events.APIGatewayProxyRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("payload was not an API Gateway proxy event: %w", err)
		}
		// Alias-aware: a no-op today (four discrete functions), load-bearing if
		// the one-function-two-aliases proposal is approved.
		if alias := inv.Alias(); alias != "" {
			h.Cfg = h.Cfg.WithAlias(alias)
		}
		return json.Marshal(h.Handle(ctx, req))
	})
}

func ackOnly() bool { return os.Getenv("RELEASEBOT_ACK_ONLY") == "1" }
