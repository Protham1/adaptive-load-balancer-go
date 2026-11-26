package main

import (
	"encoding/json"
	"sync"
)

// RequestEvent represents a real-time event for UI visualization
type RequestEvent struct {
	ID          string `json:"id"`
	Type        string `json:"type"` // "start" or "complete"
	ClientIP    string `json:"client_ip"`
	BackendURL  string `json:"backend_url"`
	BackendName string `json:"backend_name"`
	Strategy    string `json:"strategy"`
	ActiveConns int64  `json:"active_conns"`
	DurationMs  int64  `json:"duration_ms"`
	Status      int    `json:"status"`
	Timestamp   int64  `json:"timestamp"`
}

// EventBus manages SSE subscribers and event broadcasting
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[chan []byte]struct{}
	closed      bool
}

// NewEventBus creates a new EventBus instance
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[chan []byte]struct{}),
	}
}

// Subscribe registers a new subscriber channel
func (b *EventBus) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// TrySubscribe registers a subscriber unless max (> 0) subscribers already exist.
func (b *EventBus) TrySubscribe(max int) (chan []byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || (max > 0 && len(b.subscribers) >= max) {
		return nil, false
	}
	ch := make(chan []byte, 64)
	b.subscribers[ch] = struct{}{}
	return ch, true
}

// Close ends every subscription (streams see a closed channel and return) and refuses new ones. Used on
// shutdown so long-lived SSE connections cannot block a graceful HTTP shutdown.
func (b *EventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for ch := range b.subscribers {
		close(ch)
		delete(b.subscribers, ch)
	}
}

// Count returns the number of active subscribers.
func (b *EventBus) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Unsubscribe removes and closes a subscriber channel
func (b *EventBus) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	if _, exists := b.subscribers[ch]; exists {
		delete(b.subscribers, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// Publish broadcasts a RequestEvent to all active subscribers
func (b *EventBus) Publish(evt RequestEvent) {
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	for ch := range b.subscribers {
		select {
		case ch <- data:
		default:
			// Non-blocking: skip if subscriber channel is full
		}
	}
}
