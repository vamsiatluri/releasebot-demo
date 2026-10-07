// Package events carries the minimum API Gateway payload shapes ReleaseBot needs.
//
// Deliberately hand-rolled instead of pulling aws-lambda-go: provided.al2 is a
// custom runtime, so the contract we actually have to honour is the Lambda
// Runtime API over HTTP, not a vendor SDK. Keeping it stdlib-only means the
// build is hermetic -- no module proxy, no dependency CVE surface on a function
// that holds a GitHub admin token.
package events

// APIGatewayProxyRequest is the proxy-integration event for REST (v1) APIs.
// Slack posts application/x-www-form-urlencoded; Jira posts JSON. Both arrive
// here, and both can arrive base64-encoded if binary media types are set.
type APIGatewayProxyRequest struct {
	Resource              string            `json:"resource"`
	Path                  string            `json:"path"`
	HTTPMethod            string            `json:"httpMethod"`
	Headers               map[string]string `json:"headers"`
	QueryStringParameters map[string]string `json:"queryStringParameters"`
	PathParameters        map[string]string `json:"pathParameters"`
	StageVariables        map[string]string `json:"stageVariables"`
	RequestContext        ProxyContext      `json:"requestContext"`
	Body                  string            `json:"body"`
	IsBase64Encoded       bool              `json:"isBase64Encoded"`
}

type ProxyContext struct {
	RequestID string `json:"requestId"`
	Stage     string `json:"stage"`
	APIID     string `json:"apiId"`
	AccountID string `json:"accountId"`
}

type APIGatewayProxyResponse struct {
	StatusCode      int               `json:"statusCode"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            string            `json:"body"`
	IsBase64Encoded bool              `json:"isBase64Encoded,omitempty"`
}

// Header does a case-insensitive lookup. API Gateway preserves the client's
// casing, and Slack sends `X-Slack-Signature` while a curl smoke test will
// usually send `x-slack-signature`. Looking up the literal key is a classic
// way to ship a signature check that silently never runs.
func (r APIGatewayProxyRequest) Header(name string) string {
	for k, v := range r.Headers {
		if equalFold(k, name) {
			return v
		}
	}
	return ""
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func JSON(status int, body string) APIGatewayProxyResponse {
	return APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       body,
	}
}

func Text(status int, body string) APIGatewayProxyResponse {
	return APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       body,
	}
}
