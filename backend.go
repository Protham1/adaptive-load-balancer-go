package main

import (
	"adaptive-load/controller"
	"adaptive-load/metrics"
	"adaptive-load/policy"
	"adaptive-load/strategy"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
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

// GetName returns the stable backend name used as the backend ID in metrics,
// the State v1 vector, and policies.
func (b *Backend) GetName() string {
	return b.Name
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
	backends  []*Backend
	active    atomic.Pointer[activeState] // lock-free read on the request path
	collector *metrics.MetricsCollector
	mux       sync.RWMutex

	// Phase 6: immutable policy snapshots read atomically by strategies.
	policies         *policy.Store
	affinityRequired bool
	ctrl             *controller.Controller // Phase 5 rule-based baseline (optional)
	agentCtl         *AgentController       // Phase 10 AI decision loop (optional)
	events           *EventLog              // Phase 18: bounded research event log
	history          *History               // Phase 18: bounded time-series history
}

// Events returns the event log (never nil).
func (s *ServerPool) Events() *EventLog { return s.events }

// History returns the time-series history (never nil).
func (s *ServerPool) History() *History { return s.history }

// SetAgent attaches the AI decision loop (for the status / kill-switch API).
func (s *ServerPool) SetAgent(a *AgentController) {
	s.mux.Lock()
	s.agentCtl = a
	s.mux.Unlock()
}

// GetAgent returns the AI decision loop, if configured.
func (s *ServerPool) GetAgent() *AgentController {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return s.agentCtl
}

// activeState is the immutable pair the request path loads atomically: the
// strategy instance in force and the version of the policy that selected it.
// A policy swap builds a NEW strategy instance (no mutable state is shared
// between old and new versions) and publishes a new activeState; requests that
// already selected a backend are never cancelled or migrated.
type activeState struct {
	strat         strategy.SelectionStrategy[*Backend]
	policyVersion uint64
}

// NewServerPool creates a new ServerPool with the specified SelectionStrategy.
// If strat is nil, it defaults to Round-Robin.
func NewServerPool(strat strategy.SelectionStrategy[*Backend]) *ServerPool {
	if strat == nil {
		strat = strategy.NewRoundRobin[*Backend]()
	}
	p := &ServerPool{
		policies: policy.NewStore(policy.StrategyType(strat.Name())),
		events:   NewEventLog(eventLogCap),
		history:  NewHistory(historyCap),
	}
	p.active.Store(&activeState{strat: strat, policyVersion: p.policies.Current().Version})
	return p
}

// SetCollector attaches a metrics collector to the server pool.
func (s *ServerPool) SetCollector(col *metrics.MetricsCollector) {
	s.mux.Lock()
	s.collector = col
	s.mux.Unlock()
}

// GetCollector returns the attached metrics collector if any.
func (s *ServerPool) GetCollector() *metrics.MetricsCollector {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return s.collector
}

// SetStrategy updates the selection strategy dynamically and publishes the
// matching immutable policy snapshot.
func (s *ServerPool) SetStrategy(strat strategy.SelectionStrategy[*Backend]) {
	s.SetStrategyWithReason(strat, "manual")
}

// SetStrategyWithReason is SetStrategy recording a reason code on the policy.
// Order matters for atomic publication: the policy snapshot is published first,
// then the active (strategy, version) pair is swapped in one atomic store.
func (s *ServerPool) SetStrategyWithReason(strat strategy.SelectionStrategy[*Backend], reason string) {
	if strat == nil {
		return
	}
	start := time.Now()
	s.mux.Lock()
	col := s.collector
	ver := s.publishPolicyLocked(policy.StrategyType(strat.Name()), reason)
	s.active.Store(&activeState{strat: strat, policyVersion: s.policies.Current().Version})
	s.mux.Unlock()
	s.events.Add("policy", "", fmt.Sprintf("%s (policy v%d, %s)", strat.Name(), s.policies.Current().Version, reason))

	if col != nil {
		col.RecordStrategySwitch(strat.Name(), time.Since(start))
		col.SetPolicyVersion(ver)
	}
}

// sortedBackendsLocked returns the backends in stable sorted-name order, the same
// order as the State v1 vector. Caller must hold s.mux.
func (s *ServerPool) sortedBackendsLocked() []*Backend {
	bs := append([]*Backend(nil), s.backends...)
	sort.Slice(bs, func(i, j int) bool { return bs[i].Name < bs[j].Name })
	return bs
}

// publishPolicyLocked publishes the policy for strategy st, with weights where
// the strategy has them. Caller must hold s.mux. Returns the version string.
func (s *ServerPool) publishPolicyLocked(st policy.StrategyType, reason string) string {
	var ids []string
	var weights []float64
	switch st {
	case policy.StaticSWRR:
		bs := s.sortedBackendsLocked()
		infos := make([]policy.BackendInfo, len(bs))
		for i, b := range bs {
			infos[i] = policy.BackendInfo{ID: b.Name, StaticWeight: b.GetWeight()}
		}
		if w, err := policy.NormalizeStaticWeights(infos); err == nil {
			weights = w
			for _, in := range infos {
				ids = append(ids, in.ID)
			}
		}
	case policy.AdaptiveSWRR:
		if s.ctrl != nil {
			ids, weights = s.ctrl.CurrentWeights()
		}
	}
	p, err := s.policies.PublishReason(st, ids, weights, reason)
	if err != nil {
		// Weights were unusable: publish the strategy without weights rather than failing the switch.
		if p, err = s.policies.PublishReason(st, nil, nil, reason); err != nil {
			// Strategy outside the action space (e.g. a custom strategy): keep the prior policy.
			p = s.policies.Current()
		}
	}
	return policyVersionString(p)
}

func policyVersionString(p *policy.Policy) string {
	return fmt.Sprintf("v%d", p.Version)
}

// Policies returns the policy store (lock-free reads via Current()).
func (s *ServerPool) Policies() *policy.Store { return s.policies }

// SetAffinityRequired declares whether session affinity is a system requirement
// (the precondition for the Consistent-Hash action).
func (s *ServerPool) SetAffinityRequired(v bool) {
	s.mux.Lock()
	s.affinityRequired = v
	s.mux.Unlock()
}

// SetController attaches the rule-based controller, making the adaptive action available.
func (s *ServerPool) SetController(c *controller.Controller) {
	s.mux.Lock()
	s.ctrl = c
	s.mux.Unlock()
}

// GetController returns the attached rule-based controller, if any.
func (s *ServerPool) GetController() *controller.Controller {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return s.ctrl
}

// Constraints describes the current configuration that actions must be valid under.
func (s *ServerPool) Constraints() policy.Constraints {
	s.mux.RLock()
	defer s.mux.RUnlock()
	bs := s.sortedBackendsLocked()
	infos := make([]policy.BackendInfo, len(bs))
	for i, b := range bs {
		infos[i] = policy.BackendInfo{ID: b.Name, StaticWeight: b.GetWeight()}
	}
	return policy.Constraints{Backends: infos, AffinityRequired: s.affinityRequired, AdaptiveAvailable: s.ctrl != nil}
}

// ValidateAction checks an action against the current backend configuration.
func (s *ServerPool) ValidateAction(a policy.ActionID) error {
	return policy.Validate(a, s.Constraints())
}

// ApplyAction validates the action and, if valid, activates its strategy.
func (s *ServerPool) ApplyAction(a policy.ActionID) error {
	return s.ApplyActionReason(a, "action")
}

// ApplyActionReason is ApplyAction recording a reason code on the new policy.
func (s *ServerPool) ApplyActionReason(a policy.ActionID, reason string) error {
	if err := s.ValidateAction(a); err != nil {
		return err
	}
	st, _ := a.Strategy()
	return s.setStrategyByNameReason(string(st), reason)
}

// PublishAdaptiveWeights installs new dynamic weights, but only while the
// adaptive strategy is active. It reports whether the weights were applied.
func (s *ServerPool) PublishAdaptiveWeights(ids []string, weights []float64) bool {
	_, ok := s.PublishAdaptiveWeightsReason(ids, weights, "rule_based")
	return ok
}

// PublishAdaptiveWeightsReason is PublishAdaptiveWeights recording a reason code
// and returning the published snapshot.
func (s *ServerPool) PublishAdaptiveWeightsReason(ids []string, weights []float64, reason string) (*policy.Policy, bool) {
	s.mux.RLock()
	col := s.collector
	s.mux.RUnlock()
	p, ok, err := s.policies.PublishWeightsIfReason(policy.AdaptiveSWRR, ids, weights, reason)
	if err != nil || !ok {
		return nil, false
	}
	if col != nil {
		col.SetPolicyVersion(policyVersionString(p))
	}
	return p, true
}

// healthyCount returns the number of backends currently marked alive.
func (s *ServerPool) healthyCount() int {
	s.mux.RLock()
	defer s.mux.RUnlock()
	n := 0
	for _, b := range s.backends {
		if b.IsAlive() {
			n++
		}
	}
	return n
}

// backendNames returns the configured backend names (unsorted copy).
func (s *ServerPool) backendNames() []string {
	s.mux.RLock()
	defer s.mux.RUnlock()
	out := make([]string, len(s.backends))
	for i, b := range s.backends {
		out[i] = b.Name
	}
	return out
}

// SetStrategyByName updates the active strategy using its string identifier
func (s *ServerPool) SetStrategyByName(name string) error {
	return s.setStrategyByNameReason(name, "manual")
}

func (s *ServerPool) setStrategyByNameReason(name, reason string) error {
	switch policy.StrategyType(name) {
	case policy.RoundRobin:
		s.SetStrategyWithReason(strategy.NewRoundRobin[*Backend](), reason)
	case policy.LeastConnections:
		s.SetStrategyWithReason(strategy.NewLeastConnections[*Backend](), reason)
	case policy.StaticSWRR:
		s.SetStrategyWithReason(strategy.NewSmoothWeightedRoundRobin[*Backend](), reason)
	case policy.ConsistentHash:
		s.SetStrategyWithReason(strategy.NewConsistentHash[*Backend](100), reason)
	case policy.AdaptiveSWRR:
		s.SetStrategyWithReason(strategy.NewAdaptiveWeightedRoundRobin[*Backend](func() strategy.WeightLookup {
			return s.policies.Current()
		}), reason)
	default:
		return fmt.Errorf("unknown strategy: %s", name)
	}
	return nil
}

// GetStrategy returns the active selection strategy
func (s *ServerPool) GetStrategy() strategy.SelectionStrategy[*Backend] {
	return s.active.Load().strat
}

// AddBackend adds a new backend server to the pool thread-safely
func (s *ServerPool) AddBackend(b *Backend) {
	s.mux.Lock()
	s.backends = append(s.backends, b)
	col := s.collector
	s.mux.Unlock()

	if col != nil {
		col.RegisterBackend(b.Name, b.URL.String(), b.GetWeight(), 2, 1024*1024*512, 100)
	}
}

// RemoveBackend removes a backend server from the pool by its Name or URL string thread-safely
func (s *ServerPool) RemoveBackend(identifier string) bool {
	s.mux.Lock()
	var removedName string
	var found bool
	for i, b := range s.backends {
		if b.Name == identifier || (b.URL != nil && b.URL.String() == identifier) {
			removedName = b.Name
			s.backends = append(s.backends[:i], s.backends[i+1:]...)
			found = true
			break
		}
	}
	col := s.collector
	s.mux.Unlock()

	if found && col != nil {
		col.UnregisterBackend(removedName)
	}
	return found
}

// GetAllBackends returns a snapshot copy slice of all configured backends
func (s *ServerPool) GetAllBackends() []*Backend {
	s.mux.RLock()
	defer s.mux.RUnlock()
	cp := make([]*Backend, len(s.backends))
	copy(cp, s.backends)
	return cp
}

// GetBackendByName finds a backend by its configured Name thread-safely
func (s *ServerPool) GetBackendByName(name string) *Backend {
	s.mux.RLock()
	defer s.mux.RUnlock()
	for _, b := range s.backends {
		if b.Name == name {
			return b
		}
	}
	return nil
}

// GetNextPeer returns next active peer to execute a request via the active strategy
func (s *ServerPool) GetNextPeer() *Backend {
	strat := s.active.Load().strat
	s.mux.RLock()
	backends := make([]*Backend, len(s.backends))
	copy(backends, s.backends)
	s.mux.RUnlock()

	if strat == nil {
		strat = strategy.NewRoundRobin[*Backend]()
	}
	return strat.SelectNextBackend(backends)
}

// GetNextPeerWithKey returns next active peer considering a routing key (e.g. Client IP)
func (s *ServerPool) GetNextPeerWithKey(key string) *Backend {
	strat := s.active.Load().strat
	s.mux.RLock()
	backends := make([]*Backend, len(s.backends))
	copy(backends, s.backends)
	s.mux.RUnlock()

	if strat == nil {
		strat = strategy.NewRoundRobin[*Backend]()
	}
	if keyed, ok := strat.(strategy.KeyedSelectionStrategy[*Backend]); ok {
		return keyed.SelectNextBackendWithKey(backends, key)
	}
	return strat.SelectNextBackend(backends)
}

// HealthCheck pings backends concurrently and updates status
func (s *ServerPool) HealthCheck() {
	s.mux.RLock()
	backends := make([]*Backend, len(s.backends))
	copy(backends, s.backends)
	col := s.collector
	s.mux.RUnlock()

	var wg sync.WaitGroup

	for _, b := range backends {
		wg.Add(1)

		go func(b *Backend) {
			defer wg.Done()

			start := time.Now()
			alive, err := pingBackend(b.URL)
			dur := time.Since(start)

			wasAlive := b.IsAlive()
			b.SetAlive(alive)

			if col != nil {
				col.RecordProbe(b.Name, alive, dur, err)
			}

			if wasAlive != alive {
				if alive {
					s.events.Add("health", b.Name, "UP")
					log.Printf("[HealthCheck] Backend %s status changed to -> UP", b.URL)
				} else {
					s.events.Add("health", b.Name, "DOWN")
					log.Printf("[HealthCheck] Backend %s status changed to -> DOWN", b.URL)
				}
			}
		}(b)
	}

	wg.Wait()
}

// pingBackend checks backend availability via TCP ping and returns probe error
func pingBackend(u *url.URL) (bool, error) {
	timeout := 2 * time.Second
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	return true, nil
}

// isBackendAlive checks backend availability via TCP ping
func isBackendAlive(u *url.URL) bool {
	alive, _ := pingBackend(u)
	return alive
}

// sharedProxyTransport is the connection pool used by every backend's reverse proxy. Go's default
// transport keeps only 2 idle connections per host, so under concurrency the proxy dialled (and
// closed) a new TCP connection to a backend for almost every request; profiling showed that dominated
// the load balancer's CPU (about 1 ms per request in-process, 3-4 ms on Windows loopback), which
// saturated the test machine at ~600 rps. Keeping idle connections per backend removes the churn.
var sharedProxyTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:          4096,
	MaxIdleConnsPerHost:   1024,
	IdleConnTimeout:       90 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	DisableCompression:    true, // never re-compress / decompress payloads in the proxy
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
	proxy.Transport = sharedProxyTransport
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
