package main

import (
	"adaptive-load/agent"
	"adaptive-load/controller"
	"adaptive-load/dummy"
	"adaptive-load/metrics"
	"adaptive-load/strategy"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// lbOptions configures one load balancer instance. Everything has a safe default so the LB starts and
// serves with a baseline policy even when no AI agent is configured or the agent is down.
type lbOptions struct {
	Addr          string        // listen address, e.g. ":8080" ("127.0.0.1:0" in tests)
	Backends      []backendSpec // declared backends
	StartDummies  bool          // start in-process demo backends (LB_NO_DUMMY=1 disables)
	HealthEvery   time.Duration
	Security      *SecurityConfig
	DecisionLog   string // LB_DECISION_LOG (size-bounded, rotated)
	DecisionLogMB int64
	Ready         chan<- string // receives the actual listen address once serving (optional)
	ShutdownGrace time.Duration
}

func optionsFromEnv() (lbOptions, error) {
	backends, err := backendsFromEnv()
	if err != nil {
		return lbOptions{}, fmt.Errorf("invalid LB_BACKENDS: %w", err)
	}
	o := lbOptions{
		Addr: ":8080", Backends: backends, StartDummies: os.Getenv("LB_NO_DUMMY") != "1", HealthEvery: 10 * time.Second,
		Security: SecurityFromEnv(), DecisionLog: os.Getenv("LB_DECISION_LOG"), DecisionLogMB: 10, ShutdownGrace: 10 * time.Second,
	}
	if v := os.Getenv("LB_ADDR"); v != "" {
		o.Addr = v
	}
	if v, err := strconv.Atoi(os.Getenv("LB_HEALTH_INTERVAL_MS")); err == nil && v > 0 {
		o.HealthEvery = time.Duration(v) * time.Millisecond
	}
	if v, err := strconv.ParseInt(os.Getenv("LB_DECISION_LOG_MAX_MB"), 10, 64); err == nil && v > 0 {
		o.DecisionLogMB = v
	}
	return o, nil
}

func main() {
	log.Println("=== Adaptive HTTP Load Balancer ===")
	o, err := optionsFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	// SIGINT / SIGTERM (Ctrl+C, Ctrl+Break on Windows) start a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runLB(ctx, o); err != nil {
		log.Fatalf("Load Balancer error: %v", err)
	}
	log.Println("Load Balancer stopped cleanly")
}

// runLB builds the whole system, serves until ctx is cancelled, then shuts down gracefully:
// in-flight requests are allowed to finish (up to ShutdownGrace), event streams are closed, every
// background goroutine (rule controller, AI loop, history sampler, health checks) is stopped and waited
// for, the decision log is closed and in-process demo backends are stopped. It returns nil on a clean stop.
func runLB(parent context.Context, o lbOptions) error {
	ctx, cancelRun := context.WithCancel(parent)
	defer cancelRun()
	var wg sync.WaitGroup
	spawn := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
		}()
	}

	var demos []*dummy.Backend
	if o.StartDummies {
		for _, cfg := range o.Backends {
			dc := dummy.DefaultConfig(cfg.Name, cfg.Port)
			dc.Concurrency = 8 * cfg.Weight // capacity proportional to the declared weight
			if b := StartDummyBackendWith(dc); b != nil {
				demos = append(demos, b)
			}
		}
		time.Sleep(100 * time.Millisecond) // let the backend listeners bind
	}
	defer func() {
		for _, b := range demos {
			b.Stop()
		}
	}()

	metricsCollector := metrics.NewMetricsCollector(10 * time.Second)
	serverPool := NewServerPool(strategy.NewRoundRobin[*Backend]())
	serverPool.SetCollector(metricsCollector)
	serverPool.SetSecurity(o.Security)
	eventBus := NewEventBus()

	for _, cfg := range o.Backends {
		u, err := url.Parse("http://localhost:" + cfg.Port)
		if err != nil {
			return fmt.Errorf("invalid backend URL: %w", err)
		}
		b := NewBackendNamedWithWeight(u, cfg.Name, cfg.Weight)
		serverPool.AddBackend(b)
		metricsCollector.RegisterBackend(cfg.Name, u.String(), cfg.Weight, 4, 1024*1024*512, 100)
		log.Printf("Configured Backend: %s (%s, Weight: %d)", cfg.Name, u.String(), cfg.Weight)
	}

	// Phase 5: rule-based adaptive baseline (shadow mode until the adaptive action is selected).
	ruleCfg, ruleInterval := ruleControllerFromEnv()
	ruleController := controller.New(ruleCfg)
	serverPool.SetController(ruleController)
	serverPool.SetAffinityRequired(os.Getenv("LB_AFFINITY_REQUIRED") == "1")
	spawn(func() {
		ruleController.Run(ctx, ruleInterval,
			func() *metrics.StateRepresentation { return metricsCollector.GetStateRepresentation() },
			serverPool.PublishAdaptiveWeights)
	})

	// The active policy starts at the known-safe baseline BEFORE traffic is served. The AI agent is
	// optional: with no URL, or with an agent that is down, the LB serves with this baseline.
	agentCfg, mgrCfg := LoadAgentConfigFromEnv()
	if err := serverPool.ApplyActionReason(mgrCfg.SafeAction, "startup_safe_baseline"); err != nil {
		return fmt.Errorf("invalid safe baseline action %d: %w", mgrCfg.SafeAction, err)
	}
	log.Printf("Known-safe baseline active: %s", serverPool.GetStrategy().Name())

	var logFile *rotatingFile
	if agentCfg.URL != "" {
		client := agent.NewClient(agentCfg.URL, agentCfg.RequestTimeout)
		client.SetToken(os.Getenv("LB_AGENT_TOKEN"))
		mgr := NewPolicyManager(serverPool, mgrCfg, time.Now())
		ctl := NewAgentController(serverPool, metricsCollector, mgr, client, agentCfg)
		if o.DecisionLog != "" {
			if f, err := newRotatingFile(o.DecisionLog, o.DecisionLogMB<<20, 3); err == nil {
				logFile = f
				ctl.SetDecisionLog(f)
			} else {
				log.Printf("cannot open decision log %q: %v (continuing without it)", o.DecisionLog, err)
			}
		}
		serverPool.SetAgent(ctl)
		spawn(func() { ctl.Run(ctx) })
		log.Printf("AI agent enabled: %s (timeout %v, min policy age %v, shadow=%v)",
			redactURL(agentCfg.URL), agentCfg.RequestTimeout, mgrCfg.MinPolicyAge, mgrCfg.Shadow)
	} else {
		log.Printf("AI agent disabled (set LB_AGENT_URL to enable); serving with the baseline policy")
	}

	spawn(func() { serverPool.History().Run(ctx, metricsCollector, serverPool, 2*time.Second) })
	spawn(func() {
		t := time.NewTicker(o.HealthEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				serverPool.HealthCheck()
			}
		}
	})

	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return err
	}
	srv := newHTTPServer(o.Addr, lbHandler(serverPool, eventBus))
	srv.RegisterOnShutdown(eventBus.Close) // end SSE streams so they cannot hold the shutdown open
	if o.Ready != nil {
		o.Ready <- ln.Addr().String()
	}
	mode := "loopback-only"
	if o.Security != nil && o.Security.AdminToken != "" {
		mode = "bearer-token"
	}
	log.Printf("Load Balancer is running on http://%s (management API: %s; client IPs in events masked: %v)",
		ln.Addr(), mode, o.Security != nil && o.Security.MaskClientIP)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var runErr error
	select {
	case err := <-serveErr:
		runErr = err // the server died on its own
	case <-ctx.Done():
		log.Println("Shutdown requested: draining in-flight requests ...")
	}

	cancelRun() // tell every background goroutine to stop (also when the server died on its own)
	grace := o.ShutdownGrace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Printf("graceful shutdown timed out (%v): closing remaining connections", err)
		_ = srv.Close()
	}
	// Background goroutines stop on ctx; if the server died on its own they have not been told yet.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		log.Println("warning: some background goroutines did not stop within the grace period")
	}
	if logFile != nil {
		_ = logFile.Close()
	}
	if runErr != nil && !errors.Is(runErr, http.ErrServerClosed) {
		return runErr
	}
	return nil
}

// redactURL hides credentials and query strings of a configured URL before it is logged.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "(unparseable)"
	}
	u.User, u.RawQuery = nil, ""
	return strings.TrimRight(u.String(), "/")
}
