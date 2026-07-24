package domain

import (
	"fmt"
	"strings"
	"time"
)

type Extraction struct {
	Pages  []Page
	Claims []Claim
}

// StoredClaim is a persisted research claim, including the review state that
// gates whether a strategy may cite it as evidence.
type StoredClaim struct {
	ID               string
	DocumentID       string
	PageNumber       int
	Ticker           *string
	Kind             string
	Direction        string
	Text             string
	EvidenceQuote    string
	HorizonDays      *int
	Confidence       float64
	EffectiveAt      time.Time
	ValidationStatus string
	CreatedAt        time.Time
}

func ValidClaimReviewStatus(status string) bool {
	return status == "accepted" || status == "rejected"
}

type Page struct {
	Number  int
	Content string
}

type Claim struct {
	PageNumber    int
	Ticker        *string
	Kind          string
	Direction     string
	Text          string
	EvidenceQuote string
	HorizonDays   *int
	Confidence    float64
	EffectiveAt   time.Time
}

func (extraction Extraction) Validate() error {
	if len(extraction.Pages) == 0 {
		return fmt.Errorf("extraction contains no pages")
	}

	pages := make(map[int]struct{}, len(extraction.Pages))
	for _, page := range extraction.Pages {
		if page.Number < 1 {
			return fmt.Errorf("invalid page number %d", page.Number)
		}
		if _, exists := pages[page.Number]; exists {
			return fmt.Errorf("duplicate page number %d", page.Number)
		}
		pages[page.Number] = struct{}{}
	}

	for index, claim := range extraction.Claims {
		if _, exists := pages[claim.PageNumber]; !exists {
			return fmt.Errorf("claim %d references missing page %d", index, claim.PageNumber)
		}
		if strings.TrimSpace(claim.Text) == "" || strings.TrimSpace(claim.EvidenceQuote) == "" {
			return fmt.Errorf("claim %d is missing text or evidence", index)
		}
		if !validClaimKind(claim.Kind) {
			return fmt.Errorf("claim %d has invalid kind %q", index, claim.Kind)
		}
		if !validDirection(claim.Direction) {
			return fmt.Errorf("claim %d has invalid direction %q", index, claim.Direction)
		}
		if claim.Confidence < 0 || claim.Confidence > 1 {
			return fmt.Errorf("claim %d has invalid confidence", index)
		}
		if claim.HorizonDays != nil && *claim.HorizonDays < 1 {
			return fmt.Errorf("claim %d has invalid horizon", index)
		}
		if claim.EffectiveAt.IsZero() {
			return fmt.Errorf("claim %d is missing effective time", index)
		}
	}

	return nil
}

func validClaimKind(kind string) bool {
	switch kind {
	case "fundamental", "macro", "risk", "catalyst", "valuation":
		return true
	default:
		return false
	}
}

func validDirection(direction string) bool {
	switch direction {
	case "positive", "negative", "neutral":
		return true
	default:
		return false
	}
}
