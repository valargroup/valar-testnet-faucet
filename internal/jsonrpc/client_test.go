package jsonrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/valargroup/valar-testnet-faucet/internal/jsonrpc"
)

func TestCall(t *testing.T) {
	type outcome int
	const (
		ok outcome = iota
		rpcError
		notDelivered
		unknown
	)
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    outcome
		code    int
	}{
		{
			name: "result is decoded",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"result":"abc","error":null,"id":1}`)
			},
			want: ok,
		},
		{
			name: "error object on HTTP 500 is an rpc error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"result":null,"error":{"code":-6,"message":"Insufficient funds"},"id":1}`)
			},
			want: rpcError,
			code: -6,
		},
		{
			name: "error object on HTTP 200 is an rpc error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":-1,"message":"committed"},"id":1}`)
			},
			want: rpcError,
			code: -1,
		},
		{
			name:    "401 was not delivered",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			want:    notDelivered,
		},
		{
			name: "503 work queue was not delivered",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "Work queue depth exceeded", http.StatusServiceUnavailable)
			},
			want: notDelivered,
		},
		{
			name: "garbage body is an unknown outcome",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `<html>`)
			},
			want: unknown,
		},
		{
			name: "server hanging past the deadline is an unknown outcome",
			handler: func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
			},
			want: unknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			c := jsonrpc.New(srv.URL, "", "", nil)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			var out string
			err := c.Call(ctx, "method", nil, &out)
			var rpcErr *jsonrpc.Error
			switch tt.want {
			case ok:
				require.NoError(t, err)
				require.Equal(t, "abc", out)
			case rpcError:
				require.ErrorAs(t, err, &rpcErr)
				require.Equal(t, tt.code, rpcErr.Code)
				require.NotErrorIs(t, err, jsonrpc.ErrNotDelivered)
			case notDelivered:
				require.ErrorIs(t, err, jsonrpc.ErrNotDelivered)
			case unknown:
				require.Error(t, err)
				require.False(t, errors.As(err, &rpcErr))
				require.NotErrorIs(t, err, jsonrpc.ErrNotDelivered)
			}
		})
	}
}

func TestCallDialFailureIsNotDelivered(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	err := jsonrpc.New(url, "", "", nil).Call(context.Background(), "m", nil, nil)
	require.ErrorIs(t, err, jsonrpc.ErrNotDelivered)
}

func TestCallSendsEnvelopeAndAuth(t *testing.T) {
	var got struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}
	var user, pass string
	var hasAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, hasAuth = r.BasicAuth()
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		_, _ = io.WriteString(w, `{"result":null}`)
	}))
	defer srv.Close()

	require.NoError(t, jsonrpc.New(srv.URL, "faucet", "s3cret", nil).Call(context.Background(), "sendtoaddress", []any{"utest1", "0.12500000"}, nil))
	require.True(t, hasAuth)
	require.Equal(t, "faucet", user)
	require.Equal(t, "s3cret", pass)
	require.Equal(t, "1.0", got.JSONRPC)
	require.Equal(t, "sendtoaddress", got.Method)
	require.Equal(t, []any{"utest1", "0.12500000"}, got.Params)

	require.NoError(t, jsonrpc.New(srv.URL, "", "", nil).Call(context.Background(), "getblockcount", nil, nil))
	require.False(t, hasAuth, "no credentials configured, none sent")
	require.Equal(t, []any{}, got.Params, "nil params encode as an empty array")
}
