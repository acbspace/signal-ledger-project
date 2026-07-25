package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"signalledger/internal/domain"
)

// CreateBacktestWithRunJob inserts a queued backtest run and its run job in one
// transaction, mirroring the document and market-data patterns. The worker
// completes the run once the stateless quant service returns.
func (store *Store) CreateBacktestWithRunJob(ctx context.Context, input domain.CreateBacktestInput) (domain.BacktestRun, domain.Job, error) {
	if err := input.Validate(); err != nil {
		return domain.BacktestRun{}, domain.Job{}, fmt.Errorf("validate backtest input: %w", err)
	}
	parameters := input.Parameters
	if len(parameters) == 0 {
		parameters = json.RawMessage(`{}`)
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.BacktestRun{}, domain.Job{}, fmt.Errorf("begin backtest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var run domain.BacktestRun
	var rawParams, rawSummary string
	err = tx.QueryRow(ctx, `
		INSERT INTO backtest_runs (strategy_id, market_data_snapshot_id, document_cutoff_at, status, parameters)
		VALUES ($1, $2, $3, 'queued', $4::jsonb)
		RETURNING id::text, strategy_id::text, market_data_snapshot_id::text,
			document_cutoff_at, status, parameters::text, summary::text, created_at`,
		input.StrategyID, input.SnapshotID, input.DocumentCutoffAt, string(parameters),
	).Scan(&run.ID, &run.StrategyID, &run.MarketDataSnapshotID, &run.DocumentCutoffAt,
		&run.Status, &rawParams, &rawSummary, &run.CreatedAt)
	if err != nil {
		return domain.BacktestRun{}, domain.Job{}, fmt.Errorf("insert backtest run: %w", err)
	}
	run.Parameters = json.RawMessage(rawParams)
	run.Summary = json.RawMessage(rawSummary)

	payload, err := json.Marshal(map[string]string{"backtest_id": run.ID})
	if err != nil {
		return domain.BacktestRun{}, domain.Job{}, fmt.Errorf("encode run job payload: %w", err)
	}
	job, err := insertJob(ctx, tx, "run_backtest", payload)
	if err != nil {
		return domain.BacktestRun{}, domain.Job{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.BacktestRun{}, domain.Job{}, fmt.Errorf("commit backtest transaction: %w", err)
	}
	return run, *job, nil
}

func (store *Store) GetBacktestRun(ctx context.Context, id string) (domain.BacktestRun, error) {
	var run domain.BacktestRun
	var rawParams, rawSummary string
	var snapshotID, engineVersion, artifactKey, checksum pgtype.Text
	var completedAt pgtype.Timestamptz
	err := store.pool.QueryRow(ctx, `
		SELECT id::text, strategy_id::text, market_data_snapshot_id::text,
			document_cutoff_at, status, parameters::text, summary::text,
			engine_version, result_artifact_key, result_checksum, created_at, completed_at
		FROM backtest_runs
		WHERE id = $1`, id).Scan(
		&run.ID, &run.StrategyID, &snapshotID, &run.DocumentCutoffAt, &run.Status,
		&rawParams, &rawSummary, &engineVersion, &artifactKey, &checksum, &run.CreatedAt, &completedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.BacktestRun{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.BacktestRun{}, fmt.Errorf("get backtest run: %w", err)
	}
	run.Parameters = json.RawMessage(rawParams)
	run.Summary = json.RawMessage(rawSummary)
	if snapshotID.Valid {
		run.MarketDataSnapshotID = snapshotID.String
	}
	if engineVersion.Valid {
		value := engineVersion.String
		run.EngineVersion = &value
	}
	if artifactKey.Valid {
		value := artifactKey.String
		run.ResultArtifactKey = &value
	}
	if checksum.Valid {
		value := checksum.String
		run.ResultChecksum = &value
	}
	if completedAt.Valid {
		value := completedAt.Time
		run.CompletedAt = &value
	}
	return run, nil
}

func (store *Store) SetBacktestStatus(ctx context.Context, id, status string) error {
	switch status {
	case "queued", "running", "completed", "failed":
	default:
		return fmt.Errorf("invalid backtest status %q", status)
	}
	command, err := store.pool.Exec(ctx, `UPDATE backtest_runs SET status = $2 WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("set backtest status: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

// CompleteBacktest records the summary, artifact pointer, engine version, and
// content checksum, writes the run's candidate ranking, and marks the run
// completed — all in one transaction, so a ranking can never exist without the
// run that proves it. It only advances an in-progress run, so a retried job
// cannot overwrite a finished one.
func (store *Store) CompleteBacktest(ctx context.Context, id string, result domain.BacktestResult, artifactKey string) error {
	if err := result.Candidates.Validate(); err != nil {
		return fmt.Errorf("complete backtest: %w", err)
	}
	summary := result.Summary
	if len(summary) == 0 {
		summary = json.RawMessage(`{}`)
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin complete backtest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var strategyID string
	err = tx.QueryRow(ctx, `
		UPDATE backtest_runs
		SET status = 'completed', summary = $2::jsonb, result_artifact_key = $3,
			result_checksum = $4, engine_version = $5, completed_at = now()
		WHERE id = $1 AND status IN ('queued', 'running')
		RETURNING strategy_id::text`,
		id, string(summary), artifactKey, result.ResultChecksum, result.EngineVersion,
	).Scan(&strategyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("complete backtest: run is not in progress")
	}
	if err != nil {
		return fmt.Errorf("complete backtest: %w", err)
	}

	if err := insertCandidates(ctx, tx, id, strategyID, result.Candidates); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit complete backtest: %w", err)
	}
	return nil
}

func insertCandidates(ctx context.Context, tx pgx.Tx, runID, strategyID string, candidates domain.CandidateSet) error {
	// A retried job that got as far as inserting must not collide with itself.
	if _, err := tx.Exec(ctx, `DELETE FROM portfolio_candidates WHERE backtest_run_id = $1`, runID); err != nil {
		return fmt.Errorf("clear previous candidates: %w", err)
	}

	for _, position := range candidates.Positions {
		var candidateID string
		err := tx.QueryRow(ctx, `
			INSERT INTO portfolio_candidates
				(backtest_run_id, strategy_id, symbol, rank, weight, score, momentum, momentum_rank, claim_support, as_of)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING id::text`,
			runID, strategyID, position.Symbol, position.Rank, position.Weight,
			position.Score, position.Momentum, position.MomentumRank, position.ClaimSupport, candidates.AsOf,
		).Scan(&candidateID)
		if err != nil {
			return fmt.Errorf("insert candidate %s: %w", position.Symbol, err)
		}

		for _, evidence := range position.Evidence {
			if _, err := tx.Exec(ctx, `
				INSERT INTO candidate_claims (candidate_id, claim_id, contribution)
				VALUES ($1, $2::uuid, $3)
				ON CONFLICT (candidate_id, claim_id) DO NOTHING`,
				candidateID, evidence.ClaimID, evidence.Contribution,
			); err != nil {
				return fmt.Errorf("attribute candidate %s to claim %s: %w", position.Symbol, evidence.ClaimID, err)
			}
		}
	}
	return nil
}
