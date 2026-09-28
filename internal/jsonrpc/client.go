// Package jsonrpc is a minimal Bitcoin-Core-dialect JSON-RPC client, shared by the zecd
// wallet client and the Zakura node broadcaster.
package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
)

// maxResponseBytes bounds a response body; the largest thing we read is a raw
// transaction hex (zecd caps transactions at 250 kB, so ~500 kB of hex).
const maxResponseBytes = 8 << 20

// Error is a JSON-RPC error object returned by the server. The request reached the
// server and was answered, so for mutating methods it is a definitive outcome.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// ErrNotDelivered marks failures where the server provably did not execute the call:
// the connection could not be opened, credentials were refused, or the server shed the
// request under load. Callers may retry such calls without risking a duplicate effect.
var ErrNotDelivered = errors.New("request not delivered")

// Client calls one JSON-RPC endpoint. It is safe for concurrent use.
type Client struct {
	url      string
	user     string
	password string
	http     *http.Client
	nextID   atomic.Int64
}

// New returns a client for url. user may be empty to send no credentials. Per-call
// deadlines come from the context; httpClient may be nil for a default client.
func New(url, user, password string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{url: url, user: user, password: password, http: httpClient}
}

// URL returns the endpoint this client calls.
func (c *Client) URL() string { return c.url }

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// Call invokes method with positional params and decodes the result into out (which may
// be nil to discard it).
//
// Errors are one of: *Error (the server answered with an error object), an error wrapping
// ErrNotDelivered (the call provably did not run), or any other error, which means the
// outcome is unknown - the request may or may not have been executed.
func (c *Client) Call(ctx context.Context, method string, params []any, out any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(request{JSONRPC: "1.0", ID: c.nextID.Add(1), Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("%s: encode request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if isDialError(err) {
			return fmt.Errorf("%s: %w: %v", method, ErrNotDelivered, err)
		}
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s: %w: HTTP %d (check RPC credentials)", method, ErrNotDelivered, resp.StatusCode)
	case http.StatusServiceUnavailable:
		return fmt.Errorf("%s: %w: HTTP 503 (server busy)", method, ErrNotDelivered)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", method, err)
	}
	var rpcResp response
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return fmt.Errorf("%s: HTTP %d with undecodable body: %w", method, resp.StatusCode, err)
	}
	if rpcResp.Error != nil {
		return rpcResp.Error
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: unexpected HTTP %d", method, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(rpcResp.Result, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}

// isDialError reports whether err happened while opening the connection, before any
// request bytes were written.
func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}
