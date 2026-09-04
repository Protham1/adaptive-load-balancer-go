package main

import (
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

// ServerPool maintains information of all backends
type ServerPool struct {
	backends []*Backend
	current  uint64
}

// AddBackend adds a new backend server to the pool
func (s *ServerPool) AddBackend(b *Backend) {
	s.backends = append(s.backends, b)
}

// NextIndex atomically increments current index and returns next backend index
func (s *ServerPool) NextIndex() int {
	if len(s.backends) == 0 {
		return 0
	}
	return int(atomic.AddUint64(&s.current, uint64(1)) % uint64(len(s.backends)))
}

// GetNextPeer returns next active peer to execute a request
func (s *ServerPool) GetNextPeer() *Backend {
	if len(s.backends) == 0 {
		return nil
	}

	next := s.NextIndex()
	l := len(s.backends) + next
	for i := next; i < l; i++ {
		idx := i % len(s.backends)
		if s.backends[idx].IsAlive() {
			if i != next {
				atomic.StoreUint64(&s.current, uint64(idx))
			}
			return s.backends[idx]
		}
	}
	return nil
}

// HealthCheck pings backends and updates status
func (s *ServerPool) HealthCheck() {
	for _, b := range s.backends {
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
	}
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
