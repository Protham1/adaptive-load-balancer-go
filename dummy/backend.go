// Package dummy is a controllable backend used for demonstrations and the
// Phase 13 fault-injection experiments. It models a service with bounded
// capacity (concurrency / service time) and exposes a fault-control endpoint:
//
// Fault control lives on a separate control port (default: service port + 1000)
// that stays up even when the service listener is "down", so a failed backend can
// be recovered remotely:
//
//	GET  :ctrl/__fault   current fault settings
//	POST :ctrl/__fault   {"delay_ms":300,"error_rate":0.2,"cpu_burn":0.8,"cpu_threads":4,"down":false,
//	                 "concurrency":8,"seed":7}   (every field optional; unspecified fields are unchanged)
//
// "down": true closes the listener (connection refused, so the load balancer's
// TCP health check marks it down); false re-opens it. Error injection uses a
// seeded generator so a fixed seed reproduces the same error pattern.
package dummy

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Config sets the nominal behavior of a backend.
type Config struct {
	Name         string
	Port         string
	ControlPort  string        // fault-control port; default Port+1000
	ServiceTime  time.Duration // base processing time per request (sleep)
	CPUWork      time.Duration // real CPU work per request (fixed iterations, so contention slows it)
	Concurrency  int           // simultaneous requests served; excess requests queue
	QueueTimeout time.Duration // a request that waits longer gets 503 (overload)
	Seed         int64
}

// DefaultConfig returns a backend of moderate capacity.
func DefaultConfig(name, port string) Config {
	return Config{Name: name, Port: port, ServiceTime: 20 * time.Millisecond, CPUWork: 300 * time.Microsecond, Concurrency: 8,
		QueueTimeout: 500 * time.Millisecond, Seed: 1}
}

// Faults are the currently injected faults.
type Faults struct {
	DelayMs     float64 `json:"delay_ms"`
	ErrorRate   float64 `json:"error_rate"`
	CPUBurn     float64 `json:"cpu_burn"`
	CPUThreads  int     `json:"cpu_threads"`
	Down        bool    `json:"down"`
	Concurrency int     `json:"concurrency"`
}

type faultPatch struct {
	DelayMs     *float64 `json:"delay_ms"`
	ErrorRate   *float64 `json:"error_rate"`
	CPUBurn     *float64 `json:"cpu_burn"`
	CPUThreads  *int     `json:"cpu_threads"`
	Down        *bool    `json:"down"`
	Concurrency *int     `json:"concurrency"`
	Seed        *int64   `json:"seed"`
}

// Backend is a running dummy backend.
type Backend struct {
	cfg Config

	mu       sync.Mutex
	faults   Faults
	rng      *rand.Rand
	sem      chan struct{}
	srv      *http.Server
	burnStop chan struct{}
	ctrl     *http.Server

	served  atomic.Uint64
	errored atomic.Uint64
}

// Start launches the backend and returns it. The listener is opened before
// returning, so the port is bound when Start returns.
func Start(cfg Config) (*Backend, error) {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.ControlPort == "" {
		p, err := strconv.Atoi(cfg.Port)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q", cfg.Port)
		}
		cfg.ControlPort = strconv.Itoa(p + 1000)
	}
	b := &Backend{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed)),
		faults: Faults{Concurrency: cfg.Concurrency, CPUThreads: 1}}
	b.sem = make(chan struct{}, cfg.Concurrency)
	cln, err := net.Listen("tcp", ":"+cfg.ControlPort)
	if err != nil {
		return nil, err
	}
	cmux := http.NewServeMux()
	cmux.HandleFunc("/__fault", b.handleFault)
	b.ctrl = &http.Server{Handler: cmux}
	go func() { _ = b.ctrl.Serve(cln) }()
	if err := b.listen(); err != nil {
		_ = b.ctrl.Close()
		return nil, err
	}
	return b, nil
}

// Served returns the number of successful responses.
func (b *Backend) Served() uint64 { return b.served.Load() }

// Faults returns the current fault settings.
func (b *Backend) Faults() Faults {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.faults
}

func (b *Backend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
	})
	mux.HandleFunc("/", b.handleWork)
	return mux
}

func (b *Backend) listen() error {
	ln, err := net.Listen("tcp", ":"+b.cfg.Port)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: b.handler()}
	b.mu.Lock()
	b.srv = srv
	b.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (b *Backend) handleWork(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	f := b.faults
	sem := b.sem
	errRoll := b.rng.Float64()
	b.mu.Unlock()

	// bounded capacity: wait for a slot, else shed load with 503
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-time.After(b.cfg.QueueTimeout):
		b.errored.Add(1)
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
		return
	case <-r.Context().Done():
		return
	}

	if b.cfg.CPUWork > 0 {
		spin(b.cfg.CPUWork)
	}
	d := b.cfg.ServiceTime + time.Duration(f.DelayMs*float64(time.Millisecond))
	if s := r.Header.Get("X-Simulated-Delay"); s != "" {
		if x, err := time.ParseDuration(s); err == nil {
			d += x
		}
	} else if s := r.URL.Query().Get("delay"); s != "" {
		if x, err := time.ParseDuration(s); err == nil {
			d += x
		}
	}
	select {
	case <-time.After(d):
	case <-r.Context().Done():
		return
	}
	if f.ErrorRate > 0 && errRoll < f.ErrorRate {
		b.errored.Add(1)
		http.Error(w, "injected error", http.StatusServiceUnavailable)
		return
	}
	n := b.served.Add(1)
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprintf(w, "Response from %s (Port %s) | Request Count: %d\n", b.cfg.Name, b.cfg.Port, n)
}

func (b *Backend) handleFault(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var p faultPatch
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&p); err != nil {
			http.Error(w, "invalid fault body", http.StatusBadRequest)
			return
		}
		if err := b.apply(p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(b.Faults())
}

// Apply is the programmatic equivalent of POST /__fault.
func (b *Backend) Apply(p faultPatch) error { return b.apply(p) }

func (b *Backend) apply(p faultPatch) error {
	if p.ErrorRate != nil && (*p.ErrorRate < 0 || *p.ErrorRate > 1) {
		return fmt.Errorf("error_rate must be in [0,1]")
	}
	if p.CPUBurn != nil && (*p.CPUBurn < 0 || *p.CPUBurn > 1) {
		return fmt.Errorf("cpu_burn must be in [0,1]")
	}
	if p.DelayMs != nil && (*p.DelayMs < 0 || *p.DelayMs > 60000) {
		return fmt.Errorf("delay_ms must be in [0,60000]")
	}
	if p.Concurrency != nil && (*p.Concurrency < 1 || *p.Concurrency > 100000) {
		return fmt.Errorf("concurrency must be in [1,100000]")
	}
	if p.CPUThreads != nil && (*p.CPUThreads < 1 || *p.CPUThreads > 256) {
		return fmt.Errorf("cpu_threads must be in [1,256]")
	}
	b.mu.Lock()
	if p.DelayMs != nil {
		b.faults.DelayMs = *p.DelayMs
	}
	if p.ErrorRate != nil {
		b.faults.ErrorRate = *p.ErrorRate
	}
	if p.Seed != nil {
		b.rng = rand.New(rand.NewSource(*p.Seed))
	}
	if p.Concurrency != nil {
		b.faults.Concurrency = *p.Concurrency
		b.sem = make(chan struct{}, *p.Concurrency)
	}
	if p.CPUThreads != nil {
		b.faults.CPUThreads = *p.CPUThreads
	}
	burn, threads := b.faults.CPUBurn, b.faults.CPUThreads
	if p.CPUBurn != nil {
		burn = *p.CPUBurn
		b.faults.CPUBurn = burn
	}
	wantDown, isDown := b.faults.Down, b.srv == nil
	if p.Down != nil {
		wantDown = *p.Down
		b.faults.Down = wantDown
	}
	b.mu.Unlock()

	if p.CPUBurn != nil || p.CPUThreads != nil {
		b.setBurner(burn, threads)
	}
	if p.Down != nil {
		if wantDown && !isDown {
			b.mu.Lock()
			srv := b.srv
			b.srv = nil
			b.mu.Unlock()
			if srv != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_ = srv.Shutdown(ctx)
				cancel()
				_ = srv.Close() // drop in-flight connections: a crashed backend does not drain
			}
		} else if !wantDown && isDown {
			return b.listen()
		}
	}
	return nil
}

// setBurner (re)starts the controlled CPU burner: `threads` goroutines, each
// busy for `duty` of every 10 ms slot. duty 0 stops it.
func (b *Backend) setBurner(duty float64, threads int) {
	b.mu.Lock()
	if b.burnStop != nil {
		close(b.burnStop)
		b.burnStop = nil
	}
	if duty <= 0 {
		b.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	b.burnStop = stop
	b.mu.Unlock()
	if threads > runtime.NumCPU()*4 {
		threads = runtime.NumCPU() * 4
	}
	for i := 0; i < threads; i++ {
		go func() {
			const slot = 10 * time.Millisecond
			busy := time.Duration(duty * float64(slot))
			x := 1.0
			for {
				select {
				case <-stop:
					return
				default:
				}
				end := time.Now().Add(busy)
				for time.Now().Before(end) {
					x = x*1.0000001 + 1e-9 // keep the loop from being optimized away
				}
				if idle := slot - busy; idle > 0 {
					time.Sleep(idle)
				}
			}
		}()
	}
}

// Stop shuts the backend down and stops the CPU burner.
func (b *Backend) Stop() {
	_ = b.apply(faultPatch{CPUBurn: ptr(0.0), Down: ptr(true)})
	if b.ctrl != nil {
		_ = b.ctrl.Close()
	}
}

// ControlURL returns the base URL of the fault-control endpoint.
func (b *Backend) ControlURL() string { return "http://127.0.0.1:" + b.cfg.ControlPort }

var (
	calibrateOnce sync.Once
	itersPerMicro float64
	sink          atomic.Uint64
)

// spin performs a FIXED amount of CPU work (calibrated once to ~d on an idle core).
// Because the iteration count is fixed, CPU contention makes it take longer in
// wall-clock time, which is what a CPU-pressure experiment needs.
func spin(d time.Duration) {
	calibrateOnce.Do(func() {
		const probe = 2_000_000
		t := time.Now()
		x := 1.0
		for i := 0; i < probe; i++ {
			x = x*1.0000001 + 1e-9
		}
		sink.Store(uint64(x))
		itersPerMicro = float64(probe) / float64(time.Since(t).Microseconds()+1)
	})
	n := int(itersPerMicro * float64(d.Microseconds()))
	x := 1.0
	for i := 0; i < n; i++ {
		x = x*1.0000001 + 1e-9
	}
	sink.Store(uint64(x))
}

func ptr[T any](v T) *T { return &v }
