package broadcast_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/valargroup/valar-testnet-faucet/internal/broadcast"
)

func TestParseNodes(t *testing.T) {
	tests := []struct {
		spec    string
		want    []broadcast.Node
		wantErr bool
	}{
		{spec: "", want: nil},
		{spec: " , ", want: nil},
		{
			spec: "a=http://127.0.0.1:18232, b=http://164.92.209.78:18232",
			want: []broadcast.Node{{Name: "a", URL: "http://127.0.0.1:18232"}, {Name: "b", URL: "http://164.92.209.78:18232"}},
		},
		{spec: "http://10.0.0.1:18232", want: []broadcast.Node{{Name: "http://10.0.0.1:18232", URL: "http://10.0.0.1:18232"}}},
		{spec: "a=10.0.0.1:18232", wantErr: true},
		{spec: "=http://10.0.0.1:18232", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := broadcast.ParseNodes(tt.spec)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func node(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestBroadcast(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()

	nodes := []broadcast.Node{
		{Name: "accepts", URL: node(t, `{"jsonrpc":"2.0","result":"txid","id":1}`)},
		{Name: "in-mempool", URL: node(t, `{"jsonrpc":"2.0","error":{"code":-25,"message":"transaction is already in the mempool"},"id":1}`)},
		{Name: "mined", URL: node(t, `{"jsonrpc":"2.0","error":{"code":-1,"message":"any transaction with the same effects will be rejected from the mempool until a chain reset: transaction was committed to the best chain"},"id":1}`)},
		{Name: "rejects", URL: node(t, `{"jsonrpc":"2.0","error":{"code":-26,"message":"bad-txns"},"id":1}`)},
		{Name: "down", URL: downURL},
	}
	f := broadcast.New(nodes, time.Second)
	require.Equal(t, 5, f.Len())

	res := f.Broadcast(context.Background(), "0500")
	require.Len(t, res, 5)
	want := map[string]bool{"accepts": true, "in-mempool": true, "mined": true, "rejects": false, "down": false}
	for _, r := range res {
		require.Equal(t, want[r.Node], r.OK, r.Node)
		if r.OK {
			continue
		}
		require.Error(t, r.Err, r.Node)
	}
}
