// Package hub is the in-process event bus and WebSocket endpoint.
//
// Server-side events (task.state, message.new, message.read, agent.output,
// agent.online, agent.offline, group.members) are broadcast as JSON frames
// to every connected client. Clients that fall behind more than one frame are
// dropped for the duration of the burst (v1 trade-off; the UI can
// re-sync via REST).
package hub

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// Event is one broadcast record.
type Event struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

// Hub fans out events to connected WebSocket clients.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

// New creates an empty hub.
func New() *Hub {
	return &Hub{clients: make(map[chan []byte]struct{})}
}

// Clients returns the number of connected clients.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Broadcast sends ev to all clients. A client whose buffer is full is
// skipped for this event (never blocks the publisher).
func (h *Hub) Broadcast(ev Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- data:
		default:
		}
	}
}

// WSHandler returns the HTTP handler for the /ws endpoint.
// Authentication must happen before this handler runs.
//
// It uses websocket.Server with a nil Handshake (no Origin check): the
// legacy websocket.Handler default rejects clients without an Origin
// header (403 "null origin"), which breaks standard non-browser
// RFC6455 clients (Node built-in WebSocket, scripts). Access control is
// the token auth in the api layer, so the Origin check adds no security.
//
// serveWS is called synchronously (not in a goroutine): the underlying
// websocket.Server closes the hijacked TCP connection as soon as the
// handler function returns, so the handler must block for the lifetime
// of the connection.
func (h *Hub) WSHandler() http.Handler {
	return websocket.Server{
		Handler: func(ws *websocket.Conn) {
			h.serveWS(ws)
		},
	}
}

func (h *Hub) serveWS(ws *websocket.Conn) {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
		close(ch)
	}()

	// Outbound: hub -> client. A write deadline ensures a stalled client
	// cannot wedge the writer goroutine (S7-Minor10).
	wsDone := make(chan struct{})
	go func() {
		defer close(wsDone)
		for data := range ch {
			_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := ws.Write(data); err != nil {
				return
			}
		}
	}()

	// Inbound frames are drained and ignored (v1 has no client commands).
	buf := make([]byte, 4096)
	for {
		if _, err := ws.Read(buf); err != nil {
			break
		}
	}
	<-wsDone
}
