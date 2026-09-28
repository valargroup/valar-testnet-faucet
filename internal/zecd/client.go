// Package zecd is a typed client for the subset of the zecd wallet daemon's JSON-RPC API
// the faucet uses (https://github.com/zecrocks/zecd).
package zecd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/valargroup/valar-testnet-faucet/internal/jsonrpc"
	"github.com/valargroup/valar-testnet-faucet/internal/zat"
)

// Bitcoin-Core error codes zecd returns that the faucet acts on.
const (
	CodeInsufficientFunds = -6  // no spendable notes right now (often: change still confirming)
	CodeVerifyRejected    = -26 // the upstream node rejected the transaction
)

// sendTimeout bounds sendtoaddress, which proves and broadcasts synchronously.
const sendTimeout = 150 * time.Second

// readTimeout bounds every other call.
const readTimeout = 15 * time.Second

// Client talks to one zecd wallet over JSON-RPC.
type Client struct {
	rpc *jsonrpc.Client
}

// New returns a client for the zecd RPC endpoint at url (for example
// "http://127.0.0.1:18890"), authenticating with user and password.
//
// Keep-alives are disabled: a POST on a reused connection that the server closed at the
// same moment fails ambiguously, and a send must never be ambiguous when it can be
// avoided. zecd is on loopback, so a fresh connection per call costs nothing.
func New(url, user, password string) *Client {
	httpClient := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	return &Client{rpc: jsonrpc.New(url, user, password, httpClient)}
}

// AddressInfo is the z_validateaddress result.
type AddressInfo struct {
	IsValid     bool   `json:"isvalid"`
	AddressType string `json:"address_type"`
	IsMine      bool   `json:"ismine"`
}

// ValidateAddress checks addr against the wallet's network and reports whether the
// wallet owns it.
func (c *Client) ValidateAddress(ctx context.Context, addr string) (AddressInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var out AddressInfo
	err := c.rpc.Call(ctx, "z_validateaddress", []any{addr}, &out)
	return out, err
}

// SendToAddress pays amountZat zatoshis to addr and returns the txid. zecd proves,
// stores and broadcasts the transaction before returning; it rebroadcasts on its own
// afterwards, so a send that returned a txid must never be retried.
func (c *Client) SendToAddress(ctx context.Context, addr string, amountZat int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	var txid string
	if err := c.rpc.Call(ctx, "sendtoaddress", []any{addr, zat.Format(amountZat)}, &txid); err != nil {
		return "", err
	}
	if txid == "" {
		return "", errors.New("sendtoaddress returned an empty txid")
	}
	return txid, nil
}

// RawTransaction returns the hex-encoded raw transaction for one of the wallet's txids.
func (c *Client) RawTransaction(ctx context.Context, txid string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var out struct {
		Hex string `json:"hex"`
	}
	if err := c.rpc.Call(ctx, "gettransaction", []any{txid}, &out); err != nil {
		return "", err
	}
	if out.Hex == "" {
		return "", fmt.Errorf("gettransaction %s: no hex in result", txid)
	}
	return out.Hex, nil
}

// Balances is the wallet balance split, in zatoshis.
type Balances struct {
	Trusted          int64 // confirmed; includes Coinbase
	UntrustedPending int64 // received or change, waiting for confirmations
	Immature         int64
	// Coinbase is the part of Trusted that is mature transparent coinbase. Ordinary sends
	// can never spend it; it moves only through z_shieldcoinbase.
	Coinbase int64
}

// Spendable is what an ordinary send can use right now.
func (b Balances) Spendable() int64 { return max(b.Trusted-b.Coinbase, 0) }

// Balances returns the wallet's current balance split.
func (c *Client) Balances(ctx context.Context) (Balances, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var out struct {
		Mine struct {
			Trusted          json.Number `json:"trusted"`
			UntrustedPending json.Number `json:"untrusted_pending"`
			Immature         json.Number `json:"immature"`
			Coinbase         json.Number `json:"coinbase"`
		} `json:"mine"`
	}
	if err := c.rpc.Call(ctx, "getbalances", nil, &out); err != nil {
		return Balances{}, err
	}
	var b Balances
	var err error
	for _, f := range []struct {
		dst *int64
		src json.Number
	}{
		{&b.Trusted, out.Mine.Trusted},
		{&b.UntrustedPending, out.Mine.UntrustedPending},
		{&b.Immature, out.Mine.Immature},
		{&b.Coinbase, out.Mine.Coinbase},
	} {
		if f.src == "" {
			continue
		}
		if *f.dst, err = zat.Parse(f.src.String()); err != nil {
			return Balances{}, fmt.Errorf("getbalances: %w", err)
		}
	}
	return b, nil
}

// ChainInfo is the part of getblockchaininfo the faucet reads.
type ChainInfo struct {
	Chain string `json:"chain"`
	// Blocks is the wallet's fully scanned height; balances are exact as of this block.
	Blocks int64 `json:"blocks"`
	// Headers is the upstream node's chain tip.
	Headers int64 `json:"headers"`
}

// ScanLag is how many blocks the wallet trails the chain tip.
func (c ChainInfo) ScanLag() int64 { return max(c.Headers-c.Blocks, 0) }

// ChainInfo returns the wallet's view of the chain.
func (c *Client) ChainInfo(ctx context.Context) (ChainInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var out ChainInfo
	err := c.rpc.Call(ctx, "getblockchaininfo", nil, &out)
	return out, err
}

// NewAddress returns a fresh unified address for the wallet.
func (c *Client) NewAddress(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var addr string
	err := c.rpc.Call(ctx, "getnewaddress", nil, &addr)
	return addr, err
}

// ErrorCode returns the JSON-RPC error code carried by err, if any.
func ErrorCode(err error) (int, bool) {
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		return rpcErr.Code, true
	}
	return 0, false
}
