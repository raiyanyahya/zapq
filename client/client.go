// Package client is the official Go client for a zapq server. It speaks
// plain HTTP, adds bearer auth, and retries throttled or failed enqueues
// with jittered exponential backoff so callers don't have to.
//
//	c := client.New("http://localhost:8080", client.WithToken(os.Getenv("ZAPQ_API_TOKEN")))
//	_ = c.Enqueue(ctx, "", []byte("hello"))          // "" is the default queue
//	msg, ok, _ := c.Dequeue(ctx, "", 5*time.Second)  // long-poll up to 5s
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/raiyanyahya/zapq/internal/frames"
)

// ErrQueueFull is returned when the server keeps answering 429 after all
// retries are spent.
var ErrQueueFull = errors.New("queue full")

// ErrUnauthorized is returned on 401 responses; retrying won't help.
var ErrUnauthorized = errors.New("unauthorized")

// Message is a payload delivered under a lease. Ack it via Client.Ack
// before the lease expires or the server will redeliver it.
type Message struct {
	Data    []byte
	LeaseID string
	Expires time.Time
}

type Client struct {
	baseURL string
	token   string
	hc      *http.Client
	retries int
}

type Option func(*Client)

// WithToken sets the bearer token sent on every request.
func WithToken(t string) Option { return func(c *Client) { c.token = t } }

// WithHTTPClient replaces the underlying HTTP client. The default has no
// timeout because long-poll dequeues hold connections open; set deadlines
// through ctx instead.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithRetries sets how many times throttled or failed enqueues are retried
// (default 4). Zero disables retrying.
func WithRetries(n int) Option { return func(c *Client) { c.retries = n } }

func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{},
		retries: 4,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// path builds an endpoint path; queue "" targets the default queue.
func (c *Client) path(queue, endpoint string) string {
	if queue == "" {
		return c.baseURL + "/" + endpoint
	}
	return c.baseURL + "/q/" + url.PathEscape(queue) + "/" + endpoint
}

func (c *Client) do(ctx context.Context, method, u string, contentType string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.hc.Do(req)
}

// backoff sleeps for attempt n with full jitter, honoring ctx.
func backoff(ctx context.Context, n int) error {
	d := time.Duration(50<<n) * time.Millisecond
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	d = time.Duration(rand.Int63n(int64(d)) + int64(d)/2)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// post sends body, retrying on 429/503 and transport errors. Note that a
// retry after a lost response can duplicate a message: producers get
// at-least-once semantics, same as any HTTP retry.
func (c *Client) post(ctx context.Context, u, contentType string, body []byte, out func(*http.Response) error) error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, http.MethodPost, u, contentType, body)
		if err == nil {
			switch {
			case resp.StatusCode == http.StatusUnauthorized:
				resp.Body.Close()
				return ErrUnauthorized
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
				resp.Body.Close()
				lastErr = ErrQueueFull
			case resp.StatusCode >= 400:
				msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				resp.Body.Close()
				return fmt.Errorf("zapq: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
			default:
				defer resp.Body.Close()
				if out != nil {
					return out(resp)
				}
				return nil
			}
		} else {
			lastErr = err
		}
		if attempt >= c.retries {
			return lastErr
		}
		if err := backoff(ctx, attempt); err != nil {
			return err
		}
	}
}

// Enqueue pushes one message onto the queue.
func (c *Client) Enqueue(ctx context.Context, queue string, payload []byte) error {
	return c.post(ctx, c.path(queue, "enqueue"), "application/octet-stream", payload, nil)
}

// EnqueueBatch pushes all payloads in a single request. The server accepts
// the whole batch or none of it.
func (c *Client) EnqueueBatch(ctx context.Context, queue string, payloads [][]byte) error {
	var buf bytes.Buffer
	if err := frames.Write(&buf, payloads); err != nil {
		return err
	}
	return c.post(ctx, c.path(queue, "enqueue/batch"), frames.ContentType, buf.Bytes(), nil)
}

func waitParam(wait time.Duration) string {
	if wait <= 0 {
		return ""
	}
	return "wait=" + url.QueryEscape(wait.String())
}

func (c *Client) get(ctx context.Context, u string) (*http.Response, error) {
	resp, err := c.do(ctx, http.MethodGet, u, "", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, ErrUnauthorized
	}
	return resp, nil
}

// Dequeue pops the next message, long-polling up to wait when the queue is
// empty. ok is false when no message arrived in time.
func (c *Client) Dequeue(ctx context.Context, queue string, wait time.Duration) (data []byte, ok bool, err error) {
	m, ok, err := c.dequeue(ctx, queue, wait, 0)
	if !ok || err != nil {
		return nil, false, err
	}
	return m.Data, true, nil
}

// DequeueLease pops the next message under a lease. Ack it via Ack before
// the lease expires or the server requeues it (at-least-once delivery).
func (c *Client) DequeueLease(ctx context.Context, queue string, wait, lease time.Duration) (*Message, bool, error) {
	if lease <= 0 {
		return nil, false, errors.New("lease duration must be positive")
	}
	m, ok, err := c.dequeue(ctx, queue, wait, lease)
	if !ok || err != nil {
		return nil, false, err
	}
	return m, true, nil
}

func (c *Client) dequeue(ctx context.Context, queue string, wait, lease time.Duration) (*Message, bool, error) {
	params := []string{}
	if p := waitParam(wait); p != "" {
		params = append(params, p)
	}
	if lease > 0 {
		params = append(params, "lease="+url.QueryEscape(lease.String()))
	}
	u := c.path(queue, "dequeue")
	if len(params) > 0 {
		u += "?" + strings.Join(params, "&")
	}
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, false, nil
	case http.StatusOK:
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, err
		}
		m := &Message{Data: data, LeaseID: resp.Header.Get("X-Lease-Id")}
		if e := resp.Header.Get("X-Lease-Expires"); e != "" {
			m.Expires, _ = time.Parse(time.RFC3339Nano, e)
		}
		return m, true, nil
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, false, fmt.Errorf("zapq: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
}

// DequeueBatch pops up to max messages in one request, long-polling up to
// wait for the first one. With lease > 0 every message carries its own
// lease ID.
func (c *Client) DequeueBatch(ctx context.Context, queue string, max int, wait, lease time.Duration) ([]Message, error) {
	u := fmt.Sprintf("%s?max=%d", c.path(queue, "dequeue/batch"), max)
	if p := waitParam(wait); p != "" {
		u += "&" + p
	}
	if lease > 0 {
		u += "&lease=" + url.QueryEscape(lease.String())
	}
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		payloads, err := frames.Read(resp.Body, frames.MaxTotalBytes)
		if err != nil {
			return nil, err
		}
		var ids []string
		if h := resp.Header.Get("X-Lease-Ids"); h != "" {
			ids = strings.Split(h, ",")
		}
		msgs := make([]Message, len(payloads))
		for i, p := range payloads {
			msgs[i] = Message{Data: p}
			if i < len(ids) {
				msgs[i].LeaseID = ids[i]
			}
		}
		return msgs, nil
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("zapq: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
}

// Ack settles a leased message for good.
func (c *Client) Ack(ctx context.Context, queue, leaseID string) error {
	return c.post(ctx, c.path(queue, "ack")+"?id="+url.QueryEscape(leaseID), "", nil, nil)
}

// Health reports whether the server answers its liveness probe.
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.get(ctx, c.baseURL+"/health")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s", resp.Status)
	}
	return nil
}
