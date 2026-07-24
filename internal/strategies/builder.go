package strategies

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"signalledger/internal/domain"
)

// The builder turns accepted claims into a reviewable draft spec. It is fully
// deterministic: the same claims always yield the same draft, so a human can
// diff, edit, and then commit the result as an immutable version.

// macroTheme maps recurring sell-side research themes to long-only ETF proxies.
// Direction is interpreted as the view on the theme's proxy assets; themes with
// net-negative sentiment are excluded rather than shorted in v1.
type macroTheme struct {
	name     string
	keywords []string
	symbols  []string
}

var macroThemes = []macroTheme{
	{name: "rates", keywords: []string{"rate", "treasur", "yield", "duration", "fed", "bond"}, symbols: []string{"IEF", "TLT"}},
	{name: "oil", keywords: []string{"oil", "crude", "opec", "brent", "wti", "energy"}, symbols: []string{"USO", "XLE"}},
	{name: "dollar", keywords: []string{"dollar", "currency", "fx", "euro", "yen"}, symbols: []string{"UUP"}},
	{name: "credit", keywords: []string{"credit", "spread", "high yield", "investment grade", "hybrid"}, symbols: []string{"LQD", "HYG"}},
	{name: "equities", keywords: []string{"equit", "stock", "s&p", "earnings"}, symbols: []string{"SPY"}},
}

type Draft struct {
	Spec   Spec
	Claims []domain.StoredClaim
}

// BuildDraft proposes a spec from accepted claims. Ticker-bearing claims win an
// equity momentum draft; otherwise macro claims are netted per theme
// (confidence-weighted, positive minus negative) into a long-only ETF universe.
func BuildDraft(claims []domain.StoredClaim) (Draft, error) {
	accepted := make([]domain.StoredClaim, 0, len(claims))
	for _, claim := range claims {
		if claim.ValidationStatus == "accepted" {
			accepted = append(accepted, claim)
		}
	}
	if len(accepted) == 0 {
		return Draft{}, fmt.Errorf("no accepted claims to build from")
	}

	tickerSymbols := netTickerSymbols(accepted)
	if len(tickerSymbols) > 0 {
		return equityDraft(tickerSymbols, accepted), nil
	}

	themeNames, themeSymbols := netMacroThemes(accepted)
	if len(themeSymbols) == 0 {
		return Draft{}, fmt.Errorf("accepted claims have no net-positive ticker or macro theme")
	}
	return macroDraft(themeNames, themeSymbols, accepted), nil
}

func netTickerSymbols(claims []domain.StoredClaim) []string {
	net := map[string]float64{}
	for _, claim := range claims {
		if claim.Ticker == nil {
			continue
		}
		net[*claim.Ticker] += signedConfidence(claim)
	}
	symbols := make([]string, 0, len(net))
	for symbol, score := range net {
		if score > 0 {
			symbols = append(symbols, symbol)
		}
	}
	sort.Strings(symbols)
	return symbols
}

func netMacroThemes(claims []domain.StoredClaim) ([]string, []string) {
	net := map[string]float64{}
	for _, claim := range claims {
		for _, theme := range claimThemes(claim) {
			net[theme.name] += signedConfidence(claim)
		}
	}

	names := []string{}
	symbols := []string{}
	for _, theme := range macroThemes {
		if net[theme.name] > 0 {
			names = append(names, theme.name)
			symbols = append(symbols, theme.symbols...)
		}
	}
	return names, symbols
}

func signedConfidence(claim domain.StoredClaim) float64 {
	switch claim.Direction {
	case "positive":
		return claim.Confidence
	case "negative":
		return -claim.Confidence
	default:
		return 0
	}
}

func equityDraft(symbols []string, claims []domain.StoredClaim) Draft {
	slug := draftSlug("research-momentum", symbols)
	return Draft{
		Spec: Spec{
			Slug:    slug,
			Version: 1,
			Name:    "Research draft: " + strings.Join(symbols, ", "),
			Universe: Universe{
				Name:       "research-cited-equities",
				AssetClass: "equity",
				Symbols:    symbols,
			},
			Selection: Selection{
				Template: "research-supported-momentum",
				Filters:  defaultFilters(claims),
			},
			Rebalance: Rebalance{Schedule: "monthly"},
			Risk:      defaultRisk(),
		},
		Claims: claims,
	}
}

func macroDraft(themes, symbols []string, claims []domain.StoredClaim) Draft {
	slug := draftSlug("macro", themes)
	return Draft{
		Spec: Spec{
			Slug:    slug,
			Version: 1,
			Name:    "Macro theme draft: " + strings.Join(themes, ", "),
			Universe: Universe{
				Name:       "macro-theme-etf-proxies",
				AssetClass: "etf",
				Symbols:    symbols,
			},
			Selection: Selection{
				Template: "macro-theme-etf",
				Filters:  defaultFilters(claims),
			},
			Rebalance: Rebalance{Schedule: "monthly"},
			Risk:      defaultRisk(),
		},
		Claims: claims,
	}
}

// defaultFilters makes the draft's use of research explicit: the minimum
// confidence a claim needs to become a signal, and how hard that signal tilts
// the momentum ranking. A reviewer can raise or drop either before committing.
func defaultFilters(claims []domain.StoredClaim) []Filter {
	minimum := 1.0
	for _, claim := range claims {
		if claim.Confidence < minimum {
			minimum = claim.Confidence
		}
	}
	return []Filter{
		{
			Field:    "claim_confidence",
			Operator: "gte",
			Value:    math.Floor(minimum*100) / 100,
		},
		{
			Field:    "claim_signal_weight",
			Operator: "eq",
			Value:    defaultSignalWeight,
		},
	}
}

// defaultSignalWeight mirrors _DEFAULT_SIGNAL_WEIGHT in the engine: a
// full-confidence claim is worth ten points of trailing return.
const defaultSignalWeight = 0.1

func defaultRisk() Risk {
	return Risk{
		MaxPositionWeight:  0.2,
		TransactionCostBps: 5,
	}
}

func draftSlug(prefix string, parts []string) string {
	joined := strings.ToLower(prefix + "-" + strings.Join(parts, "-"))
	sanitized := strings.Map(func(character rune) rune {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			return character
		}
		return '-'
	}, joined)
	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}
	return strings.Trim(sanitized, "-")
}
