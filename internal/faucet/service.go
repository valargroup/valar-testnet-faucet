// Package faucet holds the faucet's business rules: who may claim, how claims are paid,
// and the public status snapshot.
package faucet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/valargroup/valar-testnet-faucet/internal/broadcast"
	"github.com/valargroup/valar-testnet-faucet/internal/store"
	"github.com/valargroup/valar-testnet-faucet/internal/zecd"
)

// Wallet is the zecd surface the faucet needs.
type Wallet interface {
	ValidateAddress(ctx context.Context, addr string) (zecd.AddressInfo, error)
	SendToAddress(ctx context.Context, addr string, amountZat int64) (string, error)
	RawTransaction(ctx context.Context, txid string) (string, error)
	Balances(ctx context.Context) (zecd.Balances, error)
	ChainInfo(ctx context.Context) (zecd.ChainInfo, error)
	NewAddress(ctx context.Context) (string, error)
}

// Broadcaster pushes raw transactions to extra nodes.
type Broadcaster interface {
	Broadcast(ctx context.Context, rawHex string) []broadcast.Result
	Len() int
}

// Config tunes the service.
type Config struct {
	Limits store.Limits
	// QueueTTL fails claims that wait longer than this to be paid.
	QueueTTL time.Duration
	// RetryBackoff is how long the worker waits after a send that could not run.
	RetryBackoff time.Duration
	// IdlePoll is how often the worker checks an empty queue.
	IdlePoll time.Duration
	// StatusInterval is how often the public status snapshot is refreshed.
	StatusInterval time.Duration
	// FeeReserveZat is kept aside for fees when deciding whether the faucet can pay.
	FeeReserveZat int64
	// RecentCount is how many recent payouts the status snapshot lists.
	RecentCount int
}

// DefaultConfig returns production timings around lim.
func DefaultConfig(lim store.Limits) Config {
	return Config{
		Limits:         lim,
		QueueTTL:       30 * time.Minute,
		RetryBackoff:   30 * time.Second,
		IdlePoll:       5 * time.Second,
		StatusInterval: 15 * time.Second,
		FeeReserveZat:  100_000,
		RecentCount:    5,
	}
}

// maxAddressLen bounds submitted addresses; the longest testnet unified address with
// every receiver is well under this.
const maxAddressLen = 512

// Errors returned by Claim besides *store.LimitError.
var (
	ErrInvalidAddress = errors.New("not a valid Zcash testnet address")
	ErrOwnAddress     = errors.New("that address belongs to the faucet")
)

// UnavailableError means the faucet cannot take claims right now.
type UnavailableError struct{ Reason string }

func (e *UnavailableError) Error() string { return "faucet unavailable: " + e.Reason }

// Service is the faucet.
type Service struct {
	cfg    Config
	store  *store.Store
	wallet Wallet
	bcast  Broadcaster
	log    *slog.Logger

	wake chan struct{}

	mu     sync.RWMutex
	status Status
}

// New returns a Service. Call Run to start its background loops.
func New(cfg Config, st *store.Store, w Wallet, b Broadcaster, log *slog.Logger) *Service {
	return &Service{
		cfg: cfg, store: st, wallet: w, bcast: b, log: log,
		wake:   make(chan struct{}, 1),
		status: Status{Reason: "starting"},
	}
}

// Config returns the service configuration.
func (s *Service) Config() Config { return s.cfg }

// BroadcastNodes returns how many nodes each payout is pushed to.
func (s *Service) BroadcastNodes() int { return s.bcast.Len() }

// Claim validates address and queues a payout to it on behalf of ipKey.
//
// It returns ErrInvalidAddress, ErrOwnAddress, *UnavailableError, *store.LimitError, or
// an internal error.
func (s *Service) Claim(ctx context.Context, ipKey, address string) (store.Claim, error) {
	address = strings.TrimSpace(address)
	if address == "" || len(address) > maxAddressLen {
		return store.Claim{}, ErrInvalidAddress
	}
	if st := s.Status(); !st.Ready {
		return store.Claim{}, &UnavailableError{Reason: st.Reason}
	}
	info, err := s.wallet.ValidateAddress(ctx, address)
	if err != nil {
		if _, ok := zecd.ErrorCode(err); ok {
			// zecd answered with an error for this input: treat as invalid.
			return store.Claim{}, ErrInvalidAddress
		}
		return store.Claim{}, &UnavailableError{Reason: "wallet unreachable"}
	}
	if !info.IsValid {
		return store.Claim{}, ErrInvalidAddress
	}
	if info.IsMine {
		return store.Claim{}, ErrOwnAddress
	}
	c, err := s.store.Reserve(ctx, ipKey, address, s.cfg.Limits)
	if err != nil {
		return store.Claim{}, err
	}
	s.log.Info("claim queued", "claim", c.ID, "address_type", info.AddressType)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return c, nil
}

// Lookup returns a claim by id.
func (s *Service) Lookup(ctx context.Context, id string) (store.Claim, error) {
	return s.store.Get(ctx, id)
}

// Ping checks the service's own dependencies (the database) for liveness.
func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

// Run starts the payout worker and the status refresher and blocks until ctx is done
// and both have stopped. The worker finishes an in-flight send before returning.
func (s *Service) Run(ctx context.Context) error {
	if n, err := s.store.RecoverInterrupted(ctx); err != nil {
		return err
	} else if n > 0 {
		s.log.Warn("claims interrupted mid-send moved to review", "count", n)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.runStatus(ctx) }()
	go func() { defer wg.Done(); s.runWorker(ctx) }()
	wg.Wait()
	return nil
}

// IPKey maps a client address to the key rate limits are applied to: the address
// itself for IPv4 (including IPv4-mapped IPv6), and the /64 prefix for IPv6, since one
// subscriber typically controls a whole /64.
func IPKey(addr netip.Addr) (string, error) {
	if !addr.IsValid() {
		return "", fmt.Errorf("invalid client address")
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String(), nil
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return "", fmt.Errorf("ipv6 prefix: %w", err)
	}
	return p.String(), nil
}
