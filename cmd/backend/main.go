// Command backend runs one standalone fault-injectable backend as its own
// process, so CPU pressure and failures do not share a process with the load
// balancer.
//
//	go run ./cmd/backend -port 8081 -name Backend-1 -conc 24
package main

import (
	"adaptive-load/dummy"
	"flag"
	"log"
	"os"
	"os/signal"
	"time"
)

func main() {
	port := flag.String("port", "8081", "listen port")
	name := flag.String("name", "Backend-1", "backend name")
	conc := flag.Int("conc", 8, "concurrent requests served (capacity = conc / service-ms)")
	svc := flag.Duration("service", 20*time.Millisecond, "base service time per request")
	cpu := flag.Duration("cpu-work", 300*time.Microsecond, "real CPU work per request")
	queue := flag.Duration("queue-timeout", 500*time.Millisecond, "queue wait before returning 503")
	seed := flag.Int64("seed", 1, "seed for injected-error pattern")
	flag.Parse()

	cfg := dummy.DefaultConfig(*name, *port)
	cfg.Concurrency, cfg.ServiceTime, cfg.QueueTimeout, cfg.Seed, cfg.CPUWork = *conc, *svc, *queue, *seed, *cpu
	b, err := dummy.Start(cfg)
	if err != nil {
		log.Fatalf("backend %s: %v", *name, err)
	}
	log.Printf("%s listening on :%s (concurrency %d, service %v); fault control: POST /__fault", *name, *port, *conc, *svc)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
	b.Stop()
}
