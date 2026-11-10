// Command server runs the ledger gRPC service.
//
// Debits and credits are posted, and balances, entries and checkpoints are read, over gRPC. It
// must only be reachable from inside the cluster: bind it to an internal address and never route
// it through the API gateway.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	ledgerv1 "github.com/JackMahfouz/e-wallet/services/ledger/gen/ledger/v1"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/config"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/handler"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/service"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	if err := run(); err != nil {
		slog.Error("ledger stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	if cfg.Migrate {
		if err := store.Migrate(ctx, pool); err != nil {
			return err
		}
	}

	repo := store.New(pool)
	ledger := service.New(repo)

	listener, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.GRPCAddr, err)
	}

	server := grpc.NewServer(grpc.ChainUnaryInterceptor(handler.LoggingInterceptor(logger)))
	ledgerv1.RegisterLedgerServer(server, handler.New(ledger, logger))

	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus(ledgerv1.Ledger_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)

	// Checkpoints are appended in the background; they make balances cheap, not correct.
	var background sync.WaitGroup
	if cfg.CheckpointInterval > 0 {
		checkpointer := service.NewCheckpointer(repo, cfg.CheckpointGrace, time.Now, logger)

		background.Add(1)
		go func() {
			defer background.Done()
			checkpointer.Run(ctx, cfg.CheckpointInterval)
		}()
	} else {
		logger.Warn("background checkpoints are off: balances sum every entry since the last checkpoint")
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		healthServer.Shutdown()
		server.GracefulStop()
	}()

	logger.Info("ledger listening",
		"addr", listener.Addr().String(),
		"checkpoint_interval", cfg.CheckpointInterval.String(),
		"migrations", cfg.Migrate,
	)

	// Serve returns nil after GracefulStop.
	serveErr := server.Serve(listener)

	stop()
	background.Wait()

	return serveErr
}

// connect opens the connection pool and checks the database answers.
func connect(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse LEDGER_DATABASE_URL: %w", err)
	}
	if cfg.DatabasePassword != "" {
		poolConfig.ConnConfig.Password = cfg.DatabasePassword
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("reach database: %w", err)
	}

	return pool, nil
}
