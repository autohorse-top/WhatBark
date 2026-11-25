package api

// P0-1: subprotocol-token WS handshake + short-lived HMAC signed file/avatar
// URLs (legacy ?token= kept as fallback).

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// signAt mints a signed URL for path with an explicit exp (test hook:
// same-package access to signKey; mirrors Server.signFileURL with fixed exp).
func signAt(s *Server, path string, exp int64) string {
	mac := hmac.New(sha256.New, s.signKey)
	fmt.Fprintf(mac, "%s|%d", path, exp)
	return fmt.Sprintf("%s?exp=%d&sig=%s", path, exp, hex.EncodeToString(mac.Sum(nil)))
}

// flipHex changes a hex char to a different one (for sig tampering).
func flipHex(c byte) string {
	if c == 'a' {
		return "b"
	}
	return "a"
}

// TestSignedFileURL: upload -> signed URL; bare GET works; tampered/expired/
// foreign-path sigs and missing credentials are rejected; legacy ?token= and
// Authorization header still work.
func TestSignedFileURL(t *testing.T) {
	e := newEnv(t)

	code, body, _ := upload(t, e.srv.URL+"/api/files", adminTok, "file", "shot.png", pngBytes)
	if code != http.StatusCreated {
		t.Fatalf("upload: got %d want 201: %v", code, body)
	}
	signed := body["url"].(string)
	if !strings.Contains(signed, "sig=") || !strings.Contains(signed, "exp=") {
		t.Fatalf("upload url not signed: %q", signed)
	}
	plain := strings.SplitN(signed, "?", 2)[0]

	// Derive the exp value from the minted URL to build negative variants.
	expStr := signed[strings.Index(signed, "exp=")+4 : strings.Index(signed, "&")]
	// Expired URL minted with the server's own key (same format, past time).
	expired := signAt(e.api, plain, time.Now().Unix()-60)
	// Signature computed for a different path, applied to the real path (cross-path replay).
	expF := time.Now().Unix() + 3600
	macF := hmac.New(sha256.New, e.api.signKey)
	fmt.Fprintf(macF, "%s|%d", "/api/files/other.png", expF)
	foreign := fmt.Sprintf("%s?exp=%d&sig=%s", plain, expF, hex.EncodeToString(macF.Sum(nil)))
	// Tampered: flip the first sig hex char.
	sigIdx := strings.Index(signed, "sig=") + 4
	tampered := signed[:sigIdx+1] + flipHex(signed[sigIdx+1]) + signed[sigIdx+2:]
	// Missing sig: keep exp, drop the signature param.
	noSig := plain + "?exp=" + expStr

	cases := []struct {
		name string
		url  string
		hdr  string
		want int
	}{
		{"valid signed url (bare)", e.srv.URL + signed, "", http.StatusOK},
		{"tampered sig", e.srv.URL + tampered, "", http.StatusUnauthorized},
		{"missing sig", e.srv.URL + noSig, "", http.StatusUnauthorized},
		{"expired exp", e.srv.URL + expired, "", http.StatusUnauthorized},
		{"foreign-path sig", e.srv.URL + foreign, "", http.StatusUnauthorized},
		{"legacy ?token=", e.srv.URL + plain + "?token=" + adminTok, "", http.StatusOK},
		{"authorization header", e.srv.URL + plain, "Bearer " + adminTok, http.StatusOK},
		{"no credentials", e.srv.URL + plain, "", http.StatusUnauthorized},
		// F5: exp beyond the 24h acceptance window is rejected even though
		// the HMAC is valid (fileURLMaxLifetime branch).
		{"far-future exp (25h)", e.srv.URL + signAt(e.api, plain, time.Now().Unix()+25*3600), "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, c.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.hdr != "" {
				r.Header.Set("Authorization", c.hdr)
			}
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("%s: got %d want %d", c.name, resp.StatusCode, c.want)
			}
			if c.want == http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				if !bytes.Equal(b, pngBytes) {
					t.Errorf("%s: body mismatch (%d bytes)", c.name, len(b))
				}
			}
		})
	}
}

// TestSignedAvatarURL: avatar upload response carries a signed URL; bare GET
// returns the uploaded bytes; tampered sig -> 401.
func TestSignedAvatarURL(t *testing.T) {
	e := newEnv(t)

	code, body, _ := upload(t, e.srv.URL+"/api/me/avatar", adminTok, "avatar", "boss.png", pngBytes)
	if code != http.StatusOK {
		t.Fatalf("avatar upload: got %d want 200: %v", code, body)
	}
	signed := body["avatar"].(string)
	if !strings.Contains(signed, "sig=") || !strings.Contains(signed, "exp=") {
		t.Fatalf("avatar url not signed: %q", signed)
	}
	sigIdx := strings.Index(signed, "sig=") + 4
	tampered := signed[:sigIdx+1] + flipHex(signed[sigIdx+1]) + signed[sigIdx+2:]

	getStatus := func(u string) (int, []byte) {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	c, got := getStatus(e.srv.URL + signed)
	if c != http.StatusOK {
		t.Fatalf("bare signed avatar GET: got %d want 200", c)
	}
	if !bytes.Equal(got, pngBytes) {
		t.Errorf("avatar body mismatch (%d bytes)", len(got))
	}
	if c, _ := getStatus(e.srv.URL + tampered); c != http.StatusUnauthorized {
		t.Errorf("tampered avatar sig: got %d want 401", c)
	}
}

// TestWSSubprotocolAuth: token carried as the single Sec-WebSocket-Protocol
// subprotocol; 101 echoes it; bad/absent credentials rejected (legacy
// ?token= still works as fallback).
func TestWSSubprotocolAuth(t *testing.T) {
	e := newEnv(t)
	wsBase := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws"
	origin := "http://127.0.0.1"

	cases := []struct {
		name    string
		u       string
		proto   string
		wantErr bool
	}{
		{"subprotocol token", wsBase, adminTok, false},
		{"bad subprotocol", wsBase, "bad-token", true},
		// F5: a comma-separated multi-value subprotocol list is not a valid
		// token even if its first element is — must be rejected.
		{"multi-value subprotocol", wsBase, adminTok + ", extra", true},
		{"legacy query token", wsBase + "?token=" + adminTok, "", false},
		{"no credentials", wsBase, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := e.h.Clients()
			ws, err := websocket.Dial(c.u, c.proto, origin)
			if c.wantErr {
				if err == nil {
					ws.Close()
					t.Fatalf("%s: dial succeeded, want error", c.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: dial: %v", c.name, err)
			}
			defer ws.Close()
			// The server echoes the single subprotocol in the 101 response;
			// the client keeps it in Config().Protocol.
			if c.proto != "" {
				if len(ws.Config().Protocol) != 1 || ws.Config().Protocol[0] != c.proto {
					t.Errorf("%s: echoed protocol = %v, want [%s]", c.name, ws.Config().Protocol, c.proto)
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			for e.h.Clients() <= before && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if e.h.Clients() <= before {
				t.Errorf("%s: client not registered on hub", c.name)
			}
		})
	}
}

// TestMessageFilesSignedURL: message attachment URLs in GET /api/messages are
// signed; a bare GET on them returns the uploaded bytes.
func TestMessageFilesSignedURL(t *testing.T) {
	e := newEnv(t)
	l1, _, _, _ := topo(t, e)

	code, body, _ := upload(t, e.srv.URL+"/api/files", adminTok, "file", "shot.png", pngBytes)
	if code != http.StatusCreated {
		t.Fatalf("upload: got %d want 201: %v", code, body)
	}
	fid := int(body["id"].(float64))

	// Boss dispatches to L1 with the attachment.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "attach", "file_ids": []any{float64(fid)}})
	if code != http.StatusCreated {
		t.Fatalf("dispatch with file: got %d want 201: %v", code, body)
	}

	// The thread carries the message with a signed file url.
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?to=%d", e.srv.URL, l1), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("messages: got %d", code)
	}
	var found map[string]any
	raw, _ := body["messages"].([]any)
	for _, m := range raw {
		row := m.(map[string]any)
		files, _ := row["files"].([]any)
		if len(files) == 1 {
			found = row
		}
	}
	if found == nil {
		t.Fatalf("no message with files in L1 thread: %v", raw)
	}
	url := found["files"].([]any)[0].(map[string]any)["url"].(string)
	if !strings.Contains(url, "sig=") || !strings.Contains(url, "exp=") {
		t.Fatalf("message file url not signed: %q", url)
	}
	resp, err := http.Get(e.srv.URL + url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bare GET on signed message file url: got %d want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(b, pngBytes) {
		t.Errorf("body mismatch (%d bytes)", len(b))
	}
}

// TestSignBrokenFailsClosed (F1): when the signing key could not be generated
// (signBroken=true), a zero/nil key would make HMAC forgeable, so the server
// must fail closed: signFileURL mints no signature and fileURLAuth rejects
// every URL — including one self-signed with the (zero) key.
func TestSignBrokenFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.api.signBroken = true

	// signFileURL must return the bare path (no ?exp=&sig=), so callers fall
	// through to real-token auth (which 401s without a token) instead of
	// presenting a URL that could verify against a zero key.
	if got := e.api.signFileURL("/api/files/x.png"); got != "/api/files/x.png" {
		t.Errorf("signFileURL with signBroken: got %q, want bare path", got)
	}

	// Even a URL self-signed with the (zero) key must be rejected.
	signed := signAt(e.api, "/api/files/x.png", time.Now().Unix()+3600)
	r, err := http.NewRequest(http.MethodGet, e.srv.URL+signed, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signBroken: GET self-signed URL got %d want 401", resp.StatusCode)
	}
}
