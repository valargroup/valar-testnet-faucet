package store_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/valargroup/valar-testnet-faucet/internal/store"
)

var limits = store.Limits{
	PayoutZat:          12_500_000,
	IPWindowLimitZat:   12_500_000,
	AddressCooldown:    24 * time.Hour,
	GlobalWindowCapZat: 50_000_000, // 4 claims
	Window:             24 * time.Hour,
	MaxQueue:           3,
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newStore(t *testing.T) (*store.Store, *clock) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "faucet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	s.SetClock(c.now)
	return s, c
}

// drain marks every queued claim sent so queue depth does not interfere with a test.
func drain(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	for {
		c, ok, err := s.TakeNext(ctx)
		require.NoError(t, err)
		if !ok {
			return
		}
		require.NoError(t, s.MarkSent(ctx, c.ID, "tx-"+c.ID))
	}
}

func requireLimit(t *testing.T, err error, reason store.Reason, retry time.Duration) {
	t.Helper()
	var le *store.LimitError
	require.ErrorAs(t, err, &le)
	require.Equal(t, reason, le.Reason)
	require.Equal(t, retry, le.RetryAfter)
}

func TestReserveLimits(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		run  func(t *testing.T, s *store.Store, c *clock)
	}{
		{
			name: "first claim is queued",
			run: func(t *testing.T, s *store.Store, _ *clock) {
				cl, err := s.Reserve(ctx, "1.2.3.4", "utest1a", limits)
				require.NoError(t, err)
				require.Equal(t, store.StatusQueued, cl.Status)
				require.Equal(t, int64(12_500_000), cl.AmountZat)
				require.Len(t, cl.ID, 32)
			},
		},
		{
			name: "same IP within window is refused until the first claim ages out",
			run: func(t *testing.T, s *store.Store, c *clock) {
				_, err := s.Reserve(ctx, "1.2.3.4", "utest1a", limits)
				require.NoError(t, err)
				c.advance(7 * time.Hour)
				_, err = s.Reserve(ctx, "1.2.3.4", "utest1b", limits)
				requireLimit(t, err, store.ReasonIPLimit, 17*time.Hour)
			},
		},
		{
			name: "same IP exactly one window later succeeds",
			run: func(t *testing.T, s *store.Store, c *clock) {
				_, err := s.Reserve(ctx, "1.2.3.4", "utest1a", limits)
				require.NoError(t, err)
				drain(t, s)
				c.advance(24*time.Hour - time.Second)
				_, err = s.Reserve(ctx, "1.2.3.4", "utest1b", limits)
				requireLimit(t, err, store.ReasonIPLimit, time.Second)
				c.advance(time.Second)
				_, err = s.Reserve(ctx, "1.2.3.4", "utest1b", limits)
				require.NoError(t, err)
			},
		},
		{
			name: "failed claims do not count against the IP",
			run: func(t *testing.T, s *store.Store, _ *clock) {
				cl, err := s.Reserve(ctx, "1.2.3.4", "utest1a", limits)
				require.NoError(t, err)
				taken, ok, err := s.TakeNext(ctx)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, cl.ID, taken.ID)
				require.NoError(t, s.MarkFailed(ctx, cl.ID, "rejected"))
				_, err = s.Reserve(ctx, "1.2.3.4", "utest1a", limits)
				require.NoError(t, err)
			},
		},
		{
			name: "review claims count against the IP",
			run: func(t *testing.T, s *store.Store, _ *clock) {
				cl, err := s.Reserve(ctx, "1.2.3.4", "utest1a", limits)
				require.NoError(t, err)
				_, _, err = s.TakeNext(ctx)
				require.NoError(t, err)
				require.NoError(t, s.MarkReview(ctx, cl.ID, "timeout"))
				_, err = s.Reserve(ctx, "1.2.3.4", "utest1b", limits)
				requireLimit(t, err, store.ReasonIPLimit, 24*time.Hour)
			},
		},
		{
			name: "same address from another IP hits the cooldown",
			run: func(t *testing.T, s *store.Store, c *clock) {
				_, err := s.Reserve(ctx, "1.2.3.4", "utest1a", limits)
				require.NoError(t, err)
				c.advance(time.Hour)
				_, err = s.Reserve(ctx, "5.6.7.8", "utest1a", limits)
				requireLimit(t, err, store.ReasonAddressCooldown, 23*time.Hour)
			},
		},
		{
			name: "global cap refuses once the window total is reached",
			run: func(t *testing.T, s *store.Store, c *clock) {
				for i, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"} {
					_, err := s.Reserve(ctx, ip, "utest1-"+ip, limits)
					require.NoError(t, err, "claim %d", i)
					drain(t, s)
					c.advance(time.Hour)
				}
				_, err := s.Reserve(ctx, "10.0.0.5", "utest1-new", limits)
				// The oldest claim is 4h old; it ages out in 20h.
				requireLimit(t, err, store.ReasonGlobalCap, 20*time.Hour)
			},
		},
		{
			name: "queue full refuses new claims",
			run: func(t *testing.T, s *store.Store, _ *clock) {
				for _, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
					_, err := s.Reserve(ctx, ip, "utest1-"+ip, limits)
					require.NoError(t, err)
				}
				_, err := s.Reserve(ctx, "10.0.0.4", "utest1-x", limits)
				requireLimit(t, err, store.ReasonQueueFull, time.Minute)
			},
		},
		{
			name: "payout above the IP limit can never succeed",
			run: func(t *testing.T, s *store.Store, _ *clock) {
				big := limits
				big.PayoutZat = limits.IPWindowLimitZat + 1
				_, err := s.Reserve(ctx, "1.2.3.4", "utest1a", big)
				requireLimit(t, err, store.ReasonIPLimit, 24*time.Hour)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, c := newStore(t)
			tt.run(t, s, c)
		})
	}
}

func TestReserveIsAtomicUnderConcurrency(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Reserve(ctx, "1.2.3.4", "utest1a", limits)
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	require.Equal(t, 1, ok)
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)

	_, ok, err := s.TakeNext(ctx)
	require.NoError(t, err)
	require.False(t, ok, "empty queue")

	a, err := s.Reserve(ctx, "10.0.0.1", "utest1a", limits)
	require.NoError(t, err)
	b, err := s.Reserve(ctx, "10.0.0.2", "utest1b", limits)
	require.NoError(t, err)

	// FIFO.
	got, ok, err := s.TakeNext(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.ID, got.ID)
	require.Equal(t, store.StatusSending, got.Status)

	// Requeue puts it back at the head.
	require.NoError(t, s.Requeue(ctx, a.ID))
	got, _, err = s.TakeNext(ctx)
	require.NoError(t, err)
	require.Equal(t, a.ID, got.ID)

	require.NoError(t, s.MarkSent(ctx, a.ID, "txa"))
	require.NoError(t, s.SetBroadcastOK(ctx, a.ID, 3))
	require.Error(t, s.MarkSent(ctx, a.ID, "again"), "only a sending claim can be marked sent")

	sent, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusSent, sent.Status)
	require.Equal(t, "txa", sent.TxID)
	require.Equal(t, 3, sent.BroadcastOK)

	// A crash while b is sending leaves it for review.
	_, _, err = s.TakeNext(ctx)
	require.NoError(t, err)
	n, err := s.RecoverInterrupted(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	rb, err := s.Get(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusReview, rb.Status)

	// Stale queued claims expire to failed.
	cc, err := s.Reserve(ctx, "10.0.0.3", "utest1c", limits)
	require.NoError(t, err)
	c.advance(31 * time.Minute)
	n, err = s.ExpireQueued(ctx, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	rc, err := s.Get(ctx, cc.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusFailed, rc.Status)

	st, err := s.Stats(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, store.Stats{SentCount: 1, SentZat: 12_500_000, CommittedZat: 25_000_000, Pending: 0}, st)

	recent, err := s.RecentSent(ctx, 5)
	require.NoError(t, err)
	require.Len(t, recent, 1)
	require.Equal(t, "txa", recent[0].TxID)

	_, err = s.Get(ctx, "missing")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestMeta(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, ok, err := s.Meta(ctx, "k")
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, s.SetMeta(ctx, "k", "v1"))
	require.NoError(t, s.SetMeta(ctx, "k", "v2"))
	v, ok, err := s.Meta(ctx, "k")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "v2", v)
}
