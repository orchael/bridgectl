package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/orchael/bridgectl/internal/registry"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "config/registry.yaml", "Path to registry config file")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("bridge-key-registry %s\n", version)
		os.Exit(0)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := registry.LoadConfig(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Initialize store
	var store registry.Store
	switch cfg.Storage.Type {
	case "memory":
		store = registry.NewMemoryStore()
		logger.Info("using in-memory store")
	case "file":
		store, err = registry.NewK8sStore(cfg.Storage.BaseDir, logger)
		if err != nil {
			logger.Error("failed to init file store", "error", err)
			os.Exit(1)
		}
		logger.Info("using file-backed store", "dir", cfg.Storage.BaseDir)
	default:
		logger.Error("unknown storage type", "type", cfg.Storage.Type)
		os.Exit(1)
	}

	// Build handler
	serverConfig := cfg.BuildServerConfig()
	handler := registry.NewHandler(store, serverConfig, logger)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Build TLS config for mTLS
	tlsConfig, err := buildTLSConfig(cfg.TLS)
	if err != nil {
		logger.Error("failed to configure TLS", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:         cfg.Listen,
		Handler:      mux,
		TLSConfig:    tlsConfig,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("starting bridge-key-registry", "addr", cfg.Listen, "version", version)
		// TLS cert/key are already loaded in TLSConfig, so pass empty strings
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
	}
}

func buildTLSConfig(cfg registry.TLSConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.Cert, cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}

	caPEM, err := os.ReadFile(cfg.CABundle)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certs in CA bundle")
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}, nil
}
