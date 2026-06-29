package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/forcecage/gateway/internal/budget"
	"github.com/forcecage/gateway/internal/config"
	"github.com/forcecage/gateway/internal/providers"
	"github.com/forcecage/gateway/internal/proxy"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	policyPath := envOr("POLICY_PATH", "policy.yaml")
	addr := envOr("LISTEN_ADDR", ":8080")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379")

	// Load and index policies.
	idx, err := config.Load(policyPath)
	if err != nil {
		return fmt.Errorf("load policy: %w", err)
	}
	logger.Info("policies loaded", "agents", len(idx.AgentPolicies))

	// Connect to Redis.
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	logger.Info("redis connected", "url", redisURL)

	// Wire up components.
	tracker := budget.New(rdb)
	registry := providers.NewRegistry()
	handler := proxy.New(idx, registry, tracker, logger)

	mux := http.NewServeMux()
	mux.Handle("/proxy/", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 180 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Optional forward-proxy ingress: set FORWARD_PROXY_ADDR (e.g. :8081) to let
	// agents enforce via HTTPS_PROXY instead of a base_url override.
	if fwdAddr := os.Getenv("FORWARD_PROXY_ADDR"); fwdAddr != "" {
		if err := startForwardProxy(fwdAddr, idx, registry, tracker, logger); err != nil {
			return fmt.Errorf("forward proxy: %w", err)
		}
	}

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("gateway listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-quit
	logger.Info("shutting down")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}

// startForwardProxy boots the forward-proxy (MITM) ingress on its own listener.
// The CA is loaded from FORWARD_CA_CERT/FORWARD_CA_KEY when provided; otherwise a
// self-signed CA is generated and written to FORWARD_CA_OUT (default ./forcecage-ca.pem)
// for installation into the agent's trust store.
func startForwardProxy(addr string, idx *config.Index, registry *providers.Registry, tracker *budget.Tracker, logger *slog.Logger) error {
	var ca *proxy.CertAuthority
	certPath, keyPath := os.Getenv("FORWARD_CA_CERT"), os.Getenv("FORWARD_CA_KEY")
	if certPath != "" && keyPath != "" {
		loaded, err := proxy.LoadCertAuthority(certPath, keyPath)
		if err != nil {
			return err
		}
		ca = loaded
		logger.Info("forward proxy CA loaded", "cert", certPath)
	} else {
		generated, certPEM, _, err := proxy.GenerateCertAuthority("ForceCage Forward Proxy CA")
		if err != nil {
			return err
		}
		ca = generated
		out := envOr("FORWARD_CA_OUT", "forcecage-ca.pem")
		if err := os.WriteFile(out, certPEM, 0o600); err != nil {
			return err
		}
		logger.Warn("forward proxy generated an ephemeral CA; install it in the agent trust store", "ca_cert", out)
	}

	engine := proxy.NewEngine(idx, registry, tracker, logger)
	fp := proxy.NewForwardProxy(engine, registry, ca, logger)
	go func() {
		logger.Info("forward proxy listening", "addr", addr)
		if err := http.ListenAndServe(addr, fp); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("forward proxy error", "err", err)
		}
	}()
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
