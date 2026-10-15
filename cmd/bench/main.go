package main

import (
	"adaptive-load/strategy"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type StrategyResult struct {
	Name        string
	Requests    int
	Concurrency int
	Duration    time.Duration
	Throughput  float64
	P50         time.Duration
	P95         time.Duration
	P99         time.Duration
	Errors      int64
	AllocMB     float64
	TotalAlloc  float64
}

func main() {
	log.SetOutput(io.Discard) // mute reverse proxy debug logs during run

	fmt.Println("================================================================================")
	fmt.Println(" Adaptive Load Balancer in Go — Baseline Performance Benchmark")
	fmt.Println("================================================================================")
	fmt.Printf("Go Version : %s\n", runtime.Version())
	fmt.Printf("OS/Arch    : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("CPUs       : %d logical cores\n", runtime.NumCPU())
	fmt.Println("--------------------------------------------------------------------------------")

	strategies := []struct {
		name string
		get  func() strategy.SelectionStrategy[*BackendPeer]
	}{
		{"Round-Robin (RR)", func() strategy.SelectionStrategy[*BackendPeer] {
			return strategy.NewRoundRobin[*BackendPeer]()
		}},
		{"Least-Connections (LC)", func() strategy.SelectionStrategy[*BackendPeer] {
			return strategy.NewLeastConnections[*BackendPeer]()
		}},
		{"Smooth-Weighted-Round-Robin (SWRR)", func() strategy.SelectionStrategy[*BackendPeer] {
			return strategy.NewSmoothWeightedRoundRobin[*BackendPeer]()
		}},
		{"Consistent-Hash (CH)", func() strategy.SelectionStrategy[*BackendPeer] {
			return strategy.NewConsistentHash[*BackendPeer](100)
		}},
	}

	const totalRequests = 5000
	const concurrency = 20

	var results []StrategyResult

	for _, s := range strategies {
		res := benchmarkStrategy(s.name, s.get(), totalRequests, concurrency)
		results = append(results, res)
	}

	// Print Markdown Table
	table := formatResultsTable(results)
	fmt.Print(table)

	// Save raw benchmark output to benchmarks/baseline_raw.txt
	_ = os.MkdirAll("benchmarks", 0755)
	rawPath := filepath.Join("benchmarks", "baseline_raw.txt")
	rawContent := fmt.Sprintf("Timestamp: %s\nGo: %s\nOS: %s\nArch: %s\nCPUs: %d\nRequests Per Strategy: %d\nConcurrency: %d\n\n%s",
		time.Now().Format(time.RFC3339), runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(),
		totalRequests, concurrency, table)

	if err := os.WriteFile(rawPath, []byte(rawContent), 0644); err == nil {
		fmt.Printf("\nRaw benchmark results saved to: %s\n", rawPath)
	}
}

type BackendPeer struct {
	URL         *url.URL
	Name        string
	Alive       bool
	weight      int
	conns       int64
	reqs        uint64
	Server      *httptest.Server
	mu          sync.RWMutex
}

func (b *BackendPeer) IsAlive() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.Alive
}

func (b *BackendPeer) GetActiveConnections() int64 {
	return atomic.LoadInt64(&b.conns)
}

func (b *BackendPeer) IncConns() int64 {
	return atomic.AddInt64(&b.conns, 1)
}

func (b *BackendPeer) DecConns() int64 {
	return atomic.AddInt64(&b.conns, -1)
}

func (b *BackendPeer) IncTotalRequests() uint64 {
	return atomic.AddUint64(&b.reqs, 1)
}

func (b *BackendPeer) GetWeight() int {
	return b.weight
}

func (b *BackendPeer) GetID() string {
	return b.URL.String()
}

func benchmarkStrategy(name string, strat strategy.SelectionStrategy[*BackendPeer], totalReqs, concurrency int) StrategyResult {
	// Spin up 3 backend test servers
	const numServers = 3
	peers := make([]*BackendPeer, numServers)
	weights := []int{3, 2, 1}

	for i := 0; i < numServers; i++ {
		idx := i
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf("backend-%d", idx)))
		}))
		u, _ := url.Parse(ts.URL)
		peers[i] = &BackendPeer{
			URL:    u,
			Name:   fmt.Sprintf("backend-%d", idx),
			Alive:  true,
			weight: weights[i],
			Server: ts,
		}
	}
	defer func() {
		for _, p := range peers {
			p.Server.Close()
		}
	}()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 5 * time.Second,
	}

	latencies := make([]time.Duration, totalReqs)
	var errCount int64
	reqsPerWorker := totalReqs / concurrency

	var mStart runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mStart)

	startWall := time.Now()
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for w := 0; w < concurrency; w++ {
		go func(workerID int) {
			defer wg.Done()
			offset := workerID * reqsPerWorker
			for i := 0; i < reqsPerWorker; i++ {
				reqIdx := offset + i
				var peer *BackendPeer
				if keyed, ok := strat.(strategy.KeyedSelectionStrategy[*BackendPeer]); ok {
					clientIP := fmt.Sprintf("192.168.1.%d", (workerID*10+i)%254)
					peer = keyed.SelectNextBackendWithKey(peers, clientIP)
				} else {
					peer = strat.SelectNextBackend(peers)
				}

				if peer == nil {
					atomic.AddInt64(&errCount, 1)
					continue
				}

				peer.IncConns()
				peer.IncTotalRequests()
				t0 := time.Now()

				resp, err := client.Get(peer.URL.String())
				if err != nil {
					atomic.AddInt64(&errCount, 1)
				} else {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						atomic.AddInt64(&errCount, 1)
					}
				}

				lat := time.Since(t0)
				peer.DecConns()
				latencies[reqIdx] = lat
			}
		}(w)
	}

	wg.Wait()
	totalElapsed := time.Since(startWall)

	var mEnd runtime.MemStats
	runtime.ReadMemStats(&mEnd)

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	p50 := latencies[int(float64(totalReqs)*0.50)]
	p95 := latencies[int(float64(totalReqs)*0.95)]
	p99 := latencies[int(float64(totalReqs)*0.99)]
	throughput := float64(totalReqs) / totalElapsed.Seconds()

	allocMB := float64(mEnd.Alloc) / (1024 * 1024)
	totalAllocMB := float64(mEnd.TotalAlloc-mStart.TotalAlloc) / (1024 * 1024)

	return StrategyResult{
		Name:        name,
		Requests:    totalReqs,
		Concurrency: concurrency,
		Duration:    totalElapsed,
		Throughput:  throughput,
		P50:         p50,
		P95:         p95,
		P99:         p99,
		Errors:      errCount,
		AllocMB:     allocMB,
		TotalAlloc:  totalAllocMB,
	}
}

func formatResultsTable(results []StrategyResult) string {
	res := "\n### Baseline Performance Table\n\n"
	res += "| Strategy | Requests | Concurrency | Throughput (req/s) | Latency p50 | Latency p95 | Latency p99 | Errors | Memory Total Allocated |\n"
	res += "| :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |\n"

	for _, r := range results {
		res += fmt.Sprintf("| **%s** | %d | %d | %.1f | %s | %s | %s | %d | %.2f MB |\n",
			r.Name, r.Requests, r.Concurrency, r.Throughput,
			formatDuration(r.P50), formatDuration(r.P95), formatDuration(r.P99),
			r.Errors, r.TotalAlloc)
	}
	return res
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.2f µs", float64(d.Microseconds()))
	}
	return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000.0)
}
