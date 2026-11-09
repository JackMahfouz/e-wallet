// Command server runs the internal Auth gRPC service.
//
// It must only be reachable from inside the cluster: bind it to an internal address and
// never route it through the API gateway.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	"github.com/JackMahfouz/e-wallet/libs/auth/internal/config"
	"github.com/JackMahfouz/e-wallet/libs/auth/internal/handler"
	"github.com/JackMahfouz/e-wallet/libs/auth/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	if err := run(); err != nil {
		slog.Error("auth server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	listener, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.GRPCAddr, err)
	}

	tokens := service.NewTokenService(cfg.PrivateKey, cfg.Issuer, cfg.AccessTTL, time.Now)

	server := grpc.NewServer()
	authv1.RegisterAuthServer(server, handler.NewAuthHandler(tokens))

	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus(authv1.Auth_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		healthServer.Shutdown()
		server.GracefulStop()
	}()

	slog.Info("auth server listening", "addr", listener.Addr().String(), "issuer", cfg.Issuer, "access_ttl", cfg.AccessTTL.String())

	// Serve returns nil after GracefulStop.
	return server.Serve(listener)
}
