package hub

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// TestBroadcastDelivers: a WS client connected to the hub's handler
// receives a broadcast event as a JSON frame (A7's core mechanic).
func TestBroadcastDelivers(t *testing.T) {
	h := New()
	ts := httptest.NewServer(h.WSHandler())
	defer ts.Close()

	url := "ws" + ts.URL[len("http"):] + "/ws"
	c, err := websocket.Dial(url, "", ts.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Wait for the hub to register the client.
	deadline := time.Now().Add(2 * time.Second)
	for h.Clients() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.Clients() != 1 {
		t.Fatalf("client count = %d, want 1", h.Clients())
	}

	h.Broadcast(Event{Type: "task.state", Payload: map[string]any{"task_id": 7, "status": "running"}})

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var ev Event
	if err := json.Unmarshal(buf[:n], &ev); err != nil {
		t.Fatalf("unmarshal frame %q: %v", buf[:n], err)
	}
	if ev.Type != "task.state" {
		t.Errorf("event type = %q, want task.state", ev.Type)
	}
	if got := ev.Payload["task_id"]; got != float64(7) {
		t.Errorf("payload task_id = %v, want 7", got)
	}
	if !bytes.Contains(buf[:n], []byte(`"status":"running"`)) {
		t.Errorf("frame missing status:running: %s", buf[:n])
	}
}

// TestDroppedSlowClient: a client that never reads does not block
// Broadcast (buffer full → event dropped, no goroutine leak beyond
// the client itself).
func TestDroppedSlowClient(t *testing.T) {
	h := New()
	ts := httptest.NewServer(h.WSHandler())
	defer ts.Close()

	url := "ws" + ts.URL[len("http"):] + "/ws"
	c, err := websocket.Dial(url, "", ts.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for h.Clients() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Broadcast 100 events; the 64-deep client buffer will overflow.
		for i := 0; i < 100; i++ {
			h.Broadcast(Event{Type: "task.state", Payload: map[string]any{"i": i}})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Broadcast blocked on slow client (buffer full case)")
	}
}

// TestWSHandlerServes: a valid dial against /ws completes the handshake
// (the hub handler itself requires no auth — auth is the api layer's job).
func TestWSHandlerServes(t *testing.T) {
	h := New()
	ts := httptest.NewServer(h.WSHandler())
	defer ts.Close()

	url := "ws" + ts.URL[len("http"):] + "/ws"
	c, err := websocket.Dial(url, "", ts.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()
}

// TestWSHandshakeWithoutOrigin: non-browser RFC6455 clients (e.g. Node's
// built-in WebSocket) do not send an Origin header. The hub must still
// complete the 101 handshake; the legacy websocket.Handler default
// checkOrigin rejected them with 403 (loadtest regression, 2026-11-05).
// Access control is the token auth at the api layer, not Origin.
func TestWSHandshakeWithoutOrigin(t *testing.T) {
	h := New()
	ts := httptest.NewServer(h.WSHandler())
	defer ts.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	req := "GET /ws HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		fmt.Sprintf("Sec-WebSocket-Key: %s\r\n", key) +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 101") {
		t.Fatalf("handshake without Origin = %q, want 101 Switching Protocols", buf[:n])
	}
}
