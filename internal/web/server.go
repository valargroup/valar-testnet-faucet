// Package web serves the faucet's page and JSON API.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/valargroup/valar-testnet-faucet/internal/faucet"
	"github.com/valargroup/valar-testnet-faucet/internal/store"
	"github.com/valargroup/valar-testnet-faucet/internal/zat"
)

//go:embed static
var staticFiles embed.FS

// Config holds presentation and proxy settings.
type Config struct {
	// TrustProxy honours X-Real-IP from a loopback peer (Caddy on the same host). The
	// header is ignored from any other peer, so it cannot be spoofed from outside.
	TrustProxy bool
	// ExplorerTxURL is a transaction link template containing "{txid}"; empty disables links.
	ExplorerTxURL string
	// StatusPageURL links the Zakura node status page.
	StatusPageURL string
}

// Server is the HTTP handler.
type Server struct {
	cfg      Config
	svc      *faucet.Service
	log      *slog.Logger
	attempts *attemptLimiter
	mux      *http.ServeMux
	index    []byte
}

// Claim attempts (valid or not) allowed per IP key per window, independent of payouts;
// this bounds how hard one client can drive zecd address validation.
const (
	attemptLimit  = 10
	attemptWindow = 10 * time.Minute
	maxBodyBytes  = 4 << 10
)

// New returns the faucet's HTTP handler.
func New(cfg Config, svc *faucet.Service, log *slog.Logger) http.Handler {
	s := &Server{
		cfg:      cfg,
		svc:      svc,
		log:      log,
		attempts: newAttemptLimiter(attemptLimit, attemptWindow),
		mux:      http.NewServeMux(),
		index:    renderIndex(),
	}
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // the embed path is a compile-time constant
	}
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	// Asset URLs carry a content hash (see renderIndex), so they can be cached for long.
	s.mux.Handle("GET /static/", cacheFor(24*time.Hour, http.StripPrefix("/static/", http.FileServerFS(static))))
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("POST /api/claim", s.handleClaim)
	s.mux.HandleFunc("GET /api/claim/{id}", s.handleClaimStatus)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	return securityHeaders(s.mux)
}

// renderIndex returns index.html with __ASSET_VERSION__ replaced by a hash of the
// embedded assets, so each deploy busts browser caches of the CSS and JS.
func renderIndex() []byte {
	page, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		panic(err) // embedded at compile time
	}
	h := sha256.New()
	_ = fs.WalkDir(staticFiles, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFiles.ReadFile(path)
		if err != nil {
			return err
		}
		h.Write(b)
		return nil
	})
	version := hex.EncodeToString(h.Sum(nil))[:12]
	return bytes.ReplaceAll(page, []byte("__ASSET_VERSION__"), []byte(version))
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(s.index)
}

type recentPayout struct {
	Address string `json:"address"`
	TxID    string `json:"txid"`
	Amount  string `json:"amount"`
	Time    int64  `json:"time"`
}

type statusResponse struct {
	Network                string         `json:"network"`
	Ready                  bool           `json:"ready"`
	Reason                 string         `json:"reason,omitempty"`
	Payout                 string         `json:"payout"`
	IPLimit                string         `json:"ipLimit"`
	WindowSeconds          int64          `json:"windowSeconds"`
	AddressCooldownSeconds int64          `json:"addressCooldownSeconds"`
	Balance                string         `json:"balance"`
	PendingBalance         string         `json:"pendingBalance"`
	WalletSynced           bool           `json:"walletSynced"`
	ChainHeight            int64          `json:"chainHeight"`
	PaidCount              int            `json:"paidCount"`
	PaidAmount             string         `json:"paidAmount"`
	CapRemaining           string         `json:"capRemaining"`
	QueueDepth             int            `json:"queueDepth"`
	BroadcastNodes         int            `json:"broadcastNodes"`
	DonationAddress        string         `json:"donationAddress,omitempty"`
	ExplorerTxURL          string         `json:"explorerTxUrl,omitempty"`
	StatusPageURL          string         `json:"statusPageUrl,omitempty"`
	Recent                 []recentPayout `json:"recent"`
	UpdatedAt              int64          `json:"updatedAt"`
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	st := s.svc.Status()
	lim := s.svc.Config().Limits
	remaining := max(lim.GlobalWindowCapZat-st.Stats.CommittedZat, 0)
	resp := statusResponse{
		Network:                "testnet",
		Ready:                  st.Ready,
		Reason:                 st.Reason,
		Payout:                 zat.Display(lim.PayoutZat),
		IPLimit:                zat.Display(lim.IPWindowLimitZat),
		WindowSeconds:          int64(lim.Window / time.Second),
		AddressCooldownSeconds: int64(lim.AddressCooldown / time.Second),
		Balance:                zat.Display(st.Balance.Trusted),
		PendingBalance:         zat.Display(st.Balance.UntrustedPending),
		WalletSynced:           st.WalletSynced,
		ChainHeight:            st.ChainHeight,
		PaidCount:              st.Stats.SentCount,
		PaidAmount:             zat.Display(st.Stats.SentZat),
		CapRemaining:           zat.Display(remaining),
		QueueDepth:             st.Stats.Pending,
		BroadcastNodes:         s.svc.BroadcastNodes(),
		DonationAddress:        st.DonationAddress,
		ExplorerTxURL:          s.cfg.ExplorerTxURL,
		StatusPageURL:          s.cfg.StatusPageURL,
		Recent:                 []recentPayout{},
		UpdatedAt:              st.UpdatedAt.Unix(),
	}
	for _, c := range st.Recent {
		resp.Recent = append(resp.Recent, recentPayout{
			Address: maskAddress(c.Address), TxID: c.TxID, Amount: zat.Display(c.AmountZat), Time: c.UpdatedAt.Unix(),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

type claimRequest struct {
	Address string `json:"address"`
}

type claimResponse struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Amount         string `json:"amount"`
	TxID           string `json:"txid,omitempty"`
	Message        string `json:"message,omitempty"`
	BroadcastOK    int    `json:"broadcastOk"`
	BroadcastNodes int    `json:"broadcastNodes"`
	CreatedAt      int64  `json:"createdAt"`
}

func (s *Server) claimJSON(c store.Claim) claimResponse {
	return claimResponse{
		ID: c.ID, Status: string(c.Status), Amount: zat.Display(c.AmountZat), TxID: c.TxID,
		Message: c.Error, BroadcastOK: c.BroadcastOK, BroadcastNodes: s.svc.BroadcastNodes(),
		CreatedAt: c.CreatedAt.Unix(),
	}
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	addr, err := clientAddr(r, s.cfg.TrustProxy)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_client", "Could not determine your IP address.", 0)
		return
	}
	ipKey, err := faucet.IPKey(addr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_client", "Could not determine your IP address.", 0)
		return
	}
	if ok, retry := s.attempts.allow(ipKey); !ok {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts",
			"Too many attempts from your network. Please wait a few minutes.", retry)
		return
	}

	var req claimRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Send a JSON body like {\"address\": \"utest1…\"}.", 0)
		return
	}

	c, err := s.svc.Claim(r.Context(), ipKey, req.Address)
	var limitErr *store.LimitError
	var unavailable *faucet.UnavailableError
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, s.claimJSON(c))
	case errors.Is(err, faucet.ErrInvalidAddress):
		writeError(w, http.StatusBadRequest, "invalid_address",
			"That is not a valid Zcash testnet address. Use a utest1…, ztestsapling… or tm… address.", 0)
	case errors.Is(err, faucet.ErrOwnAddress):
		writeError(w, http.StatusBadRequest, "own_address", "That address belongs to the faucet itself.", 0)
	case errors.As(err, &limitErr):
		writeError(w, http.StatusTooManyRequests, string(limitErr.Reason), limitMessage(limitErr.Reason, s.svc.Config().Limits), limitErr.RetryAfter)
	case errors.As(err, &unavailable):
		writeError(w, http.StatusServiceUnavailable, "unavailable", unavailableMessage(unavailable.Reason), 0)
	default:
		s.log.Error("claim", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "Something went wrong. Please try again.", 0)
	}
}

var claimIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Server) handleClaimStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !claimIDPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "No such claim.", 0)
		return
	}
	c, err := s.svc.Lookup(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such claim.", 0)
	case err != nil:
		s.log.Error("claim lookup", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "Something went wrong. Please try again.", 0)
	default:
		writeJSON(w, http.StatusOK, s.claimJSON(c))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	st := s.svc.Status()
	if !st.Ready {
		http.Error(w, st.Reason, http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

// clientAddr returns the requesting client's IP: the TCP peer, or X-Real-IP when the peer
// is a trusted loopback proxy.
func clientAddr(r *http.Request, trustProxy bool) (netip.Addr, error) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("remote addr %q: %w", r.RemoteAddr, err)
	}
	if trustProxy && peer.Addr().Unmap().IsLoopback() {
		if h := strings.TrimSpace(r.Header.Get("X-Real-IP")); h != "" {
			return netip.ParseAddr(h)
		}
	}
	return peer.Addr(), nil
}

func limitMessage(reason store.Reason, lim store.Limits) string {
	switch reason {
	case store.ReasonIPLimit:
		return fmt.Sprintf("Your network already received %s TAZ in the last 24 hours.", zat.Display(lim.IPWindowLimitZat))
	case store.ReasonAddressCooldown:
		return "This address was paid recently."
	case store.ReasonGlobalCap:
		return "The faucet has reached its daily payout cap."
	case store.ReasonQueueFull:
		return "The faucet is busy right now."
	default:
		return "Limit reached."
	}
}

func unavailableMessage(reason string) string {
	switch reason {
	case "faucet is empty":
		return "The faucet is empty right now. Please check back later."
	case "wallet syncing":
		return "The faucet wallet is syncing with the chain. Please try again in a few minutes."
	default:
		return "The faucet is temporarily unavailable. Please try again shortly."
	}
}

// maskAddress shortens an address for public display.
func maskAddress(a string) string {
	if len(a) <= 18 {
		return a
	}
	return a[:10] + "…" + a[len(a)-6:]
}

type errorResponse struct {
	Error             string `json:"error"`
	Message           string `json:"message"`
	RetryAfterSeconds int64  `json:"retryAfterSeconds,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string, retry time.Duration) {
	resp := errorResponse{Error: code, Message: msg}
	if retry > 0 {
		secs := int64((retry + time.Second - 1) / time.Second)
		resp.RetryAfterSeconds = secs
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

const contentSecurityPolicy = "default-src 'self'; " +
	"style-src 'self' https://fonts.googleapis.com; " +
	"font-src https://fonts.gstatic.com; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func cacheFor(d time.Duration, next http.Handler) http.Handler {
	v := "public, max-age=" + strconv.Itoa(int(d/time.Second))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", v)
		next.ServeHTTP(w, r)
	})
}
