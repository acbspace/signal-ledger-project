package quant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"signalledger/internal/domain"
)

func (client *Client) RunBacktest(ctx context.Context, request domain.BacktestRequest) (domain.BacktestResult, error) {
	payload, err := json.Marshal(backtestRequest{
		BacktestID:         request.BacktestID,
		Spec:               request.Spec,
		SnapshotStorageKey: request.SnapshotStorageKey,
		SnapshotChecksum:   request.SnapshotChecksum,
		DocumentCutoffAt:   request.DocumentCutoffAt,
		Parameters:         rawOrEmpty(request.Parameters),
		Signals:            claimSignals(request.Signals),
	})
	if err != nil {
		return domain.BacktestResult{}, fmt.Errorf("encode backtest request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/v1/backtests", bytes.NewReader(payload))
	if err != nil {
		return domain.BacktestResult{}, fmt.Errorf("create backtest request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")

	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return domain.BacktestResult{}, fmt.Errorf("call quant backtest service: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return domain.BacktestResult{}, fmt.Errorf("read backtest response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return domain.BacktestResult{}, fmt.Errorf("backtest response exceeds size limit")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return domain.BacktestResult{}, fmt.Errorf("quant backtest service returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}

	var result backtestResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return domain.BacktestResult{}, fmt.Errorf("decode backtest response: %w", err)
	}
	if len(result.Checksum) != 64 || result.EngineVersion == "" || result.EquityCurveCSV == "" {
		return domain.BacktestResult{}, fmt.Errorf("quant backtest service returned an invalid result")
	}
	candidates, err := result.Candidates.decode()
	if err != nil {
		return domain.BacktestResult{}, err
	}
	return domain.BacktestResult{
		Summary:        result.Summary,
		EquityCurveCSV: result.EquityCurveCSV,
		ResultChecksum: result.Checksum,
		EngineVersion:  result.EngineVersion,
		Candidates:     candidates,
	}, nil
}

func rawOrEmpty(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(`{}`)
	}
	return value
}

// claimSignals keeps the wire order the caller established, which is what makes
// the engine's floating-point accumulation reproducible.
func claimSignals(signals []domain.ClaimSignal) []claimSignal {
	wire := make([]claimSignal, 0, len(signals))
	for _, signal := range signals {
		wire = append(wire, claimSignal{
			ClaimID:     signal.ClaimID,
			Symbol:      signal.Symbol,
			Direction:   signal.Direction,
			Confidence:  signal.Confidence,
			EffectiveAt: signal.EffectiveAt,
			HorizonDays: signal.HorizonDays,
		})
	}
	return wire
}

type backtestRequest struct {
	BacktestID         string          `json:"backtest_id"`
	Spec               json.RawMessage `json:"spec"`
	SnapshotStorageKey string          `json:"snapshot_storage_key"`
	SnapshotChecksum   string          `json:"snapshot_checksum"`
	DocumentCutoffAt   time.Time       `json:"document_cutoff_at"`
	Parameters         json.RawMessage `json:"parameters"`
	Signals            []claimSignal   `json:"signals"`
}

type claimSignal struct {
	ClaimID     string    `json:"claim_id"`
	Symbol      string    `json:"symbol"`
	Direction   string    `json:"direction"`
	Confidence  float64   `json:"confidence"`
	EffectiveAt time.Time `json:"effective_at"`
	HorizonDays *int      `json:"horizon_days,omitempty"`
}

type backtestResponse struct {
	Summary        json.RawMessage `json:"summary"`
	EquityCurveCSV string          `json:"equity_curve_csv"`
	Checksum       string          `json:"checksum"`
	EngineVersion  string          `json:"engine_version"`
	Candidates     candidateSet    `json:"candidates"`
}

type candidateSet struct {
	AsOf      string              `json:"as_of"`
	Positions []candidatePosition `json:"positions"`
}

type candidatePosition struct {
	Symbol       string              `json:"symbol"`
	Rank         int                 `json:"rank"`
	Weight       float64             `json:"weight"`
	Score        float64             `json:"score"`
	Momentum     float64             `json:"momentum"`
	MomentumRank float64             `json:"momentum_rank"`
	ClaimSupport float64             `json:"claim_support"`
	Evidence     []candidateEvidence `json:"evidence"`
}

type candidateEvidence struct {
	ClaimID      string  `json:"claim_id"`
	Contribution float64 `json:"contribution"`
}

// decode turns the engine's projection into the domain set, rejecting anything
// that would persist a ranking Go cannot vouch for.
func (set candidateSet) decode() (domain.CandidateSet, error) {
	decoded := domain.CandidateSet{Positions: make([]domain.CandidatePosition, 0, len(set.Positions))}
	if set.AsOf != "" {
		asOf, err := time.Parse(time.DateOnly, set.AsOf)
		if err != nil {
			return domain.CandidateSet{}, fmt.Errorf("decode candidate as_of: %w", err)
		}
		decoded.AsOf = asOf
	}

	for _, position := range set.Positions {
		evidence := make([]domain.CandidateEvidence, 0, len(position.Evidence))
		for _, item := range position.Evidence {
			if item.ClaimID == "" {
				return domain.CandidateSet{}, fmt.Errorf("candidate %s cites an unidentified claim", position.Symbol)
			}
			evidence = append(evidence, domain.CandidateEvidence{
				ClaimID:      item.ClaimID,
				Contribution: item.Contribution,
			})
		}
		decoded.Positions = append(decoded.Positions, domain.CandidatePosition{
			Symbol:       position.Symbol,
			Rank:         position.Rank,
			Weight:       position.Weight,
			Score:        position.Score,
			Momentum:     position.Momentum,
			MomentumRank: position.MomentumRank,
			ClaimSupport: position.ClaimSupport,
			Evidence:     evidence,
		})
	}

	if err := decoded.Validate(); err != nil {
		return domain.CandidateSet{}, fmt.Errorf("quant backtest service returned invalid candidates: %w", err)
	}
	return decoded, nil
}
