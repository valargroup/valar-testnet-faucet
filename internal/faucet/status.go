package faucet

import (
	"context"
	"time"

	"github.com/valargroup/valar-testnet-faucet/internal/store"
	"github.com/valargroup/valar-testnet-faucet/internal/zecd"
)

// Status is the periodically refreshed public view of the faucet.
type Status struct {
	// Ready is true when claims are accepted; Reason explains why not.
	Ready  bool
	Reason string

	WalletReachable bool
	WalletSynced    bool
	ChainHeight     int64
	Balance         zecd.Balances
	Stats           store.Stats
	Recent          []store.Claim
	// DonationAddress is a faucet address testers can return funds to.
	DonationAddress string
	UpdatedAt       time.Time
}

const donationAddressKey = "donation_address"

// maxScanLag is how far the wallet may trail the tip and still count as synced. zecd
// scans each new block a few seconds after it arrives; without slack the faucet would
// flap to "syncing" on every block. Matches zecd's own [health] max_scan_lag default.
const maxScanLag = 4

// Status returns the latest snapshot.
func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *Service) runStatus(ctx context.Context) {
	t := time.NewTicker(s.cfg.StatusInterval)
	defer t.Stop()
	for {
		s.RefreshStatus(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RefreshStatus rebuilds the snapshot now; Run also refreshes it periodically. Each
// source degrades independently, so a zecd outage still leaves the page showing recent
// payouts.
func (s *Service) RefreshStatus(ctx context.Context) {
	st := Status{UpdatedAt: time.Now()}

	chain, chainErr := s.wallet.ChainInfo(ctx)
	bal, balErr := s.wallet.Balances(ctx)
	st.WalletReachable = chainErr == nil && balErr == nil
	if st.WalletReachable {
		st.ChainHeight = chain.Headers
		st.WalletSynced = chain.Headers > 0 && chain.ScanLag() <= maxScanLag
		st.Balance = bal
	} else if ctx.Err() == nil {
		s.log.Warn("wallet status unavailable", "chain_err", chainErr, "balance_err", balErr)
	}

	var err error
	if st.Stats, err = s.store.Stats(ctx, s.cfg.Limits.Window); err != nil && ctx.Err() == nil {
		s.log.Error("status stats", "err", err)
	}
	if st.Recent, err = s.store.RecentSent(ctx, s.cfg.RecentCount); err != nil && ctx.Err() == nil {
		s.log.Error("status recent payouts", "err", err)
	}
	st.DonationAddress = s.donationAddress(ctx, st.WalletReachable)

	need := s.cfg.Limits.PayoutZat + s.cfg.FeeReserveZat
	switch {
	case !st.WalletReachable:
		st.Reason = "wallet unreachable"
	case !st.WalletSynced:
		st.Reason = "wallet syncing"
	case bal.Trusted+bal.UntrustedPending < need:
		st.Reason = "faucet is empty"
	default:
		st.Ready = true
	}

	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

// donationAddress returns the faucet's receive address, asking zecd for one the first
// time and persisting it so every page shows the same address.
func (s *Service) donationAddress(ctx context.Context, walletUp bool) string {
	addr, ok, err := s.store.Meta(ctx, donationAddressKey)
	if err != nil {
		s.log.Error("load donation address", "err", err)
		return ""
	}
	if ok || !walletUp {
		return addr
	}
	addr, err = s.wallet.NewAddress(ctx)
	if err != nil {
		s.log.Warn("get donation address", "err", err)
		return ""
	}
	if err := s.store.SetMeta(ctx, donationAddressKey, addr); err != nil {
		s.log.Error("save donation address", "err", err)
	}
	return addr
}
