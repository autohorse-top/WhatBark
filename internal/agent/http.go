package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// APIError is a non-2xx response from the platform API (S27 review R1).
// Status carries the HTTP status code so callers can classify outcomes with
// errors.As (e.g. watchCancel treats 403/404 on the task probe as "the task
// is no longer this agent's" instead of a transient failure). Body is the
// server's error message, or the status line when the body is not a JSON
// error object.
type APIError struct {
	Status int
	Body   string
}

// Error keeps the historical "http %d: %s" format so existing logs and
// message-wrapping callers read unchanged.
func (e *APIError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, e.Body)
}

// apiVersion is the protocol version the agent declares on every /api
// request (docs/PROTOCOL.md §2). Mirrored locally as a const so
// internal/agent keeps its zero dependency on internal/api; it must stay in
// sync with api.APIVersionCurr.
const (
	apiVersion       = "2"
	apiVersionHeader = "X-Whatbark-Api"
)

// do executes an HTTP request with the agent's bearer token.
// body is JSON-encoded if non-nil; out is JSON-decoded if non-nil.
// Non-2xx responses return *APIError; 2xx behaviour is unchanged.
func (a *Agent) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.ServerURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set(apiVersionHeader, apiVersion) // protocol-v2: version negotiation
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Error
		if msg == "" {
			msg = resp.Status
		}
		return &APIError{Status: resp.StatusCode, Body: msg}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
