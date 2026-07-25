package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"signalledger/internal/domain"
	"signalledger/internal/strategies"
)

const maxBacktestBodyBytes int64 = 32 << 10

type BacktestStore interface {
	CreateBacktestWithRunJob(context.Context, domain.CreateBacktestInput) (domain.BacktestRun, domain.Job, error)
	GetBacktestRun(context.Context, string) (domain.BacktestRun, error)
}

func (server server) backtests(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if server.options.Backtests == nil || server.options.Strategies == nil || server.options.MarketData == nil {
		writeError(writer, http.StatusServiceUnavailable, "backtest_service_unavailable", "backtest service is not configured")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxBacktestBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var body backtestRequestBody
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "request body must be a valid backtest request")
		return
	}

	if !validUUID(body.StrategyID) {
		writeError(writer, http.StatusBadRequest, "invalid_strategy_id", "strategy_id must be a UUID")
		return
	}
	if !validUUID(body.SnapshotID) {
		writeError(writer, http.StatusBadRequest, "invalid_snapshot_id", "market_data_snapshot_id must be a UUID")
		return
	}
	cutoff, err := time.Parse(time.RFC3339, strings.TrimSpace(body.DocumentCutoffAt))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_cutoff", "document_cutoff_at must use RFC3339 format")
		return
	}

	strategy, _, err := server.options.Strategies.Get(request.Context(), body.StrategyID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(writer, http.StatusNotFound, "strategy_not_found", "strategy was not found")
			return
		}
		writeError(writer, http.StatusInternalServerError, "strategy_lookup_failed", "could not load strategy")
		return
	}

	snapshot, err := server.options.MarketData.Get(request.Context(), body.SnapshotID)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "snapshot_not_found", "market-data snapshot was not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "snapshot_lookup_failed", "could not load snapshot")
		return
	}
	if snapshot.StorageKey == nil || snapshot.Checksum == nil {
		writeError(writer, http.StatusConflict, "snapshot_not_ready", "market-data snapshot is not ready yet")
		return
	}

	// A snapshot is a shared resource, so pairing one with a strategy it cannot
	// price is an easy mistake. The engine is the authoritative check — it sees the
	// frozen CSV's actual columns, while this row records only the symbols that
	// were *requested* — but failing here turns a job that dies minutes later in
	// the worker into an answerable error naming the symbols to fetch.
	universe, err := strategies.UniverseSymbols(strategy.Spec)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "strategy_spec_unreadable", "could not read the strategy spec")
		return
	}
	if missing := strategies.MissingSymbols(universe, snapshot.Symbols); len(missing) > 0 {
		writeError(writer, http.StatusConflict, "snapshot_universe_mismatch",
			"market-data snapshot does not cover the strategy universe; missing: "+strings.Join(missing, ", "))
		return
	}

	run, job, err := server.options.Backtests.CreateBacktestWithRunJob(request.Context(), domain.CreateBacktestInput{
		StrategyID:       body.StrategyID,
		SnapshotID:       body.SnapshotID,
		DocumentCutoffAt: cutoff,
		Parameters:       body.Parameters,
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "backtest_create_failed", "could not create backtest")
		return
	}
	writeJSON(writer, http.StatusAccepted, backtestResponseFrom(run, &job))
}

func (server server) backtestByID(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if server.options.Backtests == nil {
		writeError(writer, http.StatusServiceUnavailable, "backtest_service_unavailable", "backtest service is not configured")
		return
	}

	id := strings.TrimPrefix(request.URL.Path, "/v1/backtests/")
	if !validUUID(id) {
		writeError(writer, http.StatusNotFound, "backtest_not_found", "backtest was not found")
		return
	}
	run, err := server.options.Backtests.GetBacktestRun(request.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "backtest_not_found", "backtest was not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "backtest_lookup_failed", "could not load backtest")
		return
	}
	writeJSON(writer, http.StatusOK, backtestResponseFrom(run, nil))
}

type backtestRequestBody struct {
	StrategyID       string          `json:"strategy_id"`
	SnapshotID       string          `json:"market_data_snapshot_id"`
	DocumentCutoffAt string          `json:"document_cutoff_at"`
	Parameters       json.RawMessage `json:"parameters,omitempty"`
}

type backtestResponse struct {
	ID                   string          `json:"id"`
	Status               string          `json:"status"`
	StrategyID           string          `json:"strategy_id"`
	MarketDataSnapshotID string          `json:"market_data_snapshot_id"`
	DocumentCutoffAt     time.Time       `json:"document_cutoff_at"`
	Summary              json.RawMessage `json:"summary,omitempty"`
	EngineVersion        *string         `json:"engine_version,omitempty"`
	ResultArtifactKey    *string         `json:"result_artifact_key,omitempty"`
	ResultChecksum       *string         `json:"result_checksum,omitempty"`
	JobID                string          `json:"job_id,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	CompletedAt          *time.Time      `json:"completed_at,omitempty"`
}

func backtestResponseFrom(run domain.BacktestRun, job *domain.Job) backtestResponse {
	response := backtestResponse{
		ID:                   run.ID,
		Status:               run.Status,
		StrategyID:           run.StrategyID,
		MarketDataSnapshotID: run.MarketDataSnapshotID,
		DocumentCutoffAt:     run.DocumentCutoffAt,
		EngineVersion:        run.EngineVersion,
		ResultArtifactKey:    run.ResultArtifactKey,
		ResultChecksum:       run.ResultChecksum,
		CreatedAt:            run.CreatedAt,
		CompletedAt:          run.CompletedAt,
	}
	if len(run.Summary) > 0 && string(run.Summary) != "{}" {
		response.Summary = run.Summary
	}
	if job != nil {
		response.JobID = job.ID
	}
	return response
}
