package strategies

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Spec mirrors contracts/strategy-spec.schema.json. Validation is hand-rolled
// (like domain.Extraction) so the API boundary needs no schema-library
// dependency; the JSON file remains the cross-service contract of record.
type Spec struct {
	Slug      string    `json:"slug"`
	Version   int       `json:"version"`
	Name      string    `json:"name"`
	Universe  Universe  `json:"universe"`
	Selection Selection `json:"selection"`
	Rebalance Rebalance `json:"rebalance"`
	Risk      Risk      `json:"risk"`
}

type Universe struct {
	Name       string   `json:"name"`
	AssetClass string   `json:"asset_class"`
	Symbols    []string `json:"symbols,omitempty"`
}

type Selection struct {
	Template string   `json:"template"`
	Filters  []Filter `json:"filters"`
}

type Filter struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
}

type Rebalance struct {
	Schedule string `json:"schedule"`
}

type Risk struct {
	MaxPositionWeight  float64  `json:"max_position_weight"`
	TransactionCostBps float64  `json:"transaction_cost_bps"`
	StopLossPct        *float64 `json:"stop_loss_pct,omitempty"`
}

var (
	slugPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	symbolPattern = regexp.MustCompile(`^[A-Z.]{1,10}$`)
)

func validTemplate(template string) bool {
	switch template {
	case "research-supported-momentum", "quality-trend", "macro-theme-etf":
		return true
	default:
		return false
	}
}

// filterFields are the selection knobs the engine actually reads. Rejecting
// anything else keeps a typo from committing an immutable version whose filter
// silently does nothing. Keep in sync with `_engine_params` in
// python/quant_service/backtest.py and the JSON contract.
var filterFields = map[string]bool{
	"lookback_months":       true,
	"top_n":                 true,
	"momentum":              true,
	"claim_confidence":      true,
	"claim_signal_weight":   true,
	"claim_horizon_days":    true,
	"require_claim_support": true,
}

// UniverseSymbols decodes a committed spec and returns the symbols it authorizes
// for trading. Committed specs are stored as JSON, so every consumer that has to
// check the universe decodes first; keeping that in one place means the API, the
// worker, and the engine are all asking the same question of the same field.
func UniverseSymbols(rawSpec json.RawMessage) ([]string, error) {
	var spec Spec
	if err := json.Unmarshal(rawSpec, &spec); err != nil {
		return nil, fmt.Errorf("decode strategy spec: %w", err)
	}
	return spec.Universe.Symbols, nil
}

// MissingSymbols reports which of `required` are absent from `available`, in the
// order `required` lists them. It answers the two universe questions the pipeline
// asks — does a snapshot cover the symbols a spec authorizes, and does a proposed
// ranking stay inside them — so both get the same comparison.
func MissingSymbols(required, available []string) []string {
	present := make(map[string]struct{}, len(available))
	for _, symbol := range available {
		present[symbol] = struct{}{}
	}

	missing := []string{}
	seen := map[string]struct{}{}
	for _, symbol := range required {
		if _, ok := present[symbol]; ok {
			continue
		}
		if _, duplicate := seen[symbol]; duplicate {
			continue
		}
		seen[symbol] = struct{}{}
		missing = append(missing, symbol)
	}
	return missing
}

func (spec Spec) Validate() error {
	if !slugPattern.MatchString(spec.Slug) {
		return fmt.Errorf("slug must match %s", slugPattern.String())
	}
	if spec.Version < 1 {
		return fmt.Errorf("version must be at least 1")
	}
	if length := len(strings.TrimSpace(spec.Name)); length < 3 || length > 120 {
		return fmt.Errorf("name must be between 3 and 120 characters")
	}

	if strings.TrimSpace(spec.Universe.Name) == "" {
		return fmt.Errorf("universe name is required")
	}
	switch spec.Universe.AssetClass {
	case "equity", "etf":
	default:
		return fmt.Errorf("universe asset_class must be equity or etf")
	}
	if len(spec.Universe.Symbols) == 0 {
		return fmt.Errorf("universe must list at least one symbol")
	}
	for _, symbol := range spec.Universe.Symbols {
		if !symbolPattern.MatchString(symbol) {
			return fmt.Errorf("invalid universe symbol %q", symbol)
		}
	}

	if !validTemplate(spec.Selection.Template) {
		return fmt.Errorf("invalid selection template %q", spec.Selection.Template)
	}
	for index, filter := range spec.Selection.Filters {
		if !filterFields[filter.Field] {
			return fmt.Errorf("filter %d has unknown field %q", index, filter.Field)
		}
		switch filter.Operator {
		case "gt", "gte", "lt", "lte", "eq":
		default:
			return fmt.Errorf("filter %d has invalid operator %q", index, filter.Operator)
		}
		switch filter.Value.(type) {
		case float64, int, string, bool:
		default:
			return fmt.Errorf("filter %d value must be a number, string, or boolean", index)
		}
	}

	switch spec.Rebalance.Schedule {
	case "weekly", "monthly", "quarterly":
	default:
		return fmt.Errorf("rebalance schedule must be weekly, monthly, or quarterly")
	}

	if spec.Risk.MaxPositionWeight <= 0 || spec.Risk.MaxPositionWeight > 1 {
		return fmt.Errorf("max_position_weight must be in (0, 1]")
	}
	if spec.Risk.TransactionCostBps < 0 || spec.Risk.TransactionCostBps > 1000 {
		return fmt.Errorf("transaction_cost_bps must be in [0, 1000]")
	}
	if spec.Risk.StopLossPct != nil && (*spec.Risk.StopLossPct <= 0 || *spec.Risk.StopLossPct > 1) {
		return fmt.Errorf("stop_loss_pct must be in (0, 1]")
	}
	return nil
}
