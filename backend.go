package main

import (
	"adaptive-load/strategy"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// Backend holds the data about a server backend
type Backend struct {
	URL          *url.URL
	Alive        bool
	mux          sync.RWMutex
	ReverseProxy *httputil.ReverseProxy
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

// ServerPool maintains information of all backends and active selection strategy
type ServerPool struct {
	backends []*Backend
	strategy strategy.SelectionStrategy[*Backend]
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
		s.strategy = strat
	}
}

// GetStrategy returns the active selection strategy
func (s *ServerPool) GetStrategy() strategy.SelectionStrategy[*Backend] {
	return s.strategy
}

// AddBackend adds a new backend server to the pool
func (s *ServerPool) AddBackend(b *Backend) {
	s.backends = append(s.backends, b)
}

// GetNextPeer returns next active peer to execute a request via the active strategy
func (s *ServerPool) GetNextPeer() *Backend {
	if s.strategy == nil {
		s.strategy = strategy.NewRoundRobin[*Backend]()
	}
	return s.strategy.SelectNextBackend(s.backends)
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

// NewBackend constructs a Backend with a ReverseProxy
func NewBackend(u *url.URL) *Backend {
	proxy := httputil.NewSingleHostReverseProxy(u)
	b := &Backend{
		URL:          u,
		Alive:        true,
		ReverseProxy: proxy,
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[%s] Proxy Error: %v", u.Host, err)
		b.SetAlive(false)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Service Unavailable\n"))
	}

	return b
}
