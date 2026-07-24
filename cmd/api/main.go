package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"signalledger/internal/config"
	"signalledger/internal/documents"
	"signalledger/internal/httpapi"
	"signalledger/internal/marketdata"
	"signalledger/internal/quant"
	"signalledger/internal/store"
	"signalledger/internal/strategies"
)

var version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(
		"service", "api",
		"version", version,
	)

	database, err := store.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Error("connect to database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	documentService := documents.NewService(
		database,
		documents.NewFileStore(cfg.DocumentStoragePath, cfg.MaxUploadBytes),
	)
	strategyService := strategies.NewService(database)
	marketDataService := marketdata.NewService(
		database,
		quant.NewClient(cfg.QuantServiceURL, cfg.QuantTimeout),
		marketdata.NewSnapshotStore(cfg.DocumentStoragePath),
	)

	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewHandler(httpapi.Options{
			Version:        version,
			Documents:      documentService,
			Claims:         database,
			Strategies:     strategyService,
			MarketData:     marketDataService,
			Backtests:      database,
			Candidates:     database,
			MaxUploadBytes: cfg.MaxUploadBytes,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("http server started", "address", cfg.HTTPAddr)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
}
