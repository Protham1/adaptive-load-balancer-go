package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Decider is what the control loop needs; tests substitute fakes.
type Decider interface {
	Decide(ctx context.Context, req *DecisionRequest) (*DecisionResponse, error)
}

// FailureKind classifies API failures for logs and counters.
type FailureKind string

const (
	FailTimeout   FailureKind = "timeout"
	FailConnect   FailureKind = "connect"
	FailStatus    FailureKind = "status"
	FailMalformed FailureKind = "malformed"
	FailStale     FailureKind = "stale"
	FailRequest   FailureKind = "bad_request"
)

// CallError carries the failure class.
type CallError struct {
	Kind FailureKind
	Err  error
}

func (e *CallError) Error() string { return fmt.Sprintf("agent %s: %v", e.Kind, e.Err) }
func (e *CallError) Unwrap() error { return e.Err }

// Client calls POST {base}/v1/decision with a hard timeout and size limits.
type Client struct {
	base    string
	timeout time.Duration
	http    *http.Client
	token   string // optional bearer token (the agent API must not be exposed without authentication)
}

// SetToken sets the bearer token sent with every request ("" = none).
func (c *Client) SetToken(t string) { c.token = t }

func (c *Client) authorize(r *http.Request) {
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// NewClient creates a client. timeout bounds the whole call (connect, send, read).
func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Client{
		base:    strings.TrimRight(baseURL, "/"),
		timeout: timeout,
		http: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second, DisableCompression: true},
		},
	}
}

// Decide sends one aggregated snapshot and returns the validated response.
func (c *Client) Decide(ctx context.Context, req *DecisionRequest) (*DecisionResponse, error) {
	body, err := req.Marshal()
	if err != nil {
		return nil, &CallError{FailRequest, err}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/decision", bytes.NewReader(body))
	if err != nil {
		return nil, &CallError{FailRequest, err}
	}
	hr.Header.Set("Content-Type", "application/json")
	c.authorize(hr)
	resp, err := c.http.Do(hr)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, &CallError{FailTimeout, err}
		}
		return nil, &CallError{FailConnect, err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		kind := FailConnect
		if errors.Is(err, context.DeadlineExceeded) {
			kind = FailTimeout
		}
		return nil, &CallError{kind, err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &CallError{FailStatus, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, string(data))}
	}
	out, err := ParseResponse(data, req)
	if err != nil {
		kind := FailMalformed
		if errors.Is(err, ErrStaleDecision) {
			kind = FailStale
		}
		return nil, &CallError{kind, err}
	}
	return out, nil
}

// MaxLearnerBytes bounds the learner-introspection response (it carries up to 100 recent decisions).
const MaxLearnerBytes = 512 * 1024

// Introspector is the optional read-only / maintenance side of the agent API used by the dashboard.
type Introspector interface {
	Learner(ctx context.Context, limit int) ([]byte, error)
	Reset(ctx context.Context, archive bool, reason string) ([]byte, error)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, &CallError{FailRequest, err}
	}
	if body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	c.authorize(hr)
	resp, err := c.http.Do(hr)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, &CallError{FailTimeout, err}
		}
		return nil, &CallError{FailConnect, err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &CallError{FailConnect, err}
	}
	if int64(len(data)) > limit {
		return nil, &CallError{FailMalformed, fmt.Errorf("response exceeds %d bytes", limit)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &CallError{FailStatus, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, string(data))}
	}
	return data, nil
}

// Learner fetches GET /v1/learner?limit=N (raw JSON, passed through to the dashboard).
func (c *Client) Learner(ctx context.Context, limit int) ([]byte, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	return c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/learner?limit=%d", limit), nil, MaxLearnerBytes)
}

// Reset asks the agent to archive its model and restart learning from scratch (POST /v1/reset).
func (c *Client) Reset(ctx context.Context, archive bool, reason string) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{"archive": archive, "reason": reason})
	return c.do(ctx, http.MethodPost, "/v1/reset", body, MaxBodyBytes)
}
