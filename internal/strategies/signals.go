package strategies

import (
	"sort"
	"strings"
	"time"

	"signalledger/internal/domain"
)

// Claims become selection inputs here. Drafting (builder.go) asks which symbols a
// body of research points at once, when a human is composing a universe; this
// file asks the same question per claim, per day, so a backtest can tilt toward
// the evidence that already existed at each rebalance.

// ClaimSymbols resolves the symbols a claim speaks to: its ticker when it names
// one, otherwise the ETF proxies of every macro theme its text mentions.
func ClaimSymbols(claim domain.StoredClaim) []string {
	if claim.Ticker != nil && strings.TrimSpace(*claim.Ticker) != "" {
		return []string{strings.TrimSpace(*claim.Ticker)}
	}
	symbols := []string{}
	for _, theme := range claimThemes(claim) {
		symbols = append(symbols, theme.symbols...)
	}
	return symbols
}

// BuildSignals projects claims onto the strategy universe for one backtest run.
//
// Three gates apply before the engine ever sees a claim: it must have been
// accepted by a human, it must name a symbol the strategy actually trades, and
// it must have been effective at or before the run's document cutoff. The last
// one is the look-ahead gate — research the run could not have read cannot reach
// the simulation. Neutral claims are dropped because they carry no direction to
// tilt with.
//
// The result is sorted on a total key so the payload — and therefore the
// engine's floating-point accumulation order — is identical for identical input.
func BuildSignals(claims []domain.StoredClaim, universe []string, cutoff time.Time) []domain.ClaimSignal {
	tradable := make(map[string]struct{}, len(universe))
	for _, symbol := range universe {
		tradable[symbol] = struct{}{}
	}

	signals := []domain.ClaimSignal{}
	seen := map[[2]string]struct{}{}
	for _, claim := range claims {
		if claim.ValidationStatus != "accepted" || claim.Direction == "neutral" {
			continue
		}
		if claim.EffectiveAt.After(cutoff) {
			continue
		}
		for _, symbol := range ClaimSymbols(claim) {
			if _, ok := tradable[symbol]; !ok {
				continue
			}
			key := [2]string{claim.ID, symbol}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			signals = append(signals, domain.ClaimSignal{
				ClaimID:     claim.ID,
				Symbol:      symbol,
				Direction:   claim.Direction,
				Confidence:  claim.Confidence,
				EffectiveAt: claim.EffectiveAt,
				HorizonDays: claim.HorizonDays,
			})
		}
	}

	sort.Slice(signals, func(first, second int) bool {
		left, right := signals[first], signals[second]
		if left.Symbol != right.Symbol {
			return left.Symbol < right.Symbol
		}
		if !left.EffectiveAt.Equal(right.EffectiveAt) {
			return left.EffectiveAt.Before(right.EffectiveAt)
		}
		return left.ClaimID < right.ClaimID
	})
	return signals
}

// claimThemes returns the macro themes a claim's text and evidence mention.
func claimThemes(claim domain.StoredClaim) []macroTheme {
	lower := strings.ToLower(claim.Text + " " + claim.EvidenceQuote)
	matched := []macroTheme{}
	for _, theme := range macroThemes {
		for _, keyword := range theme.keywords {
			if strings.Contains(lower, keyword) {
				matched = append(matched, theme)
				break
			}
		}
	}
	return matched
}
