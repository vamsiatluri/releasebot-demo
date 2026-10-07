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

	runtime.Start(func(ctx context.Context, inv runtime.Invocation, payload []byte) ([]byte, error) {
		if initErr != nil {
			return nil, fmt.Errorf("initialisation failed: %w", initErr)
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
