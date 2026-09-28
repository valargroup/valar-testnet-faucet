package web_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/valargroup/valar-testnet-faucet/internal/faucet"
	"github.com/valargroup/valar-testnet-faucet/internal/faucet/faucettest"
	"github.com/valargroup/valar-testnet-faucet/internal/store"
	"github.com/valargroup/valar-testnet-faucet/internal/web"
	"github.com/valargroup/valar-testnet-faucet/internal/zecd"
)

func TestClientAddr(t *testing.T) {
	tests := []struct {
		name       string
		remote     string
		realIP     string
		trustProxy bool
		want       string
		wantErr    bool
	}{
		{name: "direct peer", remote: "203.0.113.7:5555", want: "203.0.113.7"},
		{name: "proxy header from loopback is trusted", remote: "127.0.0.1:40000", realIP: "198.51.100.9", trustProxy: true, want: "198.51.100.9"},
		{name: "proxy header from ipv6 loopback is trusted", remote: "[::1]:40000", realIP: "2001:db8::1", trustProxy: true, want: "2001:db8::1"},
		{name: "spoofed header from a remote peer is ignored", remote: "203.0.113.7:5555", realIP: "10.0.0.1", trustProxy: true, want: "203.0.113.7"},
		{name: "header ignored when proxy not trusted", remote: "127.0.0.1:40000", realIP: "198.51.100.9", want: "127.0.0.1"},
		{name: "loopback without header is itself", remote: "127.0.0.1:40000", trustProxy: true, want: "127.0.0.1"},
		{name: "malformed header", remote: "127.0.0.1:40000", realIP: "not-an-ip", trustProxy: true, wantErr: true},
		{name: "malformed remote", remote: "garbage", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/claim", nil)
			r.RemoteAddr = tt.remote
			if tt.realIP != "" {
				r.Header.Set("X-Real-IP", tt.realIP)
			}
			got, err := web.ClientAddr(r, tt.trustProxy)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got.String())
		})
	}
}

func TestMaskAddress(t *testing.T) {
	require.Equal(t, "tmShort", web.MaskAddress("tmShort"))
	require.Equal(t, "utest1abcd…uvwxyz", web.MaskAddress("utest1abcdefghijklmnopqrstuvwxyz"))
}

func TestAttemptLimiter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	allow := web.NewAttemptLimiter(2, time.Minute, func() time.Time { return now })
	ok, _ := allow("a")
	require.True(t, ok)
	ok, _ = allow("a")
	require.True(t, ok)
	ok, retry := allow("a")
	require.False(t, ok)
	require.Equal(t, time.Minute, retry)
	ok, _ = allow("b")
	require.True(t, ok, "keys are independent")
	now = now.Add(time.Minute)
	ok, _ = allow("a")
	require.True(t, ok, "window reset")
}

type apiHarness struct {
	srv    *httptest.Server
	wallet *faucettest.Wallet
	store  *store.Store
	svc    *faucet.Service
}

func newAPI(t *testing.T) *apiHarness {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "faucet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	w := &faucettest.Wallet{Balance: zecd.Balances{Trusted: 5 * 100_000_000}, Height: 4_408_914}
	lim := store.Limits{
		PayoutZat: 12_500_000, IPWindowLimitZat: 12_500_000, AddressCooldown: 24 * time.Hour,
		GlobalWindowCapZat: 1_250_000_000, Window: 24 * time.Hour, MaxQueue: 20,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := faucet.New(faucet.DefaultConfig(lim), st, w, &faucettest.Broadcaster{Accept: []bool{true, true, true}}, log)
	svc.RefreshStatus(ctx)
	h := web.New(web.Config{
		TrustProxy:    true,
		ExplorerTxURL: "https://explorer.example/tx/{txid}",
		StatusPageURL: "https://status.example/",
	}, svc, log)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &apiHarness{srv: srv, wallet: w, store: st, svc: svc}
}

func (a *apiHarness) claim(t *testing.T, ip, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+"/api/claim", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Real-IP", ip) // the test client connects from loopback, like Caddy
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return resp, out
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, b
}

func TestClaimAPI(t *testing.T) {
	a := newAPI(t)

	resp, body := a.claim(t, "203.0.113.7", `{"address":"utest1abc"}`)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Equal(t, "queued", body["status"])
	require.Equal(t, "0.125", body["amount"])
	id := body["id"].(string)
	require.Len(t, id, 32)

	// Same IP, different address: refused for a full day.
	resp, body = a.claim(t, "203.0.113.7", `{"address":"utest1other"}`)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "ip_limit", body["error"])
	// Claims are stamped in whole seconds, so allow the clock to have ticked over.
	retry, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	require.NoError(t, err)
	require.InDelta(t, 86400, retry, 1)
	require.EqualValues(t, retry, body["retryAfterSeconds"])
	require.Contains(t, body["message"], "0.125 TAZ")

	// Same /64, different host: same key.
	resp, _ = a.claim(t, "2001:db8:1:2::1", `{"address":"utest1v6a"}`)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp, body = a.claim(t, "2001:db8:1:2::ffff", `{"address":"utest1v6b"}`)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "ip_limit", body["error"])

	// Same address from a new IP: address cooldown.
	resp, body = a.claim(t, "198.51.100.1", `{"address":"utest1abc"}`)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "address_cooldown", body["error"])

	// Bad input.
	resp, body = a.claim(t, "198.51.100.2", `{"address":"t1mainnet"}`)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_address", body["error"])
	resp, body = a.claim(t, "198.51.100.3", `not json`)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "bad_request", body["error"])

	// Claim status is visible by id.
	resp, raw := get(t, a.srv.URL+"/api/claim/"+id)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var claim map[string]any
	require.NoError(t, json.Unmarshal(raw, &claim))
	require.Equal(t, "queued", claim["status"])
	require.EqualValues(t, 3, claim["broadcastNodes"])

	resp, _ = get(t, a.srv.URL+"/api/claim/"+strings.Repeat("0", 32))
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, _ = get(t, a.srv.URL+"/api/claim/..%2f..%2fetc")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestClaimAttemptLimit(t *testing.T) {
	a := newAPI(t)
	for range 10 {
		resp, _ := a.claim(t, "203.0.113.50", `{"address":"bogus"}`)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
	resp, body := a.claim(t, "203.0.113.50", `{"address":"utest1abc"}`)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "too_many_attempts", body["error"])
}

func TestClaimWhenEmpty(t *testing.T) {
	a := newAPI(t)
	a.wallet.Balance = zecd.Balances{}
	a.svc.RefreshStatus(context.Background())
	resp, body := a.claim(t, "203.0.113.7", `{"address":"utest1abc"}`)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "unavailable", body["error"])
	require.Contains(t, body["message"], "empty")

	resp, raw := get(t, a.srv.URL+"/readyz")
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Contains(t, string(raw), "faucet is empty")
}

func TestStatusAPI(t *testing.T) {
	a := newAPI(t)
	resp, raw := get(t, a.srv.URL+"/api/status")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var st map[string]any
	require.NoError(t, json.Unmarshal(raw, &st))
	require.Equal(t, true, st["ready"])
	require.Equal(t, "0.125", st["payout"])
	require.Equal(t, "0.125", st["ipLimit"])
	require.Equal(t, "5", st["balance"])
	require.EqualValues(t, 4_408_914, st["chainHeight"])
	require.EqualValues(t, 3, st["broadcastNodes"])
	require.Equal(t, "12.5", st["capRemaining"])
	require.Equal(t, "utest1faucetdonation", st["donationAddress"])
	require.Equal(t, "https://explorer.example/tx/{txid}", st["explorerTxUrl"])
	require.Equal(t, []any{}, st["recent"])
}

func TestPagesAndHeaders(t *testing.T) {
	a := newAPI(t)

	resp, raw := get(t, a.srv.URL+"/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'self'")
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	page := string(raw)
	require.Contains(t, page, "Testnet faucet")
	require.Contains(t, page, "https://valargroup.dev")
	require.NotContains(t, page, "valargroup.org")
	require.NotContains(t, page, "__ASSET_VERSION__")
	require.Contains(t, page, "/static/app.js?v=")

	for _, path := range []string{"/static/app.js", "/static/styles.css", "/static/valar-logo.png", "/static/favicon-32.png"} {
		resp, _ := get(t, a.srv.URL+path)
		require.Equal(t, http.StatusOK, resp.StatusCode, path)
		require.Equal(t, "public, max-age=86400", resp.Header.Get("Cache-Control"), path)
	}

	resp, _ = get(t, a.srv.URL+"/nope")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, raw = get(t, a.srv.URL+"/healthz")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "ok\n", string(raw))

	resp, _ = get(t, a.srv.URL+"/readyz")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
