package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureDir = "../testdata/contract"

func fixtures(t *testing.T, sub, prefix string) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(fixtureDir, sub, prefix+"*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures for %s/%s* (run scripts/gen_contract_fixtures.py)", sub, prefix)
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = b
	}
	return out
}

func strictDecodeRequest(b []byte) (*DecisionRequest, error) { return DecodeRequest(b) }

func baseRequest(t *testing.T) *DecisionRequest {
	t.Helper()
	r, err := strictDecodeRequest(fixtures(t, "valid", "request_basic")["request_basic.json"])
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestValidRequestFixturesAccepted(t *testing.T) {
	for name, b := range fixtures(t, "valid", "request_") {
		if _, err := strictDecodeRequest(b); err != nil {
			t.Errorf("%s should be valid: %v", name, err)
		}
	}
}

func TestInvalidRequestFixturesRejected(t *testing.T) {
	for name, b := range fixtures(t, "invalid", "request_") {
		if _, err := strictDecodeRequest(b); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestValidResponseFixturesAccepted(t *testing.T) {
	req := baseRequest(t)
	for name, b := range fixtures(t, "valid", "response_") {
		if _, err := ParseResponse(b, req); err != nil {
			t.Errorf("%s should be valid: %v", name, err)
		}
	}
}

func TestInvalidResponseFixturesRejected(t *testing.T) {
	req := baseRequest(t)
	for name, b := range fixtures(t, "invalid", "response_") {
		if _, err := ParseResponse(b, req); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestStaleDecisionIsDistinguished(t *testing.T) {
	req := baseRequest(t)
	b := fixtures(t, "invalid", "response_stale_decision_id")["response_stale_decision_id.json"]
	if _, err := ParseResponse(b, req); err != ErrStaleDecision {
		t.Fatalf("want ErrStaleDecision, got %v", err)
	}
}

func TestMarshalNeverEmitsNonFinite(t *testing.T) {
	req := baseRequest(t)
	req.Features.Global.RPSNormalized = nan()
	if _, err := req.Marshal(); err == nil {
		t.Fatal("NaN feature must be rejected before encoding")
	}
}

func nan() float64 { var z float64; return z / z }

func TestWireNamesOnlyAcceptedStrategies(t *testing.T) {
	req := baseRequest(t)
	req.CurrentStrategy = "Round-Robin" // display name, not wire name
	if req.Validate() == nil {
		t.Fatal("display names must not be accepted on the wire")
	}
}

// ---------------------------------------------------------------- client behavior
func serverReturning(t *testing.T, code int, body func(*DecisionRequest) []byte, delay time.Duration) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/decision" {
			http.NotFound(w, r)
			return
		}
		raw, _ := readAll(r)
		req, err := strictDecodeRequest(raw)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		time.Sleep(delay)
		w.WriteHeader(code)
		_, _ = w.Write(body(req))
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

func readAll(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, MaxBodyBytes))
	return buf.Bytes(), err
}

func okBody(strategy string) func(*DecisionRequest) []byte {
	return func(r *DecisionRequest) []byte {
		b, _ := json.Marshal(DecisionResponse{SchemaVersion: 1, DecisionID: r.DecisionID, Strategy: strategy,
			ModelVersion: "test-v1", Diagnostic: "ok"})
		return b
	}
}

func TestClientHappyPath(t *testing.T) {
	s, _ := serverReturning(t, 200, okBody("least_connections"), 0)
	resp, err := NewClient(s.URL, time.Second).Decide(context.Background(), baseRequest(t))
	if err != nil || resp.Strategy != "least_connections" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func kindOf(err error) FailureKind {
	if ce, ok := err.(*CallError); ok {
		return ce.Kind
	}
	return ""
}

func TestClientTimeoutIsBounded(t *testing.T) {
	s, _ := serverReturning(t, 200, okBody("round_robin"), 600*time.Millisecond)
	start := time.Now()
	_, err := NewClient(s.URL, 100*time.Millisecond).Decide(context.Background(), baseRequest(t))
	if kindOf(err) != FailTimeout {
		t.Fatalf("want timeout, got %v", err)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Fatalf("did not return promptly: %v", time.Since(start))
	}
}

func TestClientFailureClasses(t *testing.T) {
	req := baseRequest(t)
	s, _ := serverReturning(t, 500, func(*DecisionRequest) []byte { return []byte("boom") }, 0)
	if k := kindOf(errOf(NewClient(s.URL, time.Second).Decide(context.Background(), req))); k != FailStatus {
		t.Fatalf("500 -> %v", k)
	}
	s2, _ := serverReturning(t, 200, func(*DecisionRequest) []byte { return []byte(`{"strategy":"magic"}`) }, 0)
	if k := kindOf(errOf(NewClient(s2.URL, time.Second).Decide(context.Background(), req))); k != FailMalformed {
		t.Fatalf("garbage -> %v", k)
	}
	s3, _ := serverReturning(t, 200, func(r *DecisionRequest) []byte {
		b, _ := json.Marshal(DecisionResponse{SchemaVersion: 1, DecisionID: "other", Strategy: "round_robin", ModelVersion: "m", Diagnostic: "ok"})
		return b
	}, 0)
	if k := kindOf(errOf(NewClient(s3.URL, time.Second).Decide(context.Background(), req))); k != FailStale {
		t.Fatalf("other id -> %v", k)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	if k := kindOf(errOf(NewClient(url, time.Second).Decide(context.Background(), req))); k != FailConnect {
		t.Fatalf("closed port -> %v", k)
	}
	bad := *req
	bad.DecisionID = "no spaces allowed"
	if k := kindOf(errOf(NewClient(s.URL, time.Second).Decide(context.Background(), &bad))); k != FailRequest {
		t.Fatalf("invalid request -> %v", k)
	}
}

func errOf(_ *DecisionResponse, err error) error { return err }

func TestClientRejectsOversizedResponse(t *testing.T) {
	s, _ := serverReturning(t, 200, func(*DecisionRequest) []byte { return []byte(strings.Repeat("x", MaxBodyBytes+10)) }, 0)
	if k := kindOf(errOf(NewClient(s.URL, time.Second).Decide(context.Background(), baseRequest(t)))); k != FailMalformed {
		t.Fatalf("oversized -> %v", k)
	}
}
