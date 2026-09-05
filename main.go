package main

import (
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
		Port string
		Name string
	}{
		{Port: "8081", Name: "Backend-1"},
		{Port: "8082", Name: "Backend-2"},
		{Port: "8083", Name: "Backend-3"},
	}

	for _, cfg := range backendConfigs {
		StartDummyBackend(cfg.Port, cfg.Name)
	}

	// Brief pause to allow backend servers to bind to ports
	time.Sleep(100 * time.Millisecond)

	// 2. Initialize Server Pool with Round-Robin Strategy
	roundRobin := strategy.NewRoundRobin[*Backend]()
	serverPool := NewServerPool(roundRobin)
	log.Printf("Active Load Balancing Strategy: %s", serverPool.GetStrategy().Name())

	for _, cfg := range backendConfigs {
		u, err := url.Parse("http://localhost:" + cfg.Port)
		if err != nil {
			log.Fatalf("Invalid backend URL: %v", err)
		}
		b := NewBackend(u)
		serverPool.AddBackend(b)
		log.Printf("Configured Backend: %s", u.String())
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
		Handler: lbHandler(serverPool),
	}

	log.Printf("Load Balancer is running on http://localhost:%s", lbPort)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Load Balancer error: %v", err)
	}
}
