// Package sqsq sends to an SQS queue and parses the SQS event Lambda delivers.
//
// SendMessage uses SQS's AWS-JSON protocol (application/x-amz-json-1.0), signed
// with internal/awssig, so the project stays free of the AWS SDK.
package sqsq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/awssig"
)

// Sender sends one message body to QueueURL.
type Sender interface {
	Send(ctx context.Context, body string) error
}

type Client struct {
	QueueURL string
	HTTP     *http.Client
	// Endpoint overrides the SQS endpoint (tests). Default: the queue URL's host.
	Endpoint string
}

func New(queueURL string) *Client {
	return &Client{QueueURL: queueURL, HTTP: &http.Client{Timeout: 3 * time.Second}}
}

func (c *Client) Send(ctx context.Context, body string) error {
	if c.QueueURL == "" {
		return fmt.Errorf("sqs: ASYNC_QUEUE_URL is not set")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		return fmt.Errorf("sqs: no AWS_REGION")
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		u, err := url.Parse(c.QueueURL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("sqs: bad queue URL")
		}
		endpoint = "https://" + u.Host + "/"
	}
	creds, err := awssig.FromEnvironment()
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]string{"QueueUrl": c.QueueURL, "MessageBody": body})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS.SendMessage")
	awssig.Sign(req, raw, creds, "sqs", region, time.Now())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("sqs: send: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		kind := resp.Header.Get("X-Amzn-Errortype")
		if i := strings.IndexByte(kind, ':'); i > 0 {
			kind = kind[:i]
		}
		return fmt.Errorf("sqs: SendMessage returned %d (%s)", resp.StatusCode, kind)
	}
	return nil
}

// Record is one message of an SQS event.
type Record struct {
	MessageID   string `json:"messageId"`
	Body        string `json:"body"`
	EventSource string `json:"eventSource"`
}

// Parse returns the records if payload is an SQS event (every record has
// eventSource aws:sqs), else ok=false.
func Parse(payload []byte) ([]Record, bool) {
	var ev struct {
		Records []Record `json:"Records"`
	}
	if json.Unmarshal(payload, &ev) != nil || len(ev.Records) == 0 {
		return nil, false
	}
	for _, r := range ev.Records {
		if r.EventSource != "aws:sqs" {
			return nil, false
		}
	}
	return ev.Records, true
}
