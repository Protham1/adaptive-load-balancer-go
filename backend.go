package main

import (
	"adaptive-load/strategy"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Backend holds the data about a server backend
type Backend struct {
	URL               *url.URL
	Name              string
	Alive             bool
	mux               sync.RWMutex
	ReverseProxy      *httputil.ReverseProxy
	weight            int
	activeConnections int64
	totalRequests     uint64
}

// SetAlive sets the liveness state of the backend thread-safely
func (b *Backend) SetAlive(alive bool) {
	b.mux.Lock()
	b.Alive = alive
	b.mux.Unlock()
}

// IsAlive returns true if backend is alive
func (b *Backend) IsAlive() bool {
	b.mux.RLock()
	alive := b.Alive
	b.mux.RUnlock()
	return alive
}

// IncConns atomically increments active in-flight connections
func (b *Backend) IncConns() int64 {
	return atomic.AddInt64(&b.activeConnections, 1)
}

// DecConns atomically decrements active in-flight connections
func (b *Backend) DecConns() int64 {
	return atomic.AddInt64(&b.activeConnections, -1)
}

// GetActiveConnections returns the count of active in-flight connections
func (b *Backend) GetActiveConnections() int64 {
	return atomic.LoadInt64(&b.activeConnections)
}

// IncTotalRequests increments and returns the total requests handled
func (b *Backend) IncTotalRequests() uint64 {
	return atomic.AddUint64(&b.totalRequests, 1)
}

// GetTotalRequests returns the total requests handled
func (b *Backend) GetTotalRequests() uint64 {
	return atomic.LoadUint64(&b.totalRequests)
}

// GetWeight returns the capacity weight of the backend (minimum 1)
func (b *Backend) GetWeight() int {
	b.mux.RLock()
	defer b.mux.RUnlock()
	if b.weight <= 0 {
		return 1
	}
	return b.weight
}

// SetWeight updates the capacity weight of the backend thread-safely
func (b *Backend) SetWeight(weight int) {
	b.mux.Lock()
	b.weight = weight
	b.mux.Unlock()
}

// GetID returns the string identifier of the backend (URL string) for consistent hashing
func (b *Backend) GetID() string {
	if b.URL == nil {
		return ""
	}
	return b.URL.String()
}

// ServerPool maintains information of all backends and active selection strategy
type ServerPool struct {
	backends []*Backend
	strategy strategy.SelectionStrategy[*Backend]
	mux      sync.RWMutex
}

// NewServerPool creates a new ServerPool with the specified SelectionStrategy.
// If strat is nil, it defaults to Round-Robin.
func NewServerPool(strat strategy.SelectionStrategy[*Backend]) *ServerPool {
	if strat == nil {
		strat = strategy.NewRoundRobin[*Backend]()
	}
	return &ServerPool{
		strategy: strat,
	}
}

// SetStrategy updates the selection strategy dynamically
func (s *ServerPool) SetStrategy(strat strategy.SelectionStrategy[*Backend]) {
	if strat != nil {
		s.mux.Lock()
		s.strategy = strat
		s.mux.Unlock()
	}
}

// SetStrategyByName updates the active strategy using its string identifier
func (s *ServerPool) SetStrategyByName(name string) error {
	switch name {
	case "Round-Robin":
		s.SetStrategy(strategy.NewRoundRobin[*Backend]())
	case "Least-Connections":
		s.SetStrategy(strategy.NewLeastConnections[*Backend]())
	case "Smooth-Weighted-Round-Robin":
		s.SetStrategy(strategy.NewSmoothWeightedRoundRobin[*Backend]())
	case "Consistent-Hash":
		s.SetStrategy(strategy.NewConsistentHash[*Backend](100))
	default:
		return fmt.Errorf("unknown strategy: %s", name)
	}
	return nil
}

// GetStrategy returns the active selection strategy
func (s *ServerPool) GetStrategy() strategy.SelectionStrategy[*Backend] {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return s.strategy
}

// AddBackend adds a new backend server to the pool
func (s *ServerPool) AddBackend(b *Backend) {
	s.backends = append(s.backends, b)
}

// GetAllBackends returns a slice of all configured backends
func (s *ServerPool) GetAllBackends() []*Backend {
	return s.backends
}

// GetBackendByName finds a backend by its configured Name
func (s *ServerPool) GetBackendByName(name string) *Backend {
	for _, b := range s.backends {
		if b.Name == name {
			return b
		}
	}
	return nil
}

// GetNextPeer returns next active peer to execute a request via the active strategy
func (s *ServerPool) GetNextPeer() *Backend {
	s.mux.RLock()
	strat := s.strategy
	s.mux.RUnlock()

	if strat == nil {
		strat = strategy.NewRoundRobin[*Backend]()
	}
	return strat.SelectNextBackend(s.backends)
}

// GetNextPeerWithKey returns next active peer considering a routing key (e.g. Client IP)
func (s *ServerPool) GetNextPeerWithKey(key string) *Backend {
	s.mux.RLock()
	strat := s.strategy
	s.mux.RUnlock()

	if strat == nil {
		strat = strategy.NewRoundRobin[*Backend]()
	}
	if keyed, ok := strat.(strategy.KeyedSelectionStrategy[*Backend]); ok {
		return keyed.SelectNextBackendWithKey(s.backends, key)
	}
	return strat.SelectNextBackend(s.backends)
}

// HealthCheck pings backends concurrently and updates status
func (s *ServerPool) HealthCheck() {
	var wg sync.WaitGroup

	for _, b := range s.backends {
		wg.Add(1)

		go func(b *Backend) {
			defer wg.Done()

			alive := isBackendAlive(b.URL)
			wasAlive := b.IsAlive()
			b.SetAlive(alive)

			if wasAlive != alive {
				if alive {
					log.Printf("[HealthCheck] Backend %s status changed to -> UP", b.URL)
				} else {
					log.Printf("[HealthCheck] Backend %s status changed to -> DOWN", b.URL)
				}
			}
		}(b)
	}

	wg.Wait()
}

// isBackendAlive checks backend availability via TCP ping
func isBackendAlive(u *url.URL) bool {
	timeout := 2 * time.Second
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// NewBackend constructs a Backend with default weight 1 and a ReverseProxy
func NewBackend(u *url.URL) *Backend {
	return NewBackendNamedWithWeight(u, u.Host, 1)
}

// NewBackendWithWeight constructs a Backend with specified weight and a ReverseProxy
func NewBackendWithWeight(u *url.URL, weight int) *Backend {
	return NewBackendNamedWithWeight(u, u.Host, weight)
}

// NewBackendNamedWithWeight constructs a Backend with name, weight, and ReverseProxy
func NewBackendNamedWithWeight(u *url.URL, name string, weight int) *Backend {
	proxy := httputil.NewSingleHostReverseProxy(u)
	b := &Backend{
		URL:          u,
		Name:         name,
		Alive:        true,
		ReverseProxy: proxy,
		weight:       weight,
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[%s] Proxy Error: %v", u.Host, err)
		b.SetAlive(false)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Service Unavailable\n"))
	}

	return b
}
