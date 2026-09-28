// Package faucettest provides in-memory fakes of the faucet's wallet and broadcaster.
package faucettest

import (
	"context"
	"strings"
	"sync"

	"github.com/valargroup/valar-testnet-faucet/internal/broadcast"
	"github.com/valargroup/valar-testnet-faucet/internal/zecd"
)

// Wallet is a scriptable faucet.Wallet.
type Wallet struct {
	mu sync.Mutex

	// SendErr, when set, is returned by the next SendToAddress calls instead of paying.
	SendErr error
	// ValidateErr, when set, is returned by ValidateAddress.
	ValidateErr error
	// Down makes every status call fail.
	Down bool
	// ScanLag is how many blocks the wallet trails Height.
	ScanLag int64
	Balance zecd.Balances
	Height  int64
	// Own lists addresses the wallet reports as its own.
	Own map[string]bool

	Sends []Send
}

// Send is one recorded SendToAddress call.
type Send struct {
	Address   string
	AmountZat int64
}

var errDown = &zecdDown{}

type zecdDown struct{}

func (*zecdDown) Error() string { return "fake zecd down" }

func (w *Wallet) ValidateAddress(_ context.Context, addr string) (zecd.AddressInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ValidateErr != nil {
		return zecd.AddressInfo{}, w.ValidateErr
	}
	valid := strings.HasPrefix(addr, "utest1") || strings.HasPrefix(addr, "tm") || strings.HasPrefix(addr, "ztestsapling1")
	return zecd.AddressInfo{IsValid: valid, AddressType: "unified", IsMine: w.Own[addr]}, nil
}

func (w *Wallet) SendToAddress(_ context.Context, addr string, amountZat int64) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.SendErr != nil {
		return "", w.SendErr
	}
	w.Sends = append(w.Sends, Send{Address: addr, AmountZat: amountZat})
	return TxID(len(w.Sends)), nil
}

func (w *Wallet) RawTransaction(_ context.Context, txid string) (string, error) {
	return "raw-" + txid, nil
}

func (w *Wallet) Balances(context.Context) (zecd.Balances, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Down {
		return zecd.Balances{}, errDown
	}
	return w.Balance, nil
}

func (w *Wallet) ChainInfo(context.Context) (zecd.ChainInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Down {
		return zecd.ChainInfo{}, errDown
	}
	return zecd.ChainInfo{Chain: "test", Blocks: w.Height - w.ScanLag, Headers: w.Height}, nil
}

func (w *Wallet) NewAddress(context.Context) (string, error) {
	return "utest1faucetdonation", nil
}

// SendCount returns how many sends succeeded.
func (w *Wallet) SendCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.Sends)
}

// SetSendErr sets SendErr under the lock.
func (w *Wallet) SetSendErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.SendErr = err
}

// TxID returns the deterministic txid of the n-th fake send (1-based).
func TxID(n int) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = hexdigits[(n+i)%16]
	}
	return string(b)
}

// Broadcaster is a faucet.Broadcaster whose nodes accept according to Accept.
type Broadcaster struct {
	Accept []bool

	mu   sync.Mutex
	Sent []string
}

func (b *Broadcaster) Len() int { return len(b.Accept) }

func (b *Broadcaster) Broadcast(_ context.Context, raw string) []broadcast.Result {
	b.mu.Lock()
	b.Sent = append(b.Sent, raw)
	b.mu.Unlock()
	out := make([]broadcast.Result, len(b.Accept))
	for i, ok := range b.Accept {
		out[i] = broadcast.Result{Node: "node", OK: ok}
		if !ok {
			out[i].Err = errDown
		}
	}
	return out
}
