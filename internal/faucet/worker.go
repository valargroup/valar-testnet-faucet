package faucet

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/valargroup/valar-testnet-faucet/internal/jsonrpc"
	"github.com/valargroup/valar-testnet-faucet/internal/store"
	"github.com/valargroup/valar-testnet-faucet/internal/zecd"
)

// storeWriteTimeout bounds bookkeeping writes made after a send, which run detached
// from shutdown so a paid claim is always recorded.
const storeWriteTimeout = 10 * time.Second

// runWorker pays queued claims one at a time until ctx is done. Sends are serialized
// because zecd proves one transaction at a time anyway, and a single sender keeps the
// "never pay a claim twice" argument trivial: only this loop moves a claim out of
// sending, and it only picks claims that are queued.
func (s *Service) runWorker(ctx context.Context) {
	for ctx.Err() == nil {
		if n, err := s.store.ExpireQueued(ctx, s.cfg.QueueTTL); err != nil {
			s.log.Error("expire queued claims", "err", err)
		} else if n > 0 {
			s.log.Warn("queued claims expired unpaid", "count", n)
		}

		c, ok, err := s.store.TakeNext(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("take next claim", "err", err)
			}
			sleep(ctx, s.cfg.RetryBackoff)
			continue
		}
		if !ok {
			select {
			case <-ctx.Done():
			case <-s.wake:
			case <-time.After(s.cfg.IdlePoll):
			}
			continue
		}
		if !s.pay(ctx, c) {
			sleep(ctx, s.cfg.RetryBackoff)
		}
	}
}

// pay sends one claim and records the outcome. It reports whether the worker may move
// straight on to the next claim; false asks it to back off first.
//
// The send and the bookkeeping after it run detached from ctx: once zecd is asked to
// pay, shutdown waits for the answer rather than abandoning a send with an unknown
// outcome.
func (s *Service) pay(ctx context.Context, c store.Claim) bool {
	bg := context.WithoutCancel(ctx)
	log := s.log.With("claim", c.ID)

	txid, err := s.wallet.SendToAddress(bg, c.Address, c.AmountZat)
	wctx, cancel := context.WithTimeout(bg, storeWriteTimeout)
	defer cancel()

	switch code, isRPC := zecd.ErrorCode(err); {
	case err == nil:
		if err := s.store.MarkSent(wctx, c.ID, txid); err != nil {
			// Paid but unrecorded: the claim stays "sending", which the next start moves
			// to review. It is never picked up for payment again.
			log.Error("claim paid but not recorded", "txid", txid, "err", err)
			return true
		}
		log.Info("claim paid", "txid", txid, "amount_zat", c.AmountZat)
		s.fanout(bg, c.ID, txid)
		// Show the payout and the new balance now rather than at the next tick.
		s.RefreshStatus(ctx)
		return true

	case errors.Is(err, jsonrpc.ErrNotDelivered):
		s.requeue(wctx, c.ID, log.With("err", err), "wallet did not accept the request; will retry")
		return false

	case isRPC && code == zecd.CodeInsufficientFunds:
		s.requeue(wctx, c.ID, log, "no spendable notes yet (waiting for change to confirm or for funding)")
		return false

	case isRPC:
		log.Warn("claim failed", "code", code, "err", err)
		if err := s.store.MarkFailed(wctx, c.ID, failureMessage(code)); err != nil {
			log.Error("record failed claim", "err", err)
		}
		return true

	default:
		// Transport error or timeout after the request was sent: zecd may or may not
		// have committed the transaction. Never guess; leave it for an operator.
		log.Error("claim outcome unknown, moved to review", "err", err)
		if err := s.store.MarkReview(wctx, c.ID, "payment outcome unknown; under review"); err != nil {
			log.Error("record review claim", "err", err)
		}
		return false
	}
}

func (s *Service) requeue(ctx context.Context, id string, log *slog.Logger, why string) {
	if err := s.store.Requeue(ctx, id); err != nil {
		log.Error("requeue claim", "err", err)
		return
	}
	log.Info("claim requeued", "why", why)
}

// fanout pushes a paid transaction to every configured Zakura node and records how
// many accepted it. zecd has already broadcast through its own upstream; this only
// makes propagation independent of gossip. Failures are logged, never fatal.
func (s *Service) fanout(ctx context.Context, id, txid string) {
	if s.bcast.Len() == 0 {
		return
	}
	log := s.log.With("claim", id, "txid", txid)
	raw, err := s.wallet.RawTransaction(ctx, txid)
	if err != nil {
		log.Warn("fetch raw transaction for fan-out", "err", err)
		return
	}
	ok := 0
	for _, r := range s.bcast.Broadcast(ctx, raw) {
		if r.OK {
			ok++
			continue
		}
		log.Warn("node did not accept broadcast", "node", r.Node, "err", r.Err)
	}
	wctx, cancel := context.WithTimeout(ctx, storeWriteTimeout)
	defer cancel()
	if err := s.store.SetBroadcastOK(wctx, id, ok); err != nil {
		log.Error("record broadcast result", "err", err)
	}
	log.Info("broadcast fan-out", "accepted", ok, "nodes", s.bcast.Len())
}

// failureMessage is the user-facing reason for a claim zecd definitively refused.
func failureMessage(code int) string {
	switch code {
	case zecd.CodeVerifyRejected:
		return "the network rejected the payment; please try again"
	case -5, -8:
		return "the wallet cannot pay this address"
	default:
		return "the wallet could not build the payment; please try again"
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
