// Command localdev runs both ReleaseBot handlers behind a local HTTP server
// that speaks API Gateway's proxy-integration shape.
//
// The point is to exercise the deployed code path -- same app.Build, same
// handler, same signature verification -- with no AWS account, no cost, and no
// chance of touching a live environment. Everything the migration has to prove
// about behaviour can be rehearsed here first.
//
//	go run ./cmd/localdev            # listens on :8080
//	POST /beta/cut    -> cutRelease
//	POST /beta/merge  -> releaseAutomationMergeback
//	GET  /beta/health -> both
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/app"
	"github.com/vamsiatluri/releasebot-migration/internal/events"
	"github.com/vamsiatluri/releasebot-migration/internal/handler"
)

func main() {
	addr := envOr("LISTEN_ADDR", ":8080")
	stage := envOr("RARC_ENV", "test")
	ackOnly := os.Getenv("RELEASEBOT_ACK_ONLY") == "1"

	ctx := context.Background()
	cut, err := app.Build(ctx, handler.ActionCut, "", ackOnly)
	if err != nil {
		log.Fatalf("building cutRelease: %v", err)
	}
	merge, err := app.Build(ctx, handler.ActionMergeback, "", ackOnly)
	if err != nil {
		log.Fatalf("building releaseAutomationMergeback: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/beta/cut", proxy(cut, stage))
	mux.Handle("/beta/merge", proxy(merge, stage))
	mux.Handle("/beta/health", proxy(cut, stage))

	fmt.Printf("releasebot localdev listening on %s  (stage=%s, ack_only=%v)\n", addr, stage, ackOnly)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// proxy converts a real HTTP request into the event API Gateway would deliver.
func proxy(h *handler.Handler, stage string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		hdrs := map[string]string{}
		for k, v := range r.Header {
			if len(v) > 0 {
				hdrs[k] = v[0]
			}
		}
		qs := map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				qs[k] = v[0]
			}
		}
		ev := events.APIGatewayProxyRequest{
			Resource:              r.URL.Path,
			Path:                  r.URL.Path,
			HTTPMethod:            r.Method,
			Headers:               hdrs,
			QueryStringParameters: qs,
			Body:                  string(body),
			RequestContext: events.ProxyContext{
				RequestID: fmt.Sprintf("local-%d", time.Now().UnixNano()),
				Stage:     stage,
				APIID:     "localdev",
				AccountID: "000000000000",
			},
		}
		resp := h.Handle(r.Context(), ev)
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.StatusCode)
		io.WriteString(w, resp.Body)

		// Mirror the response as a structured line so a terminal transcript of
		// a local run reads the same way CloudWatch will.
		rec, _ := json.Marshal(map[string]any{
			"local_request": strings.TrimPrefix(r.URL.Path, "/"),
			"status":        resp.StatusCode,
		})
		fmt.Println(string(rec))
	})
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
