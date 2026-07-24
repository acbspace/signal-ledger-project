package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// BacktestRun records one deterministic simulation. Reproducibility inputs are
// pinned: an immutable strategy version, a frozen market-data snapshot, the
// document cutoff used to prevent look-ahead, and the engine version.
type BacktestRun struct {
	ID                   string
	StrategyID           string
	MarketDataSnapshotID string
	DocumentCutoffAt     time.Time
	Status               string
	Parameters           json.RawMessage
	Summary              json.RawMessage
	EngineVersion        *string
	ResultArtifactKey    *string
	ResultChecksum       *string
	CreatedAt            time.Time
	CompletedAt          *time.Time
}

type CreateBacktestInput struct {
	StrategyID       string
	SnapshotID       string
	DocumentCutoffAt time.Time
	Parameters       json.RawMessage
}

func (input CreateBacktestInput) Validate() error {
	if input.StrategyID == "" {
		return fmt.Errorf("strategy_id is required")
	}
	if input.SnapshotID == "" {
		return fmt.Errorf("market_data_snapshot_id is required")
	}
	if input.DocumentCutoffAt.IsZero() {
		return fmt.Errorf("document_cutoff_at is required")
	}
	if len(input.Parameters) > 0 && !json.Valid(input.Parameters) {
		return fmt.Errorf("parameters must be valid JSON")
	}
	return nil
}

// ClaimSignal is one accepted research claim projected onto a tradable symbol.
// EffectiveAt is the point-in-time stamp the engine must respect: a claim may
// only influence a rebalance on or after that day, and only until its horizon
// expires. HorizonDays is nil when the claim did not state one, in which case
// the engine applies the spec's default horizon.
type ClaimSignal struct {
	ClaimID     string
	Symbol      string
	Direction   string
	Confidence  float64
	EffectiveAt time.Time
	HorizonDays *int
}

// BacktestRequest is the self-contained payload the worker sends to the stateless
// quant service: the immutable spec JSON, the frozen snapshot to read by
// storage_key, its checksum to verify, the look-ahead cutoff, and the accepted
// research claims that may tilt selection.
type BacktestRequest struct {
	BacktestID         string
	Spec               json.RawMessage
	SnapshotStorageKey string
	SnapshotChecksum   string
	DocumentCutoffAt   time.Time
	Parameters         json.RawMessage
	Signals            []ClaimSignal
}

// BacktestResult is what the stateless quant service returns for Go to persist.
// EquityCurveCSV holds the exact bytes ResultChecksum was taken over, so the
// stored artifact always hashes to the recorded checksum. Candidates are a
// projection of the same simulation — the last rebalance — and are stored in
// Postgres rather than in the checksummed artifact.
type BacktestResult struct {
	Summary        json.RawMessage
	EquityCurveCSV string
	ResultChecksum string
	EngineVersion  string
	Candidates     CandidateSet
}
