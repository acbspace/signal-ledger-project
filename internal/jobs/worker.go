package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"signalledger/internal/domain"
	"signalledger/internal/store"
	"signalledger/internal/strategies"
)

type Extractor interface {
	ExtractClaims(context.Context, domain.Document) (domain.Extraction, error)
}

type SnapshotFetcher interface {
	FetchSnapshot(context.Context, domain.MarketDataRequest) (domain.MarketDataSnapshot, error)
}

type Backtester interface {
	RunBacktest(context.Context, domain.BacktestRequest) (domain.BacktestResult, error)
}

// ArtifactStore persists run artifacts on the shared volume. The quant service
// mounts that volume read-only, so the worker does the writing.
type ArtifactStore interface {
	Save(content []byte) (string, error)
}

type Options struct {
	Store         *store.Store
	Extractor     Extractor
	MarketData    SnapshotFetcher
	Backtest      Backtester
	Artifacts     ArtifactStore
	WorkerID      string
	PollInterval  time.Duration
	LeaseDuration time.Duration
}

// Run leases durable jobs and delegates analysis to the stateless Python service.
// A failed job is safely retried because page/claim persistence replaces the prior
// extraction in one transaction.
func Run(ctx context.Context, logger *slog.Logger, options Options) error {
	if options.Store == nil || options.Extractor == nil {
		return fmt.Errorf("worker requires a store and extractor")
	}
	if options.PollInterval <= 0 || options.LeaseDuration <= 0 {
		return fmt.Errorf("worker intervals must be positive")
	}

	logger.Info("worker started",
		"worker_id", options.WorkerID,
		"poll_interval", options.PollInterval.String(),
		"lease_duration", options.LeaseDuration.String(),
	)

	for {
		processed, err := runOne(ctx, logger, options)
		if err != nil {
			logger.Error("job loop error", "error", err)
		}
		if processed {
			continue
		}
		if !wait(ctx, options.PollInterval) {
			logger.Info("worker stopped")
			return nil
		}
	}
}

func runOne(ctx context.Context, logger *slog.Logger, options Options) (bool, error) {
	job, err := options.Store.LeaseNextJob(ctx, options.WorkerID, options.LeaseDuration)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}

	logger.Info("job leased", "job_id", job.ID, "job_type", job.Type, "attempt", job.Attempts)
	documentID, processErr := process(ctx, logger, options, *job)
	if processErr == nil {
		if err := options.Store.CompleteJob(ctx, job.ID); err != nil {
			return true, err
		}
		logger.Info("job completed", "job_id", job.ID, "job_type", job.Type)
		return true, nil
	}

	terminal, failErr := options.Store.FailJob(ctx, *job, processErr)
	if failErr != nil {
		return true, fmt.Errorf("process job %s: %w; record failure: %v", job.ID, processErr, failErr)
	}
	if terminal && documentID != "" {
		if err := options.Store.SetDocumentStatus(ctx, documentID, "failed"); err != nil {
			return true, fmt.Errorf("mark document failed after job error: %w", err)
		}
	}
	logger.Warn("job failed", "job_id", job.ID, "terminal", terminal, "error", processErr)
	return true, nil
}

func process(ctx context.Context, logger *slog.Logger, options Options, job domain.Job) (string, error) {
	switch job.Type {
	case "extract_document":
		return processExtraction(ctx, logger, options, job)
	case "fetch_market_data":
		return "", processMarketData(ctx, logger, options, job)
	case "run_backtest":
		return "", processBacktest(ctx, logger, options, job)
	default:
		return "", fmt.Errorf("unsupported job type %q", job.Type)
	}
}

func processExtraction(ctx context.Context, logger *slog.Logger, options Options, job domain.Job) (string, error) {
	var payload struct {
		DocumentID string `json:"document_id"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return "", fmt.Errorf("decode extract_document payload: %w", err)
	}
	if payload.DocumentID == "" {
		return "", fmt.Errorf("decode extract_document payload: missing document_id")
	}

	document, err := options.Store.GetDocument(ctx, payload.DocumentID)
	if err != nil {
		return payload.DocumentID, err
	}
	if err := options.Store.SetDocumentStatus(ctx, document.ID, "processing"); err != nil {
		return document.ID, err
	}

	extraction, err := options.Extractor.ExtractClaims(ctx, document)
	if err != nil {
		return document.ID, err
	}
	if err := options.Store.ReplaceExtraction(ctx, document.ID, extraction); err != nil {
		return document.ID, err
	}
	logger.Debug("document extraction persisted", "document_id", document.ID, "pages", len(extraction.Pages), "claims", len(extraction.Claims))
	return document.ID, nil
}

func processMarketData(ctx context.Context, logger *slog.Logger, options Options, job domain.Job) error {
	if options.MarketData == nil {
		return fmt.Errorf("market data fetcher is not configured")
	}

	var request domain.MarketDataRequest
	if err := json.Unmarshal(job.Payload, &request); err != nil {
		return fmt.Errorf("decode fetch_market_data payload: %w", err)
	}

	snapshot, err := options.MarketData.FetchSnapshot(ctx, request)
	if err != nil {
		return err
	}
	logger.Debug("market data snapshot persisted",
		"snapshot_id", snapshot.ID,
		"provider", snapshot.Provider,
		"symbols", snapshot.Symbols,
	)
	return nil
}

func processBacktest(ctx context.Context, logger *slog.Logger, options Options, job domain.Job) error {
	if options.Backtest == nil || options.Artifacts == nil {
		return fmt.Errorf("backtest runner is not configured")
	}

	var payload struct {
		BacktestID string `json:"backtest_id"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode run_backtest payload: %w", err)
	}
	if payload.BacktestID == "" {
		return fmt.Errorf("decode run_backtest payload: missing backtest_id")
	}

	// A backtest is only marked failed once its retry budget is exhausted, so a
	// transient error still gets the queue's retries.
	failIfTerminal := func(cause error) error {
		if job.Attempts >= job.MaxAttempts {
			if statusErr := options.Store.SetBacktestStatus(ctx, payload.BacktestID, "failed"); statusErr != nil {
				return fmt.Errorf("%w; mark backtest failed: %v", cause, statusErr)
			}
		}
		return cause
	}

	run, err := options.Store.GetBacktestRun(ctx, payload.BacktestID)
	if err != nil {
		return failIfTerminal(err)
	}
	if run.Status == "completed" {
		logger.Debug("backtest already completed; skipping", "backtest_id", run.ID)
		return nil
	}

	strategy, citations, err := options.Store.GetStrategyWithCitations(ctx, run.StrategyID)
	if err != nil {
		return failIfTerminal(err)
	}
	snapshot, err := options.Store.GetMarketDataSnapshot(ctx, run.MarketDataSnapshotID)
	if err != nil {
		return failIfTerminal(err)
	}
	if snapshot.StorageKey == nil || snapshot.Checksum == nil {
		return failIfTerminal(fmt.Errorf("snapshot %s is not ready", snapshot.ID))
	}

	// The strategy's own cited claims are its research signal. Anything effective
	// after the run's cutoff is dropped here, before the engine can see it.
	signals, err := backtestSignals(strategy.Spec, citations, run.DocumentCutoffAt)
	if err != nil {
		return failIfTerminal(err)
	}

	if err := options.Store.SetBacktestStatus(ctx, run.ID, "running"); err != nil {
		return failIfTerminal(err)
	}

	result, err := options.Backtest.RunBacktest(ctx, domain.BacktestRequest{
		BacktestID:         run.ID,
		Spec:               strategy.Spec,
		SnapshotStorageKey: *snapshot.StorageKey,
		SnapshotChecksum:   *snapshot.Checksum,
		DocumentCutoffAt:   run.DocumentCutoffAt,
		Parameters:         run.Parameters,
		Signals:            signals,
	})
	if err != nil {
		return failIfTerminal(err)
	}

	// The engine checksummed exactly these bytes, so the stored artifact always
	// hashes to the recorded result_checksum.
	artifactKey, err := options.Artifacts.Save([]byte(result.EquityCurveCSV))
	if err != nil {
		return failIfTerminal(err)
	}
	// The run and the candidate ranking it proposes land together, so a ranking
	// can never exist without the completed run that proves it.
	if err := options.Store.CompleteBacktest(ctx, run.ID, result, artifactKey); err != nil {
		return failIfTerminal(err)
	}
	logger.Debug("backtest completed",
		"backtest_id", run.ID,
		"engine_version", result.EngineVersion,
		"artifact_key", artifactKey,
		"claim_signals", len(signals),
		"candidates", len(result.Candidates.Positions),
	)
	return nil
}

// backtestSignals projects the strategy's cited claims onto the symbols its spec
// actually trades. A claim citing something outside the universe stays evidence
// for the strategy without becoming a trading signal.
func backtestSignals(rawSpec json.RawMessage, citations []domain.StoredClaim, cutoff time.Time) ([]domain.ClaimSignal, error) {
	var spec strategies.Spec
	if err := json.Unmarshal(rawSpec, &spec); err != nil {
		return nil, fmt.Errorf("decode strategy spec: %w", err)
	}
	return strategies.BuildSignals(citations, spec.Universe.Symbols, cutoff), nil
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
