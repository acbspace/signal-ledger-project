package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"signalledger/internal/domain"
	"signalledger/internal/strategies"
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

	signals, universe, err := backtestSignals(spec, citations, cutoff)
	if err != nil {
		t.Fatalf("build signals: %v", err)
	}

	// XOM is cited but not traded, and claim "c" is past the cutoff.
	if len(signals) != 1 || signals[0].ClaimID != "a" {
		t.Fatalf("signals = %+v", signals)
	}
	// The caller needs the universe back to check what the engine proposes.
	if len(universe) != 1 || universe[0] != "SPY" {
		t.Fatalf("universe = %v", universe)
	}
}

func TestBacktestSignalsRejectsUnreadableSpec(t *testing.T) {
	t.Parallel()

	if _, _, err := backtestSignals(json.RawMessage(`not json`), nil, time.Now()); err == nil {
		t.Fatal("expected an error for an unreadable spec")
	}
}

// The engine is supposed to rank only the committed universe, but the worker is
// what writes portfolio_candidates — so it verifies the ranking independently
// rather than trusting the service that produced it.
func TestCandidateSymbolsOutsideTheUniverseAreDetected(t *testing.T) {
	t.Parallel()

	universe := []string{"IEF", "TLT"}
	proposed := domain.CandidateSet{
		AsOf: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Positions: []domain.CandidatePosition{
			{Symbol: "TLT", Rank: 1, Weight: 0.5},
			{Symbol: "ZZZ", Rank: 2, Weight: 0.5},
		},
	}

	unauthorized := strategies.MissingSymbols(proposed.Symbols(), universe)

	if len(unauthorized) != 1 || unauthorized[0] != "ZZZ" {
		t.Fatalf("unauthorized = %v", unauthorized)
	}
}

func TestCandidateSymbolsInsideTheUniverseArePermitted(t *testing.T) {
	t.Parallel()

	proposed := domain.CandidateSet{Positions: []domain.CandidatePosition{
		{Symbol: "TLT", Rank: 1, Weight: 1.0},
	}}

	if unauthorized := strategies.MissingSymbols(proposed.Symbols(), []string{"IEF", "TLT"}); len(unauthorized) != 0 {
		t.Fatalf("unauthorized = %v", unauthorized)
	}
}
