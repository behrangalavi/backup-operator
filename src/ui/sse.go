package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// --- Server-Sent Events ---

type sseEvent struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type sseBroker struct {
	mu         sync.Mutex
	clients    map[chan sseEvent]struct{}
	maxClients int // 0 = unlimited
}

func newSSEBroker() *sseBroker {
	return &sseBroker{clients: make(map[chan sseEvent]struct{})}
}

// subscribe registers a new SSE client. Returns nil when the broker is at
// maxClients capacity, in which case the caller MUST refuse the connection
// rather than block — otherwise an unauthenticated client (UI auth is
// pluggable) can pin operator memory by hoarding subscriptions.
func (b *sseBroker) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 16)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxClients > 0 && len(b.clients) >= b.maxClients {
		close(ch)
		return nil
	}
	b.clients[ch] = struct{}{}
	return ch
}

func (b *sseBroker) unsubscribe(ch chan sseEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.clients[ch]; !ok {
		return
	}
	delete(b.clients, ch)
	close(ch)
}

func (b *sseBroker) publish(ev sseEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *Server) broadcast(ev sseEvent) {
	if s.sse != nil {
		s.sse.publish(ev)
	}
}

// Broadcast is the public entry point for controllers and other
// non-UI packages to push an SSE event. Wraps the internal sseEvent
// type so callers don't need to import it. Safe to call from any
// goroutine and before the SSE broker has any subscribers — the
// broker's publish handles both cases.
func (s *Server) Broadcast(eventType, data string) {
	s.broadcast(sseEvent{Type: eventType, Data: data})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, codeInternal, "SSE not supported")
		return
	}

	ch := s.sse.subscribe()
	if ch == nil {
		// Broker is full. Refuse explicitly so the client retries later
		// rather than holding a half-open SSE stream that pins memory.
		writeError(w, http.StatusServiceUnavailable, codeInternal, "too many SSE clients; retry shortly")
		return
	}
	defer s.sse.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Send initial ping.
	_, _ = fmt.Fprintf(w, "event: connected\ndata: ok\n\n")
	flusher.Flush()

	ctx := r.Context()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			data, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
