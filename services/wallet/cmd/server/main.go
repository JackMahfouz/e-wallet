// Command server runs the wallet service: the REST API behind the gateway, the internal gRPC API
// for other services, and the recovery job.
//
// This is the composition root of the hexagon. It is the one place that knows which adapter
// plugs into which port: it builds the driven adapters (PostgreSQL, the ledger and auth clients,
// one-time codes, funding, the clock), hands them to the application core, and hands the core to
// the driving adapters (REST, gRPC, recovery). Nothing else imports across those lines.
//
// The gRPC port must only be reachable from inside the cluster: bind it to an internal address and
// never route it through the API gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	ledgerv1 "github.com/JackMahfouz/e-wallet/services/ledger/gen/ledger/v1"
	walletv1 "github.com/JackMahfouz/e-wallet/services/wallet/gen/wallet/v1"
	grpcapi "github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/in/grpc"
	httpapi "github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/in/http"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/in/recovery"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/authverify"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/funding"
	ledgeradapter "github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/ledger"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/otp"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/postgres"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/system"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	if err := run(); err != nil {
		slog.Error("wallet stopped", "error", err)
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
		if err := postgres.Migrate(ctx, pool); err != nil {
			return err
		}
	}

	// Driven adapters.
	ledgerConn, err := grpc.NewClient(cfg.LedgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("ledger client: %w", err)
	}
	defer ledgerConn.Close()

	authConn, err := grpc.NewClient(cfg.AuthAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("auth client: %w", err)
	}
	defer authConn.Close()

	otpVerifier, err := newOTP(cfg, logger)
	if err != nil {
		return err
	}

	core := application.New(application.Dependencies{
		Store:   postgres.New(pool),
		Ledger:  ledgeradapter.New(ledgerv1.NewLedgerClient(ledgerConn), cfg.LedgerTimeout),
		Tokens:  authverify.New(authv1.NewAuthClient(authConn), cfg.AuthTimeout),
		OTP:     otpVerifier,
		Funding: newFunding(cfg, logger),
		Clock:   system.Clock{},
		IDs:     system.IDs{},
		Logger:  logger,
	}, options(cfg))

	// Driving adapters.
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(core, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}

	grpcListener, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.GRPCAddr, err)
	}
	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(grpcapi.LoggingInterceptor(logger)))
	walletv1.RegisterWalletServer(grpcServer, grpcapi.New(core, logger))

	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus(walletv1.Wallet_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)

	var background sync.WaitGroup

	if cfg.RecoveryInterval > 0 {
		runner := recovery.New(core, cfg.RecoveryInterval, logger)

		background.Add(1)
		go func() {
			defer background.Done()
			runner.Run(ctx)
		}()
	} else {
		logger.Warn("recovery is off: operations that stop part-way will not be finished")
	}

	serveErrs := make(chan error, 2)
	go func() { serveErrs <- ignoreClosed(httpServer.Serve(httpListener)) }()
	go func() { serveErrs <- grpcServer.Serve(grpcListener) }()

	logger.Info("wallet listening",
		"http", httpListener.Addr().String(), "grpc", grpcListener.Addr().String(),
		"ledger", cfg.LedgerAddr, "auth", cfg.AuthAddr,
		"otp", cfg.OTPMode, "funding", cfg.FundingMode, "recovery_interval", cfg.RecoveryInterval.String(),
		"migrations", cfg.Migrate)

	// Run until told to stop, or until a server fails.
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serveErrs:
		stop()
	}

	logger.Info("shutting down")

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	healthServer.Shutdown()
	_ = httpServer.Shutdown(shutdown)
	grpcServer.GracefulStop()
	background.Wait()

	return serveErr
}

// options turns the configuration into the core's rules.
func options(cfg config.Config) application.Options {
	return application.Options{
		ConfirmTTL:      cfg.ConfirmTTL,
		MaxHoldTTL:      cfg.MaxHoldTTL,
		TimeoutWorkdays: cfg.TimeoutWorkdays,
		MaxAccounts:     cfg.MaxAccounts,
		FundingEnabled:  cfg.FundingMode == config.FundingSimulated,
		RetryAfter:      cfg.RecoveryMinAge,
		RecoveryBatch:   50,
	}
}

func newOTP(cfg config.Config, logger *slog.Logger) (application.OTPVerifier, error) {
	if cfg.OTPMode == config.OTPDisabled {
		logger.Warn("one-time codes are DISABLED: every code is accepted. Local development only.")

		return otp.Disabled{}, nil
	}

	logger.Warn("one-time codes are a static shared secret until libs/auth issues real ones: this is not a second factor")

	return otp.NewStatic(cfg.OTPStaticCode)
}

func newFunding(cfg config.Config, logger *slog.Logger) application.FundingGateway {
	if cfg.FundingMode == config.FundingSimulated {
		logger.Warn("funding is SIMULATED: deposits create money with nothing behind it. Never in production.")

		return funding.Simulated{}
	}

	return funding.Refusing{}
}

func ignoreClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}

// connect opens the connection pool and checks the database answers.
func connect(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse WALLET_DATABASE_URL: %w", err)
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
