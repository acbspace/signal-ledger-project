package domain

import (
	"fmt"
	"time"
)

// CandidateSet is a completed backtest's last rebalance: the paper portfolio
// that run proposes, ranked, with each position attributed to the claims that
// were live behind it on AsOf.
type CandidateSet struct {
	AsOf      time.Time
	Positions []CandidatePosition
}

type CandidatePosition struct {
	Symbol       string
	Rank         int
	Weight       float64
	Score        float64
	Momentum     float64
	ClaimSupport float64
	Evidence     []CandidateEvidence
}

// CandidateEvidence attributes one position to one claim. Contribution is that
// claim's own signed confidence; contributions need not sum to ClaimSupport,
// which is the net clamped to [-1, 1].
type CandidateEvidence struct {
	ClaimID      string
	Contribution float64
}

func (set CandidateSet) Validate() error {
	if len(set.Positions) == 0 {
		return nil
	}
	if set.AsOf.IsZero() {
		return fmt.Errorf("candidate set has positions but no as_of date")
	}
	for index, position := range set.Positions {
		if position.Symbol == "" {
			return fmt.Errorf("candidate %d is missing a symbol", index)
		}
		if position.Rank != index+1 {
			return fmt.Errorf("candidate %d has rank %d; ranks must be dense and ordered", index, position.Rank)
		}
		if position.Weight <= 0 {
			return fmt.Errorf("candidate %s has non-positive weight", position.Symbol)
		}
	}
	return nil
}

// Candidate is one ranked position as served by the API. It carries the run
// that produced it — and therefore that run's reproducibility inputs — plus the
// page-cited research behind it.
type Candidate struct {
	ID              string
	BacktestRunID   string
	StrategyID      string
	StrategySlug    string
	StrategyVersion int
	EngineVersion   *string
	ResultChecksum  *string
	AsOf            time.Time
	Symbol          string
	Rank            int
	Weight          float64
	Score           float64
	Momentum        float64
	ClaimSupport    float64
	Evidence        []CandidateClaim
}

type CandidateClaim struct {
	Claim        StoredClaim
	Contribution float64
}

// CandidateFilter selects which run's candidates to serve. With neither ID set
// the store serves the latest completed run of every strategy — the paper
// portfolio as a whole.
type CandidateFilter struct {
	StrategyID string
	BacktestID string
	Limit      int
}

const (
	DefaultCandidateLimit = 100
	MaxCandidateLimit     = 500
)

func (filter CandidateFilter) Normalized() CandidateFilter {
	if filter.Limit <= 0 {
		filter.Limit = DefaultCandidateLimit
	}
	if filter.Limit > MaxCandidateLimit {
		filter.Limit = MaxCandidateLimit
	}
	return filter
}
