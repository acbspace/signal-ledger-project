package strategies

import (
	"reflect"
	"testing"
	"time"

	"signalledger/internal/domain"
)

var cutoff = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

func signalClaim(id, text, direction string, confidence float64, ticker *string, effective time.Time) domain.StoredClaim {
	return domain.StoredClaim{
		ID:               id,
		Ticker:           ticker,
		Kind:             "macro",
		Direction:        direction,
		Text:             text,
		EvidenceQuote:    text,
		Confidence:       confidence,
		EffectiveAt:      effective,
		ValidationStatus: "accepted",
	}
}

func symbolsOf(signals []domain.ClaimSignal) []string {
	symbols := make([]string, 0, len(signals))
	for _, signal := range signals {
		symbols = append(symbols, signal.Symbol)
	}
	return symbols
}

func TestBuildSignalsResolvesTickersAndMacroProxies(t *testing.T) {
	t.Parallel()

	ticker := "XOM"
	claims := []domain.StoredClaim{
		signalClaim("a", "XOM production growth beats expectations.", "positive", 0.8, &ticker, cutoff.AddDate(0, 0, -30)),
		signalClaim("b", "Treasury yields face a higher floor.", "positive", 0.6, nil, cutoff.AddDate(0, 0, -10)),
	}

	signals := BuildSignals(claims, []string{"IEF", "TLT", "XOM"}, cutoff)

	if got := symbolsOf(signals); !reflect.DeepEqual(got, []string{"IEF", "TLT", "XOM"}) {
		t.Fatalf("symbols = %v", got)
	}
	if signals[0].ClaimID != "b" || signals[0].Confidence != 0.6 {
		t.Fatalf("rates signal = %+v", signals[0])
	}
}

func TestBuildSignalsDropsClaimsOutsideTheUniverse(t *testing.T) {
	t.Parallel()

	ticker := "XOM"
	claims := []domain.StoredClaim{
		signalClaim("a", "XOM production growth beats expectations.", "positive", 0.8, &ticker, cutoff),
	}

	// The claim stays evidence for the strategy, but a symbol the spec does not
	// trade cannot become a trading signal.
	if signals := BuildSignals(claims, []string{"SPY"}, cutoff); len(signals) != 0 {
		t.Fatalf("expected no signals, got %v", symbolsOf(signals))
	}
}

func TestBuildSignalsEnforcesTheDocumentCutoff(t *testing.T) {
	t.Parallel()

	ticker := "SPY"
	claims := []domain.StoredClaim{
		signalClaim("past", "SPY earnings beat.", "positive", 0.5, &ticker, cutoff.Add(-time.Second)),
		signalClaim("future", "SPY earnings beat again.", "positive", 0.9, &ticker, cutoff.Add(time.Second)),
	}

	signals := BuildSignals(claims, []string{"SPY"}, cutoff)

	if len(signals) != 1 || signals[0].ClaimID != "past" {
		t.Fatalf("cutoff not enforced: %+v", signals)
	}
}

func TestBuildSignalsSkipsUnusableClaims(t *testing.T) {
	t.Parallel()

	ticker := "SPY"
	pending := signalClaim("pending", "SPY earnings beat.", "positive", 0.5, &ticker, cutoff)
	pending.ValidationStatus = "pending"
	rejected := signalClaim("rejected", "SPY earnings beat.", "positive", 0.5, &ticker, cutoff)
	rejected.ValidationStatus = "rejected"
	neutral := signalClaim("neutral", "SPY earnings are in line.", "neutral", 0.5, &ticker, cutoff)

	signals := BuildSignals([]domain.StoredClaim{pending, rejected, neutral}, []string{"SPY"}, cutoff)

	if len(signals) != 0 {
		t.Fatalf("expected no signals, got %+v", signals)
	}
}

func TestBuildSignalsIsDeterministic(t *testing.T) {
	t.Parallel()

	spy, lqd := "SPY", "LQD"
	claims := []domain.StoredClaim{
		signalClaim("c", "SPY earnings beat.", "positive", 0.5, &spy, cutoff.AddDate(0, 0, -1)),
		signalClaim("a", "LQD spreads tighten.", "negative", 0.4, &lqd, cutoff.AddDate(0, 0, -2)),
		signalClaim("b", "SPY earnings beat.", "positive", 0.7, &spy, cutoff.AddDate(0, 0, -1)),
	}

	forward := BuildSignals(claims, []string{"LQD", "SPY"}, cutoff)
	reverseInput := []domain.StoredClaim{claims[2], claims[1], claims[0]}
	backward := BuildSignals(reverseInput, []string{"SPY", "LQD"}, cutoff)

	if !reflect.DeepEqual(forward, backward) {
		t.Fatalf("signal order depends on input order:\n%+v\n%+v", forward, backward)
	}
	// Sorted by symbol, then effective time, then claim id.
	if got := []string{forward[0].ClaimID, forward[1].ClaimID, forward[2].ClaimID}; !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("claim order = %v", got)
	}
}
