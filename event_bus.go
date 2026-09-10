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
