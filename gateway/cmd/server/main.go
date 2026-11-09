// Command server runs the API gateway.
//
//	server -config gateway.yaml
//
// The config path can also come from GATEWAY_CONFIG. See gateway.yaml for every setting.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/JackMahfouz/e-wallet/gateway/internal/authfilter"
	"github.com/JackMahfouz/e-wallet/gateway/internal/config"
	"github.com/JackMahfouz/e-wallet/gateway/internal/proxy"
	"github.com/JackMahfouz/e-wallet/gateway/internal/ratelimit"
	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", envOr("GATEWAY_CONFIG", "gateway.yaml"), "path to the YAML configuration")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// The auth service listens on an internal port only; the mesh sidecar provides mTLS.
	authConn, err := grpc.NewClient(cfg.Auth.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect to auth service: %w", err)
	}
	defer authConn.Close()
	verifier := authfilter.NewGRPC(authv1.NewAuthClient(authConn), cfg.Auth.Timeout)

	limiter, closeLimiter, err := buildLimiter(cfg, logger)
	if err != nil {
		return err
	}
	defer closeLimiter()

	handler, err := proxy.New(proxy.Options{Config: cfg, Verifier: verifier, Limiter: limiter, Logger: logger})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	logger.Info("gateway listening", "addr", cfg.Server.Listen, "config", *configPath, "routes", len(cfg.Routes))

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// buildLimiter returns the rate limiter and a function that releases its resources. With a
// redis block it is Redis backed by the in-memory limiter for outages; without one, in-memory only.
func buildLimiter(cfg *config.Config, logger *slog.Logger) (ratelimit.Limiter, func(), error) {
	memory := ratelimit.NewMemory(time.Now)

	if cfg.Redis == nil {
		logger.Warn("no redis configured: rate limits are in-memory and per gateway instance")

		return memory, func() {}, nil
	}

	password, err := readPassword(cfg.Redis.PasswordFile)
	if err != nil {
		return nil, nil, err
	}

	client := ratelimit.NewRedisClient(ratelimit.ClientConfig{
		Addr:     cfg.Redis.Addr,
		DB:       cfg.Redis.DB,
		Password: password,
		TLS:      cfg.Redis.TLS,
		Timeout:  cfg.Redis.Timeout,
	})

	// Not fatal: if Redis is down at startup the failover limiter uses memory until it is back.
	pingCtx, cancel := context.WithTimeout(context.Background(), cfg.Redis.Timeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		logger.Warn("redis unreachable at startup, using the in-memory limiter until it is", "addr", cfg.Redis.Addr, "error", err)
	}

	redisLimiter := ratelimit.NewRedis(client, cfg.Redis.KeyPrefix, cfg.Redis.Timeout)
	limiter := ratelimit.NewFailover(redisLimiter, memory, cfg.Redis.RetryInterval, time.Now, logger)

	return limiter, func() { _ = client.Close() }, nil
}

// readPassword reads the Redis password from a file, so it never sits in the YAML or the
// environment. An empty path means no password.
func readPassword(file string) (string, error) {
	if file == "" {
		return "", nil
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read redis password file: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
