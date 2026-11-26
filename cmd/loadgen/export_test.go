package main

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestHistogramIndexMonotoneAndBounded(t *testing.T) {
	prev := -1
	for ms := 0.01; ms < 200000; ms *= 1.01 {
		i := histIndex(ms)
		if i < prev {
			t.Fatalf("histIndex not monotone at %v", ms)
		}
		prev = i
		if i < 0 || i >= histBuckets {
			t.Fatalf("index %d out of range at %v", i, ms)
		}
	}
}

func TestHistogramUpperBoundWithinFivePercent(t *testing.T) {
	for _, ms := range []float64{0.2, 1, 5.5, 22, 24.7, 120, 424.1, 909.5, 5000} {
		upper := histBaseMs * math.Pow(histRatio, float64(histIndex(ms)))
		if upper < ms-1e-9 || upper > ms*histRatio+1e-9 {
			t.Errorf("%v ms -> upper bound %v is not within +5%%", ms, upper)
		}
	}
}

func TestSamplingAndClientsDoNotChangeSchedule(t *testing.T) {
	base := buildSchedule(scenario(), 0)
	s := scenario()
	s.Mix.Clients = 50
	with := buildSchedule(s, 0.1)
	if len(base) != len(with) {
		t.Fatalf("arrival count changed: %d vs %d", len(base), len(with))
	}
	for i := range base {
		if base[i].T != with[i].T || base[i].Long != with[i].Long {
			t.Fatal("clients/sampling must not perturb arrival times or the long/short mix")
		}
	}
	sampled, clients := 0, map[int]bool{}
	for _, a := range with {
		if a.Sample {
			sampled++
		}
		if a.Client < 1 || a.Client > 50 {
			t.Fatalf("client id %d out of range", a.Client)
		}
		clients[a.Client] = true
	}
	if f := float64(sampled) / float64(len(with)); math.Abs(f-0.1) > 0.02 {
		t.Errorf("sample fraction %.3f, want ~0.1", f)
	}
	if len(clients) < 40 {
		t.Errorf("only %d distinct clients", len(clients))
	}
	if !reflect.DeepEqual(buildSchedule(s, 0.1), with) {
		t.Fatal("schedule with clients and sampling must be deterministic")
	}
}

func TestCategorize(t *testing.T) {
	cases := []struct {
		status int
		err    error
		want   string
	}{
		{200, nil, ""}, {302, nil, ""}, {404, nil, "http_4xx"}, {503, nil, "http_5xx"},
		{0, context.DeadlineExceeded, "timeout"}, {0, errors.New("connection refused"), "transport"},
	}
	for _, c := range cases {
		if got := categorize(c.status, c.err); got != c.want {
			t.Errorf("categorize(%d,%v)=%q want %q", c.status, c.err, got, c.want)
		}
	}
}

func TestParseBackendName(t *testing.T) {
	if n := parseBackendName([]byte("Response from Backend-2 (Port 8082) | Request Count: 5\n")); n != "Backend-2" {
		t.Fatalf("got %q", n)
	}
	if n := parseBackendName([]byte("garbage")); n != "" {
		t.Fatalf("got %q", n)
	}
	if n := parseBackendName([]byte("Response from X")); n != "X" {
		t.Fatalf("got %q", n)
	}
}

func TestSyntheticIPIsPrivateRange(t *testing.T) {
	if ip := syntheticIP(258); ip != "10.0.1.2" {
		t.Fatalf("got %s", ip)
	}
}

func TestValidationRejectsBadClients(t *testing.T) {
	s := scenario()
	s.Mix.Clients = -1
	if s.validate() == nil {
		t.Fatal("negative clients must be rejected")
	}
}
