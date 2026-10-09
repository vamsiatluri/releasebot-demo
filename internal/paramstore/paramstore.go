// Package paramstore reads a function's per-environment configuration from
// SSM Parameter Store at INIT.
//
// WHY THIS EXISTS, in one sentence: so a published version freezes a POINTER to
// its configuration rather than the configuration itself.
//
// With values baked into the function's environment variables, a Lambda version
// freezes code AND config together. That is why rolling an alias back also
// rolls the config back -- and why rotating a secret quietly invalidates every
// older version as a rollback target, which is exactly the day you need one.
// Their cutover plan rotates secrets at steps 6, 7 and 8.
//
// Reading the values here instead means the version freezes `RARC_ENV=prod`,
// and the values behind `/releasebot/prod/` can be rotated without stranding a
// single rollback target.
//
// ⚠️ This is read ONCE, during the Lambda INIT phase, not per invocation. INIT
// has its own 10s budget and is not billed per request under provisioned
// concurrency; a fetch inside the handler would pay Parameter Store's latency
// on every single call and put its availability in front of ours.
package paramstore

import (
	"bytes"
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

// Values is a flat map of the last path segment to its value, so
// /releasebot/prod/default-owner arrives as "default-owner".
type Values map[string]string

type Result struct {
	Path   string // the prefix actually read
	Values Values
	Source string // "ssm" when the fetch succeeded, "env" when it was skipped
}

// Load reads every parameter under /releasebot/<env>/.
//
// A failure here is NOT fatal and must not be: the function still holds working
// environment variables, and a Parameter Store blip should degrade the service
// to its previous behaviour rather than take it down. The caller records which
// source won, and the health endpoint reports it, so "we silently fell back"
// can never be invisible.
func Load(ctx context.Context, env string) (Result, error) {
	path := "/releasebot/" + env + "/"
	res := Result{Path: path, Values: Values{}, Source: "env"}

	region := os.Getenv("AWS_REGION")
	if region == "" {
		return res, fmt.Errorf("no AWS_REGION")
	}
	creds, err := awssig.FromEnvironment()
	if err != nil {
		return res, err
	}

	// GetParametersByPath is paginated. A single page is enough for ten keys,
	// but a loop that silently reads only the first page is the kind of thing
	// that works until someone adds an eleventh parameter.
	var next string
	for {
		body, _ := json.Marshal(map[string]any{
			"Path":           path,
			"Recursive":      false,
			"WithDecryption": true,
			"MaxResults":     10,
			"NextToken":      next,
		})
		if next == "" {
			body, _ = json.Marshal(map[string]any{
				"Path": path, "Recursive": false, "WithDecryption": true, "MaxResults": 10,
			})
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://ssm."+region+".amazonaws.com/", bytes.NewReader(body))
		if err != nil {
			return res, err
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
		req.Header.Set("X-Amz-Target", "AmazonSSM.GetParametersByPath")
		awssig.Sign(req, body, creds, "ssm", region, time.Now())

		resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
		if err != nil {
			return res, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			// Never echo the body: an SSM error names the parameter ARN and the
			// caller identity.
			return res, fmt.Errorf("ssm returned %d", resp.StatusCode)
		}

		var out struct {
			Parameters []struct {
				Name  string `json:"Name"`
				Value string `json:"Value"`
			} `json:"Parameters"`
			NextToken string `json:"NextToken"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return res, fmt.Errorf("unreadable ssm response")
		}
		for _, p := range out.Parameters {
			res.Values[p.Name[strings.LastIndex(p.Name, "/")+1:]] = p.Value
		}
		if out.NextToken == "" {
			break
		}
		next = out.NextToken
	}

	res.Source = "ssm"
	return res, nil
}
