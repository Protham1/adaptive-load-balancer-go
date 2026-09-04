package main

import (
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
)

// StartDummyBackend launches an HTTP server on the given port for demonstration purposes
func StartDummyBackend(port string, name string) {
	var requestCount uint64

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddUint64(&requestCount, 1)
		msg := fmt.Sprintf("Response from %s (Port %s) | Request Count: %d\n", name, port, count)
		log.Printf("[%s] Handled request #%d", name, count)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(msg))
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
	})

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	go func() {
		log.Printf("Starting %s on http://localhost:%s", name, port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Server %s stopped: %v", name, err)
		}
	}()
}
