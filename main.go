package main

import (
	"adaptive-load/agent"
	"adaptive-load/controller"
	"adaptive-load/dummy"
	"adaptive-load/metrics"
	"adaptive-load/strategy"
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

func main() {
	log.Println("=== Initializing Phase 1 HTTP Load Balancer ===")

	// 1. Start 3 Dummy Backend HTTP Servers on different ports
	backendConfigs, err := backendsFromEnv()
	if err != nil {
		log.Fatalf("invalid LB_BACKENDS: %v", err)
	}

	// LB_NO_DUMMY=1: backends run as separate processes (go run ./cmd/backend), so CPU
	// pressure and failures do not share a process with the load balancer.
	if os.Getenv("LB_NO_DUMMY") != "1" {
		for _, cfg := range backendConfigs {
			dc := dummy.DefaultConfig(cfg.Name, cfg.Port)
			dc.Concurrency = 8 * cfg.Weight // capacity proportional to the declared weight
			StartDummyBackendWith(dc)
		}
	}

	// Brief pause to allow backend servers to bind to ports
	time.Sleep(100 * time.Millisecond)

	// 2. Initialize Metrics Collector with 10s sliding aggregation window
	metricsCollector := metrics.NewMetricsCollector(10 * time.Second)

	// 3. Initialize Server Pool with Round-Robin Strategy
	activeStrategy := strategy.NewRoundRobin[*Backend]()
	serverPool := NewServerPool(activeStrategy)
	serverPool.SetCollector(metricsCollector)
	log.Printf("Active Load Balancing Strategy: %s", serverPool.GetStrategy().Name())

	eventBus := NewEventBus()

	for _, cfg := range backendConfigs {
		u, err := url.Parse("http://localhost:" + cfg.Port)
		if err != nil {
			log.Fatalf("Invalid backend URL: %v", err)
		}
		b := NewBackendNamedWithWeight(u, cfg.Name, cfg.Weight)
		serverPool.AddBackend(b)
		metricsCollector.RegisterBackend(cfg.Name, u.String(), cfg.Weight, 4, 1024*1024*512, 100)
		log.Printf("Configured Backend: %s (%s, Weight: %d)", cfg.Name, u.String(), cfg.Weight)
	}

	// Phase 5: rule-based adaptive baseline. It runs in shadow mode until the
	// Adaptive-Weighted-Round-Robin action is selected (POST /api/policy {"action":4}).
	ruleCfg, ruleInterval := ruleControllerFromEnv()
	ruleController := controller.New(ruleCfg)
	serverPool.SetController(ruleController)
	serverPool.SetAffinityRequired(os.Getenv("LB_AFFINITY_REQUIRED") == "1")
	go ruleController.Run(context.Background(), ruleInterval,
		func() *metrics.StateRepresentation { return metricsCollector.GetStateRepresentation() },
		serverPool.PublishAdaptiveWeights)

	// Phase 10-12: the active policy starts at the known-safe baseline BEFORE traffic is served.
	agentCfg, mgrCfg := LoadAgentConfigFromEnv()
	if err := serverPool.ApplyActionReason(mgrCfg.SafeAction, "startup_safe_baseline"); err != nil {
		log.Fatalf("invalid safe baseline action %d: %v", mgrCfg.SafeAction, err)
	}
	log.Printf("Known-safe baseline active: %s", serverPool.GetStrategy().Name())
	if agentCfg.URL != "" {
		mgr := NewPolicyManager(serverPool, mgrCfg, time.Now())
		ctl := NewAgentController(serverPool, metricsCollector, mgr, agent.NewClient(agentCfg.URL, agentCfg.RequestTimeout), agentCfg)
		if path := os.Getenv("LB_DECISION_LOG"); path != "" {
			if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				ctl.SetDecisionLog(f)
			} else {
				log.Printf("cannot open LB_DECISION_LOG %q: %v", path, err)
			}
		}
		serverPool.SetAgent(ctl)
		go ctl.Run(context.Background())
		log.Printf("AI agent enabled: %s (timeout %v, min policy age %v, shadow=%v)",
			agentCfg.URL, agentCfg.RequestTimeout, mgrCfg.MinPolicyAge, mgrCfg.Shadow)
	} else {
		log.Printf("AI agent disabled (set LB_AGENT_URL to enable)")
	}

	// Phase 18: bounded time-series history for the research dashboard (sampled from the metrics window).
	go serverPool.History().Run(context.Background(), metricsCollector, serverPool, 2*time.Second)

	// 3. Start Periodic Background Health Check
	healthEvery := 10 * time.Second
	if v, err := strconv.Atoi(os.Getenv("LB_HEALTH_INTERVAL_MS")); err == nil && v > 0 {
		healthEvery = time.Duration(v) * time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(healthEvery)
		for range ticker.C {
			serverPool.HealthCheck()
		}
	}()

	// 4. Launch Load Balancer Server on Port 8080
	lbPort := "8080"
	server := &http.Server{
		Addr:    ":" + lbPort,
		Handler: lbHandler(serverPool, eventBus),
	}

	log.Printf("Load Balancer is running on http://localhost:%s", lbPort)
	log.Printf("Metrics API contract available at http://localhost:%s/api/metrics", lbPort)
	log.Printf("Interactive Dashboard available at http://localhost:5173 (Dev) or http://localhost:%s/ (Production)", lbPort)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Load Balancer error: %v", err)
	}
}
