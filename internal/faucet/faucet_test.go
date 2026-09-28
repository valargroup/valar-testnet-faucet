package faucet_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/valargroup/valar-testnet-faucet/internal/faucet"
	"github.com/valargroup/valar-testnet-faucet/internal/faucet/faucettest"
	"github.com/valargroup/valar-testnet-faucet/internal/jsonrpc"
	"github.com/valargroup/valar-testnet-faucet/internal/store"
	"github.com/valargroup/valar-testnet-faucet/internal/zecd"
)

var limits = store.Limits{
	PayoutZat:          12_500_000,
	IPWindowLimitZat:   12_500_000,
	AddressCooldown:    24 * time.Hour,
	GlobalWindowCapZat: 1_250_000_000,
	Window:             24 * time.Hour,
	MaxQueue:           20,
}

type harness struct {
	svc    *faucet.Service
	store  *store.Store
	wallet *faucettest.Wallet
	bcast  *faucettest.Broadcaster
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "faucet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	w := &faucettest.Wallet{Balance: zecd.Balances{Trusted: 10 * 100_000_000}, Height: 4_400_000}
	b := &faucettest.Broadcaster{Accept: []bool{true, true, false}}
	cfg := faucet.DefaultConfig(limits)
	cfg.RetryBackoff = 10 * time.Millisecond
	cfg.IdlePoll = 10 * time.Millisecond
	svc := faucet.New(cfg, st, w, b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.RefreshStatus(ctx)
	return &harness{svc: svc, store: st, wallet: w, bcast: b}
}

func TestIPKey(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "203.0.113.7", want: "203.0.113.7"},
		{in: "::ffff:203.0.113.7", want: "203.0.113.7"},
		{in: "2001:db8:1:2:3:4:5:6", want: "2001:db8:1:2::/64"},
		{in: "2001:db8:1:2:ffff::1", want: "2001:db8:1:2::/64"},
		{in: "fe80::1%eth0", want: "fe80::/64"},
		{in: "::1", want: "::/64"},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var addr netip.Addr
			if tt.in != "" {
				addr = netip.MustParseAddr(tt.in)
			}
			got, err := faucet.IPKey(addr)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestClaimValidation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		setup   func(h *harness)
		address string
		check   func(t *testing.T, err error)
	}{
		{
			name:    "valid address is queued",
			address: "  utest1abc  ",
			check:   func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name:    "empty address",
			address: "   ",
			check:   func(t *testing.T, err error) { require.ErrorIs(t, err, faucet.ErrInvalidAddress) },
		},
		{
			name:    "oversized address",
			address: "utest1" + string(make([]byte, 600)),
			check:   func(t *testing.T, err error) { require.ErrorIs(t, err, faucet.ErrInvalidAddress) },
		},
		{
			name:    "address zecd rejects",
			address: "t1mainnetaddress",
			check:   func(t *testing.T, err error) { require.ErrorIs(t, err, faucet.ErrInvalidAddress) },
		},
		{
			name:    "zecd RPC error on validation means invalid",
			setup:   func(h *harness) { h.wallet.ValidateErr = &jsonrpc.Error{Code: -5, Message: "bad"} },
			address: "utest1abc",
			check:   func(t *testing.T, err error) { require.ErrorIs(t, err, faucet.ErrInvalidAddress) },
		},
		{
			name:    "zecd unreachable on validation means unavailable",
			setup:   func(h *harness) { h.wallet.ValidateErr = fmt.Errorf("dial: %w", jsonrpc.ErrNotDelivered) },
			address: "utest1abc",
			check: func(t *testing.T, err error) {
				var u *faucet.UnavailableError
				require.ErrorAs(t, err, &u)
			},
		},
		{
			name:    "faucet's own address",
			setup:   func(h *harness) { h.wallet.Own = map[string]bool{"utest1mine": true} },
			address: "utest1mine",
			check:   func(t *testing.T, err error) { require.ErrorIs(t, err, faucet.ErrOwnAddress) },
		},
		{
			name: "empty wallet refuses claims",
			setup: func(h *harness) {
				h.wallet.Balance = zecd.Balances{Trusted: 12_500_000}
				h.svc.RefreshStatus(ctx)
			},
			address: "utest1abc",
			check: func(t *testing.T, err error) {
				var u *faucet.UnavailableError
				require.ErrorAs(t, err, &u)
				require.Equal(t, "faucet is empty", u.Reason)
			},
		},
		{
			name: "unshielded coinbase alone is not spendable",
			setup: func(h *harness) {
				h.wallet.Balance = zecd.Balances{Trusted: 4_000 * 100_000_000, Coinbase: 4_000 * 100_000_000}
				h.svc.RefreshStatus(ctx)
			},
			address: "utest1abc",
			check: func(t *testing.T, err error) {
				var u *faucet.UnavailableError
				require.ErrorAs(t, err, &u)
				require.Equal(t, "faucet is empty", u.Reason)
			},
		},
		{
			name: "pending balance still counts as funded",
			setup: func(h *harness) {
				h.wallet.Balance = zecd.Balances{UntrustedPending: 50_000_000}
				h.svc.RefreshStatus(ctx)
			},
			address: "utest1abc",
			check:   func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name: "wallet a few blocks behind still takes claims",
			setup: func(h *harness) {
				h.wallet.ScanLag = 4
				h.svc.RefreshStatus(ctx)
			},
			address: "utest1abc",
			check:   func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name: "syncing wallet refuses claims",
			setup: func(h *harness) {
				h.wallet.ScanLag = 5
				h.svc.RefreshStatus(ctx)
			},
			address: "utest1abc",
			check: func(t *testing.T, err error) {
				var u *faucet.UnavailableError
				require.ErrorAs(t, err, &u)
				require.Equal(t, "wallet syncing", u.Reason)
			},
		},
		{
			name: "unreachable wallet refuses claims",
			setup: func(h *harness) {
				h.wallet.Down = true
				h.svc.RefreshStatus(ctx)
			},
			address: "utest1abc",
			check: func(t *testing.T, err error) {
				var u *faucet.UnavailableError
				require.ErrorAs(t, err, &u)
				require.Equal(t, "wallet unreachable", u.Reason)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.setup != nil {
				tt.setup(h)
			}
			_, err := h.svc.Claim(ctx, "203.0.113.7", tt.address)
			tt.check(t, err)
		})
	}
}

func TestClaimTrimsAddressBeforeStoring(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	c, err := h.svc.Claim(ctx, "203.0.113.7", "\tutest1abc \n")
	require.NoError(t, err)
	require.Equal(t, "utest1abc", c.Address)
	_, err = h.svc.Claim(ctx, "203.0.113.7", "utest1other")
	var le *store.LimitError
	require.ErrorAs(t, err, &le)
	require.Equal(t, store.ReasonIPLimit, le.Reason)
}

func TestPayOutcomes(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name        string
		sendErr     error
		wantStatus  store.Status
		wantNext    bool
		wantSends   int
		wantBcastOK int
		wantMessage string
	}{
		{
			name:        "success records txid and fan-out count",
			wantStatus:  store.StatusSent,
			wantNext:    true,
			wantSends:   1,
			wantBcastOK: 2,
		},
		{
			name:       "not delivered is requeued",
			sendErr:    fmt.Errorf("sendtoaddress: %w: connection refused", jsonrpc.ErrNotDelivered),
			wantStatus: store.StatusQueued,
		},
		{
			name:       "insufficient funds is requeued",
			sendErr:    &jsonrpc.Error{Code: zecd.CodeInsufficientFunds, Message: "Insufficient funds"},
			wantStatus: store.StatusQueued,
		},
		{
			name:        "node rejection fails the claim",
			sendErr:     &jsonrpc.Error{Code: zecd.CodeVerifyRejected, Message: "rejected"},
			wantStatus:  store.StatusFailed,
			wantNext:    true,
			wantMessage: "the network rejected the payment; please try again",
		},
		{
			name:        "invalid parameter fails the claim",
			sendErr:     &jsonrpc.Error{Code: -8, Message: "privacy policy"},
			wantStatus:  store.StatusFailed,
			wantNext:    true,
			wantMessage: "the wallet cannot pay this address",
		},
		{
			name:        "ambiguous transport error goes to review",
			sendErr:     errors.New("sendtoaddress: context deadline exceeded"),
			wantStatus:  store.StatusReview,
			wantMessage: "payment outcome unknown; under review",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			queued, err := h.svc.Claim(ctx, "203.0.113.7", "utest1abc")
			require.NoError(t, err)
			c, ok, err := h.store.TakeNext(ctx)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, queued.ID, c.ID)

			h.wallet.SetSendErr(tt.sendErr)
			require.Equal(t, tt.wantNext, h.svc.Pay(ctx, c))

			got, err := h.store.Get(ctx, c.ID)
			require.NoError(t, err)
			require.Equal(t, tt.wantStatus, got.Status)
			require.Equal(t, tt.wantSends, h.wallet.SendCount())
			require.Equal(t, tt.wantBcastOK, got.BroadcastOK)
			require.Equal(t, tt.wantMessage, got.Error)
			if tt.wantStatus == store.StatusSent {
				require.Equal(t, faucettest.TxID(1), got.TxID)
				require.Equal(t, []string{"raw-" + faucettest.TxID(1)}, h.bcast.Sent)
				require.Equal(t, []faucettest.Send{{Address: "utest1abc", AmountZat: 12_500_000}}, h.wallet.Sends)
			}
		})
	}
}

func TestPaySurvivesCancelledContext(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.Claim(context.Background(), "203.0.113.7", "utest1abc")
	require.NoError(t, err)
	c, _, err := h.store.TakeNext(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown already requested
	h.svc.Pay(ctx, c)

	got, err := h.store.Get(context.Background(), c.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusSent, got.Status, "an accepted send is always recorded")
}

func TestRunPaysQueueAndRecoversInterrupted(t *testing.T) {
	h := newHarness(t)
	bg := context.Background()

	// A claim left "sending" by a previous process must go to review, never be re-paid.
	stale, err := h.svc.Claim(bg, "198.51.100.1", "utest1stale")
	require.NoError(t, err)
	_, _, err = h.store.TakeNext(bg)
	require.NoError(t, err)

	fresh, err := h.svc.Claim(bg, "198.51.100.2", "utest1fresh")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); require.NoError(t, h.svc.Run(ctx)) }()

	require.Eventually(t, func() bool {
		c, err := h.store.Get(bg, fresh.ID)
		return err == nil && c.Status == store.StatusSent
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done

	s, err := h.store.Get(bg, stale.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusReview, s.Status)
	require.Equal(t, 1, h.wallet.SendCount())
	require.Equal(t, "utest1fresh", h.wallet.Sends[0].Address)
}

func TestDonationAddressFollowsWalletSwap(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// A previous wallet's address is still stored.
	require.NoError(t, h.store.SetMeta(ctx, "donation_address", "utest1previouswallet"))
	h.wallet.Own = map[string]bool{"utest1faucetdonation": true}

	svc := faucet.New(faucet.DefaultConfig(limits), h.store, h.wallet, h.bcast, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.RefreshStatus(ctx)
	require.Equal(t, "utest1faucetdonation", svc.Status().DonationAddress)

	// A stored address the wallet owns is kept as is.
	svc = faucet.New(faucet.DefaultConfig(limits), h.store, h.wallet, h.bcast, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.RefreshStatus(ctx)
	require.Equal(t, "utest1faucetdonation", svc.Status().DonationAddress)
}

func TestStatusSnapshot(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	st := h.svc.Status()
	require.True(t, st.Ready)
	require.True(t, st.WalletSynced)
	require.Equal(t, int64(4_400_000), st.ChainHeight)
	require.Equal(t, "utest1faucetdonation", st.DonationAddress)

	_, err := h.svc.Claim(ctx, "203.0.113.7", "utest1abc")
	require.NoError(t, err)
	c, _, err := h.store.TakeNext(ctx)
	require.NoError(t, err)
	h.svc.Pay(ctx, c)
	h.svc.RefreshStatus(ctx)
	st = h.svc.Status()
	require.Equal(t, 1, st.Stats.SentCount)
	require.Len(t, st.Recent, 1)
	require.Equal(t, faucettest.TxID(1), st.Recent[0].TxID)
}
