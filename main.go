package main

import (
	"adaptive-load/metrics"
	"adaptive-load/strategy"
	"log"
	"net/http"
	"net/url"
	"time"
)

func main() {
	log.Println("=== Initializing Phase 1 HTTP Load Balancer ===")

	// 1. Start 3 Dummy Backend HTTP Servers on different ports
	backendConfigs := []struct {
		Port   string
		Name   string
		Weight int
	}{
		{Port: "8081", Name: "Backend-1", Weight: 3},
		{Port: "8082", Name: "Backend-2", Weight: 2},
		{Port: "8083", Name: "Backend-3", Weight: 1},
	}

	for _, cfg := range backendConfigs {
		StartDummyBackend(cfg.Port, cfg.Name)
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

	// 3. Start Periodic Background Health Check
	go func() {
		ticker := time.NewTicker(10 * time.Second)
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
