package strategies

import (
	"reflect"
	"testing"
	"time"

	"signalledger/internal/domain"
)

func acceptedClaim(text, direction string, confidence float64, ticker *string) domain.StoredClaim {
	return domain.StoredClaim{
		ID:               "claim",
		Ticker:           ticker,
		Kind:             "macro",
		Direction:        direction,
		Text:             text,
		EvidenceQuote:    text,
		Confidence:       confidence,
		EffectiveAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidationStatus: "accepted",
	}
}

func TestBuildDraftMapsMacroThemesToETFs(t *testing.T) {
	t.Parallel()

	claims := []domain.StoredClaim{
		acceptedClaim("Oil demand destruction appears smaller than feared; crude balances tighten.", "positive", 0.7, nil),
		acceptedClaim("Treasury yields face a higher floor as the Fed holds rates.", "negative", 0.6, nil),
	}

	draft, err := BuildDraft(claims)
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}
	if draft.Spec.Selection.Template != "macro-theme-etf" {
		t.Fatalf("template = %q", draft.Spec.Selection.Template)
	}
	if draft.Spec.Universe.AssetClass != "etf" {
		t.Fatalf("asset class = %q", draft.Spec.Universe.AssetClass)
	}
	// Oil netted positive so its proxies appear; rates netted negative so they
	// are excluded from the long-only universe.
	if !reflect.DeepEqual(draft.Spec.Universe.Symbols, []string{"USO", "XLE"}) {
		t.Fatalf("symbols = %v", draft.Spec.Universe.Symbols)
	}
	if err := draft.Spec.Validate(); err != nil {
		t.Fatalf("draft spec does not validate: %v", err)
	}
}

func TestBuildDraftPrefersTickerClaims(t *testing.T) {
	t.Parallel()

	ticker := "XOM"
	claims := []domain.StoredClaim{
		acceptedClaim("XOM production growth beats expectations.", "positive", 0.8, &ticker),
		acceptedClaim("Oil demand should improve.", "positive", 0.6, nil),
	}

	draft, err := BuildDraft(claims)
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}
	if draft.Spec.Selection.Template != "research-supported-momentum" {
		t.Fatalf("template = %q", draft.Spec.Selection.Template)
	}
	if !reflect.DeepEqual(draft.Spec.Universe.Symbols, []string{"XOM"}) {
		t.Fatalf("symbols = %v", draft.Spec.Universe.Symbols)
	}
}

func TestBuildDraftIsDeterministic(t *testing.T) {
	t.Parallel()

	claims := []domain.StoredClaim{
		acceptedClaim("Credit spreads should stay contained; the hybrid renaissance continues.", "positive", 0.65, nil),
		acceptedClaim("Dollar strength should fade in the second half.", "negative", 0.55, nil),
	}

	first, err := BuildDraft(claims)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := BuildDraft(claims)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if !reflect.DeepEqual(first.Spec, second.Spec) {
		t.Fatalf("drafts differ:\n%+v\n%+v", first.Spec, second.Spec)
	}
}

func TestBuildDraftRejectsUnreviewedClaims(t *testing.T) {
	t.Parallel()

	claim := acceptedClaim("Oil demand should improve.", "positive", 0.6, nil)
	claim.ValidationStatus = "pending"

	if _, err := BuildDraft([]domain.StoredClaim{claim}); err == nil {
		t.Fatal("expected error for unreviewed claims")
	}
}
