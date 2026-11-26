package agent

import (
	"bytes"
	"context"
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
