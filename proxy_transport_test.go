package main

import (
	"adaptive-load/strategy"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

// Regression: the reverse proxy must reuse backend connections under concurrency. With Go's default
// transport (2 idle connections per host) every round of concurrent requests re-dialled the backend,
// which dominated the load balancer's CPU in the experiments.
func TestProxyReusesBackendConnections(t *testing.T) {
	var conns atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	counting := httptest.NewUnstartedServer(handler)
	counting.Config.ConnState = func(_ netConn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	counting.Start()
	defer counting.Close()

	u, _ := url.Parse(counting.URL)
	pool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	pool.AddBackend(NewBackendNamedWithWeight(u, "B", 1))
	lb := httptest.NewServer(lbHandler(pool))
	defer lb.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	const concurrency, rounds = 12, 15
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := client.Get(lb.URL + "/work")
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}()
		}
		wg.Wait()
	}
	// default transport: ~ 12 + 14*10 = 150 dials; pooled transport: about one per concurrent request
	if n := conns.Load(); n > 2*concurrency {
		t.Fatalf("backend saw %d new connections for %d requests: the proxy is not reusing connections", n, concurrency*rounds)
	}
}

type netConn = net.Conn
