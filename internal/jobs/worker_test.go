package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"signalledger/internal/domain"
)

func TestBacktestSignalsRestrictsToTheSpecUniverse(t *testing.T) {
	t.Parallel()

	spec := json.RawMessage(`{
		"slug": "research-momentum-spy",
		"version": 1,
		"name": "Research momentum",
		"universe": {"name": "u", "asset_class": "equity", "symbols": ["SPY"]},
		"selection": {"template": "research-supported-momentum", "filters": []},
		"rebalance": {"schedule": "monthly"},
		"risk": {"max_position_weight": 0.2, "transaction_cost_bps": 5}
	}`)
	cutoff := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	spy, xom := "SPY", "XOM"
	citations := []domain.StoredClaim{
		{ID: "a", Ticker: &spy, Direction: "positive", Confidence: 0.6, EffectiveAt: cutoff.AddDate(0, 0, -1), ValidationStatus: "accepted"},
		{ID: "b", Ticker: &xom, Direction: "positive", Confidence: 0.9, EffectiveAt: cutoff.AddDate(0, 0, -1), ValidationStatus: "accepted"},
		{ID: "c", Ticker: &spy, Direction: "positive", Confidence: 0.9, EffectiveAt: cutoff.AddDate(0, 0, 1), ValidationStatus: "accepted"},
	}

	signals, err := backtestSignals(spec, citations, cutoff)
	if err != nil {
		t.Fatalf("build signals: %v", err)
	}

	// XOM is cited but not traded, and claim "c" is past the cutoff.
	if len(signals) != 1 || signals[0].ClaimID != "a" {
		t.Fatalf("signals = %+v", signals)
	}
}

func TestBacktestSignalsRejectsUnreadableSpec(t *testing.T) {
	t.Parallel()

	if _, err := backtestSignals(json.RawMessage(`not json`), nil, time.Now()); err == nil {
		t.Fatal("expected an error for an unreadable spec")
	}
}
