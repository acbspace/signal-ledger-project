package quant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"signalledger/internal/domain"
)

const validChecksum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func backtestRequestFixture() domain.BacktestRequest {
	horizon := 60
	return domain.BacktestRequest{
		BacktestID:         "run-1",
		Spec:               json.RawMessage(`{"slug":"macro-oil"}`),
		SnapshotStorageKey: "market-data/snapshot.csv",
		SnapshotChecksum:   validChecksum,
		DocumentCutoffAt:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Signals: []domain.ClaimSignal{{
			ClaimID:     "claim-1",
			Symbol:      "XLE",
			Direction:   "positive",
			Confidence:  0.7,
			EffectiveAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			HorizonDays: &horizon,
		}},
	}
}

// serveBacktest replies with body and captures the request the client sent.
func serveBacktest(t *testing.T, body string, captured *map[string]any) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if captured != nil {
			raw, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read request: %v", err)
			}
			if err := json.Unmarshal(raw, captured); err != nil {
				t.Errorf("decode request: %v", err)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, body)
	}))
	t.Cleanup(server.Close)
	return NewClient(server.URL, 5*time.Second)
}

func TestRunBacktestSendsSignalsAndDecodesCandidates(t *testing.T) {
	t.Parallel()

	captured := map[string]any{}
	client := serveBacktest(t, `{
		"summary": {"sharpe": 1.2},
		"equity_curve_csv": "date,equity\n2026-03-02,1.00000000\n",
		"checksum": "`+validChecksum+`",
		"engine_version": "momentum-claims-v2",
		"candidates": {
			"as_of": "2026-03-02",
			"positions": [{
				"symbol": "XLE", "rank": 1, "weight": 0.2, "score": 0.31,
				"momentum": 0.24, "momentum_rank": -0.34, "claim_support": 0.7,
				"evidence": [{"claim_id": "claim-1", "contribution": 0.7}]
			}]
		}
	}`, &captured)

	result, err := client.RunBacktest(context.Background(), backtestRequestFixture())
	if err != nil {
		t.Fatalf("run backtest: %v", err)
	}

	signals, ok := captured["signals"].([]any)
	if !ok || len(signals) != 1 {
		t.Fatalf("signals = %v", captured["signals"])
	}
	signal := signals[0].(map[string]any)
	if signal["claim_id"] != "claim-1" || signal["symbol"] != "XLE" || signal["horizon_days"] != float64(60) {
		t.Fatalf("signal = %v", signal)
	}

	if len(result.Candidates.Positions) != 1 {
		t.Fatalf("candidates = %+v", result.Candidates)
	}
	position := result.Candidates.Positions[0]
	if position.Symbol != "XLE" || position.Rank != 1 || position.ClaimSupport != 0.7 {
		t.Fatalf("position = %+v", position)
	}
	// Raw momentum and the ranked value the engine actually scored are separate
	// numbers under momentum-claims-v4, and a stored position needs both to
	// reproduce its own score.
	if position.Momentum != 0.24 || position.MomentumRank != -0.34 {
		t.Fatalf("position lost its momentum decomposition: %+v", position)
	}
	if len(position.Evidence) != 1 || position.Evidence[0].ClaimID != "claim-1" {
		t.Fatalf("evidence = %+v", position.Evidence)
	}
	if !result.Candidates.AsOf.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("as_of = %s", result.Candidates.AsOf)
	}
}

func TestRunBacktestOmitsHorizonWhenTheClaimHasNone(t *testing.T) {
	t.Parallel()

	captured := map[string]any{}
	client := serveBacktest(t, `{
		"summary": {},
		"equity_curve_csv": "date,equity\n",
		"checksum": "`+validChecksum+`",
		"engine_version": "momentum-claims-v2",
		"candidates": {"as_of": null, "positions": []}
	}`, &captured)

	request := backtestRequestFixture()
	request.Signals[0].HorizonDays = nil
	if _, err := client.RunBacktest(context.Background(), request); err != nil {
		t.Fatalf("run backtest: %v", err)
	}

	// Absent rather than zero, so the engine applies the spec's default horizon.
	signal := captured["signals"].([]any)[0].(map[string]any)
	if _, present := signal["horizon_days"]; present {
		t.Fatalf("horizon_days should be omitted: %v", signal)
	}
}

func TestRunBacktestRejectsUnrankedCandidates(t *testing.T) {
	t.Parallel()

	client := serveBacktest(t, `{
		"summary": {},
		"equity_curve_csv": "date,equity\n",
		"checksum": "`+validChecksum+`",
		"engine_version": "momentum-claims-v2",
		"candidates": {
			"as_of": "2026-03-02",
			"positions": [
				{"symbol": "XLE", "rank": 2, "weight": 0.2, "score": 0.31, "momentum": 0.24, "claim_support": 0.7}
			]
		}
	}`, nil)

	_, err := client.RunBacktest(context.Background(), backtestRequestFixture())
	if err == nil || !strings.Contains(err.Error(), "rank") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunBacktestRejectsCandidatesWithoutAnAsOfDate(t *testing.T) {
	t.Parallel()

	client := serveBacktest(t, `{
		"summary": {},
		"equity_curve_csv": "date,equity\n",
		"checksum": "`+validChecksum+`",
		"engine_version": "momentum-claims-v2",
		"candidates": {
			"positions": [
				{"symbol": "XLE", "rank": 1, "weight": 0.2, "score": 0.31, "momentum": 0.24, "claim_support": 0.7}
			]
		}
	}`, nil)

	_, err := client.RunBacktest(context.Background(), backtestRequestFixture())
	if err == nil || !strings.Contains(err.Error(), "as_of") {
		t.Fatalf("error = %v", err)
	}
}
