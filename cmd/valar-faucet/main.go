// Command valar-faucet serves the Valar Group Zcash testnet faucet.
//
// Configuration comes from FAUCET_* environment variables (see deploy/faucet.env). The
// zecd RPC password is read from a file, by default the systemd credential
// $CREDENTIALS_DIRECTORY/zecd-rpc-password, so it never appears in the environment.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/valargroup/valar-testnet-faucet/internal/broadcast"
	"github.com/valargroup/valar-testnet-faucet/internal/faucet"
	"github.com/valargroup/valar-testnet-faucet/internal/store"
	"github.com/valargroup/valar-testnet-faucet/internal/web"
	"github.com/valargroup/valar-testnet-faucet/internal/zecd"
)

// version is set at build time with -ldflags "-X main.version=<git sha>".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type config struct {
	listen        string
	dbPath        string
	zecdURL       string
	zecdUser      string
	zecdPassword  string
	limits        store.Limits
	queueTTL      time.Duration
	nodes         []broadcast.Node
	trustProxy    bool
	explorerTxURL string
	statusPageURL string
}

func loadConfig() (config, error) {
	var c config
	var errs []error
	env := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok {
			return strings.TrimSpace(v)
		}
		return def
	}
	intEnv := func(key string, def int64) int64 {
		v, err := strconv.ParseInt(env(key, strconv.FormatInt(def, 10)), 10, 64)
		if err != nil || v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive integer", key))
		}
		return v
	}
	durEnv := func(key string, def time.Duration) time.Duration {
		v, err := time.ParseDuration(env(key, def.String()))
		if err != nil || v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive duration", key))
		}
		return v
	}

	c.listen = env("FAUCET_LISTEN", "127.0.0.1:8093")
	c.dbPath = env("FAUCET_DB", "faucet.db")
	c.zecdURL = env("FAUCET_ZECD_URL", "http://127.0.0.1:18890")
	c.zecdUser = env("FAUCET_ZECD_USER", "faucet")
	c.limits = store.Limits{
		PayoutZat:          intEnv("FAUCET_PAYOUT_ZAT", 12_500_000),
		IPWindowLimitZat:   intEnv("FAUCET_IP_LIMIT_ZAT", 12_500_000),
		GlobalWindowCapZat: intEnv("FAUCET_GLOBAL_CAP_ZAT", 1_250_000_000),
		MaxQueue:           int(intEnv("FAUCET_MAX_QUEUE", 20)),
		Window:             durEnv("FAUCET_WINDOW", 24*time.Hour),
		AddressCooldown:    durEnv("FAUCET_ADDRESS_COOLDOWN", 24*time.Hour),
	}
	c.queueTTL = durEnv("FAUCET_QUEUE_TTL", 30*time.Minute)
	c.explorerTxURL = env("FAUCET_EXPLORER_TX_URL", "")
	c.statusPageURL = env("FAUCET_STATUS_PAGE_URL", "https://status.testnet.zakura.valargroup.dev/")
	c.trustProxy = env("FAUCET_TRUST_PROXY", "true") == "true"

	nodes, err := broadcast.ParseNodes(env("FAUCET_BROADCAST_NODES", ""))
	if err != nil {
		errs = append(errs, err)
	}
	c.nodes = nodes

	pwFile := env("FAUCET_ZECD_PASSWORD_FILE", "")
	if pwFile == "" {
		if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
			pwFile = filepath.Join(dir, "zecd-rpc-password")
		}
	}
	if pwFile == "" {
		errs = append(errs, errors.New("set FAUCET_ZECD_PASSWORD_FILE or run under systemd with LoadCredential=zecd-rpc-password"))
	} else if pw, err := os.ReadFile(pwFile); err != nil {
		errs = append(errs, fmt.Errorf("read zecd RPC password file: %w", err))
	} else {
		c.zecdPassword = strings.TrimSpace(string(pw))
	}
	return c, errors.Join(errs...)
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	wallet := zecd.New(cfg.zecdURL, cfg.zecdUser, cfg.zecdPassword)
	fan := broadcast.New(cfg.nodes, 10*time.Second)
	svc := faucet.New(faucet.DefaultConfig(cfg.limits), st, wallet, fan, log)

	srv := &http.Server{
		Addr: cfg.listen,
		Handler: web.New(web.Config{
			TrustProxy:    cfg.trustProxy,
			ExplorerTxURL: cfg.explorerTxURL,
			StatusPageURL: cfg.statusPageURL,
		}, svc, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	svcDone := make(chan struct{})
	go func() {
		defer close(svcDone)
		if err := svc.Run(ctx); err != nil {
			log.Error("faucet service stopped", "err", err)
			stop()
		}
	}()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe() }()

	log.Info("faucet started", "version", version, "listen", cfg.listen,
		"payout_zat", cfg.limits.PayoutZat, "ip_limit_zat", cfg.limits.IPWindowLimitZat,
		"broadcast_nodes", len(cfg.nodes))

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-srvErr:
		runErr = fmt.Errorf("http server: %w", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	// The worker finishes an in-flight send (bounded by zecd's send timeout) first.
	<-svcDone
	log.Info("faucet stopped")
	return runErr
}
