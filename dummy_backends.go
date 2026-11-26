package main

import (
	"adaptive-load/dummy"
	"log"
)

// StartDummyBackend launches an in-process fault-injectable demo backend.
// Fault control is served on port+1000 (see package dummy).
func StartDummyBackend(port string, name string) {
	StartDummyBackendWith(dummy.DefaultConfig(name, port))
}

// StartDummyBackendWith launches an in-process backend with an explicit configuration.
func StartDummyBackendWith(cfg dummy.Config) {
	if _, err := dummy.Start(cfg); err != nil {
		log.Printf("Backend %s failed to start: %v", cfg.Name, err)
		return
	}
	log.Printf("Started %s on http://localhost:%s (fault control :%s)", cfg.Name, cfg.Port, cfg.ControlPort)
}
