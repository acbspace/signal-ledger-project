package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"signalledger/internal/backtests"
	"signalledger/internal/config"
	"signalledger/internal/jobs"
	"signalledger/internal/marketdata"
	"signalledger/internal/quant"
	"signalledger/internal/store"
)

var version = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(
		"service", "worker",
		"version", version,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("connect to database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	quantClient := quant.NewClient(cfg.QuantServiceURL, cfg.QuantTimeout)
	marketDataService := marketdata.NewService(
		database,
		quantClient,
		marketdata.NewSnapshotStore(cfg.DocumentStoragePath),
	)
	if err := jobs.Run(ctx, logger, jobs.Options{
		Store:         database,
		Extractor:     quantClient,
		MarketData:    marketDataService,
		Backtest:      quantClient,
		Artifacts:     backtests.NewArtifactStore(cfg.DocumentStoragePath),
		WorkerID:      cfg.WorkerID,
		PollInterval:  cfg.WorkerPollInterval,
		LeaseDuration: cfg.JobLeaseDuration,
	}); err != nil {
		logger.Error("worker stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}
