package strategies

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUniverseSymbolsReadsTheCommittedSpec(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"universe":{"name":"u","asset_class":"etf","symbols":["IEF","TLT"]}}`)

	symbols, err := UniverseSymbols(raw)
	if err != nil {
		t.Fatalf("universe symbols: %v", err)
	}
	if len(symbols) != 2 || symbols[0] != "IEF" || symbols[1] != "TLT" {
		t.Fatalf("symbols = %v", symbols)
	}
}

func TestUniverseSymbolsRejectsUnreadableSpec(t *testing.T) {
	t.Parallel()

	if _, err := UniverseSymbols(json.RawMessage(`not json`)); err == nil {
		t.Fatal("expected an error for an unreadable spec")
	}
}

func TestMissingSymbols(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		required  []string
		available []string
		expected  []string
	}{
		{"fully covered", []string{"IEF", "TLT"}, []string{"IEF", "SPY", "TLT"}, []string{}},
		{"one gap", []string{"IEF", "TLT"}, []string{"IEF"}, []string{"TLT"}},
		{"nothing available", []string{"IEF"}, nil, []string{"IEF"}},
		{"nothing required", nil, []string{"IEF"}, []string{}},
		// Order follows `required` so the error message is stable, and a symbol
		// listed twice is reported once.
		{"deduplicated", []string{"TLT", "IEF", "TLT"}, []string{"SPY"}, []string{"TLT", "IEF"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			missing := MissingSymbols(testCase.required, testCase.available)

			if len(missing) != len(testCase.expected) {
				t.Fatalf("missing = %v, want %v", missing, testCase.expected)
			}
			for index, symbol := range testCase.expected {
				if missing[index] != symbol {
					t.Fatalf("missing = %v, want %v", missing, testCase.expected)
				}
			}
		})
	}
}

func validSpec() Spec {
	return Spec{
		Slug:    "macro-oil",
		Version: 1,
		Name:    "Macro oil theme",
		Universe: Universe{
			Name:       "macro-theme-etf-proxies",
			AssetClass: "etf",
			Symbols:    []string{"USO", "XLE"},
		},
		Selection: Selection{
			Template: "macro-theme-etf",
			Filters:  []Filter{{Field: "claim_confidence", Operator: "gte", Value: 0.5}},
		},
		Rebalance: Rebalance{Schedule: "monthly"},
		Risk:      Risk{MaxPositionWeight: 0.2, TransactionCostBps: 5},
	}
}

func TestSpecValidateAcceptsValidSpec(t *testing.T) {
	t.Parallel()
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestSpecValidateRejectsInvalidSpecs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*Spec)
		wantErr string
	}{
		{"bad slug", func(spec *Spec) { spec.Slug = "Bad Slug!" }, "slug"},
		{"zero version", func(spec *Spec) { spec.Version = 0 }, "version"},
		{"short name", func(spec *Spec) { spec.Name = "ab" }, "name"},
		{"bad asset class", func(spec *Spec) { spec.Universe.AssetClass = "crypto" }, "asset_class"},
		{"no symbols", func(spec *Spec) { spec.Universe.Symbols = nil }, "symbol"},
		{"bad symbol", func(spec *Spec) { spec.Universe.Symbols = []string{"uso"} }, "symbol"},
		{"bad template", func(spec *Spec) { spec.Selection.Template = "yolo" }, "template"},
		{"bad operator", func(spec *Spec) { spec.Selection.Filters[0].Operator = "like" }, "operator"},
		// A filter the engine does not read would silently do nothing.
		{"unknown filter field", func(spec *Spec) { spec.Selection.Filters[0].Field = "claim_confidance" }, "unknown field"},
		{"bad schedule", func(spec *Spec) { spec.Rebalance.Schedule = "hourly" }, "schedule"},
		{"weight too high", func(spec *Spec) { spec.Risk.MaxPositionWeight = 1.5 }, "max_position_weight"},
		{"negative costs", func(spec *Spec) { spec.Risk.TransactionCostBps = -1 }, "transaction_cost_bps"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			spec := validSpec()
			testCase.mutate(&spec)
			err := spec.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error %q does not mention %q", err, testCase.wantErr)
			}
		})
	}
}
