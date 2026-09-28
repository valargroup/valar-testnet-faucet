package zecd_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/valargroup/valar-testnet-faucet/internal/jsonrpc"
	"github.com/valargroup/valar-testnet-faucet/internal/zecd"
)

// fakeZecd answers each method with a canned result or error body.
func fakeZecd(t *testing.T, answers map[string]string, seen *[]any) *zecd.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		if seen != nil {
			*seen = req.Params
		}
		body, ok := answers[req.Method]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"result":null,"error":{"code":-32601,"message":"Method not found"}}`)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return zecd.New(srv.URL, "u", "p")
}

func TestSendToAddress(t *testing.T) {
	var params []any
	c := fakeZecd(t, map[string]string{
		"sendtoaddress": `{"result":"aa11"}`,
	}, &params)
	txid, err := c.SendToAddress(context.Background(), "utest1x", 12_500_000)
	require.NoError(t, err)
	require.Equal(t, "aa11", txid)
	require.Equal(t, []any{"utest1x", "0.12500000"}, params)
}

func TestSendToAddressInsufficientFunds(t *testing.T) {
	c := fakeZecd(t, map[string]string{
		"sendtoaddress": `{"result":null,"error":{"code":-6,"message":"Insufficient funds"}}`,
	}, nil)
	_, err := c.SendToAddress(context.Background(), "utest1x", 1)
	code, ok := zecd.ErrorCode(err)
	require.True(t, ok)
	require.Equal(t, zecd.CodeInsufficientFunds, code)

	_, ok = zecd.ErrorCode(jsonrpc.ErrNotDelivered)
	require.False(t, ok)
}

func TestBalances(t *testing.T) {
	c := fakeZecd(t, map[string]string{
		"getbalances": `{"result":{"mine":{"trusted":12.50000000,"untrusted_pending":0.12500000,"immature":0.00000000,"coinbase":10.00000000},"lastprocessedblock":{"hash":"00","height":5}}}`,
	}, nil)
	b, err := c.Balances(context.Background())
	require.NoError(t, err)
	require.Equal(t, zecd.Balances{Trusted: 1_250_000_000, UntrustedPending: 12_500_000, Coinbase: 1_000_000_000}, b)
	require.Equal(t, int64(250_000_000), b.Spendable())
	require.Equal(t, int64(0), zecd.Balances{Trusted: 1, Coinbase: 5}.Spendable())
}

func TestReads(t *testing.T) {
	c := fakeZecd(t, map[string]string{
		"z_validateaddress": `{"result":{"isvalid":true,"address":"utest1x","address_type":"unified","ismine":false,"receivers":["orchard"]}}`,
		"gettransaction":    `{"result":{"txid":"aa","hex":"0500ff","confirmations":0}}`,
		"getblockchaininfo": `{"result":{"chain":"test","blocks":4408914,"headers":4408914}}`,
		"getnewaddress":     `{"result":"utest1new"}`,
	}, nil)
	ctx := context.Background()

	info, err := c.ValidateAddress(ctx, "utest1x")
	require.NoError(t, err)
	require.Equal(t, zecd.AddressInfo{IsValid: true, AddressType: "unified"}, info)

	hex, err := c.RawTransaction(ctx, "aa")
	require.NoError(t, err)
	require.Equal(t, "0500ff", hex)

	chain, err := c.ChainInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, zecd.ChainInfo{Chain: "test", Blocks: 4408914, Headers: 4408914}, chain)
	require.Equal(t, int64(0), chain.ScanLag())
	require.Equal(t, int64(3), zecd.ChainInfo{Blocks: 7, Headers: 10}.ScanLag())
	require.Equal(t, int64(0), zecd.ChainInfo{Blocks: 10, Headers: 7}.ScanLag())

	addr, err := c.NewAddress(ctx)
	require.NoError(t, err)
	require.Equal(t, "utest1new", addr)
}
