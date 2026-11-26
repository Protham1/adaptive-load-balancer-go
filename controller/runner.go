package controller

import (
	"adaptive-load/metrics"
	"context"
	"log"
	"time"
)

// Run executes the controller once per decision window until ctx is cancelled.
// getState returns the latest State v1 snapshot; publish installs weights and
// reports whether they were applied (false when the active policy is not the
// adaptive one, in which case the controller runs in shadow mode: it keeps its
// smoothing state warm but changes nothing).
func (c *Controller) Run(ctx context.Context, interval time.Duration,
	getState func() *metrics.StateRepresentation,
	publish func(ids []string, weights []float64) bool) {

	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d := c.Update(getState())
			if d.Updated {
				if publish(d.BackendIDs, d.Weights) {
					log.Printf("[Controller] published adaptive weights %v (max delta %.3f)", d.Weights, d.MaxDelta)
				}
			}
		}
	}
}
