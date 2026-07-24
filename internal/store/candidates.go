package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"signalledger/internal/domain"
)

// ListCandidates serves the ranked positions of completed backtest runs.
//
// With no filter it returns the latest completed run of every strategy, which
// is the paper portfolio as a whole. A strategy_id narrows that to one strategy;
// a backtest_id pins an exact historical run. Evidence is loaded in a second
// query and stitched on, mirroring GetStrategyWithCitations.
func (store *Store) ListCandidates(ctx context.Context, filter domain.CandidateFilter) ([]domain.Candidate, error) {
	filter = filter.Normalized()

	rows, err := store.pool.Query(ctx, `
		WITH selected_runs AS (
			SELECT DISTINCT ON (strategy_id) id
			FROM backtest_runs
			WHERE status = 'completed'
				AND ($1::uuid IS NULL OR strategy_id = $1::uuid)
				AND ($2::uuid IS NULL OR id = $2::uuid)
			ORDER BY strategy_id, completed_at DESC NULLS LAST, created_at DESC
		)
		SELECT candidates.id::text, candidates.backtest_run_id::text, candidates.strategy_id::text,
			strategies.slug, strategies.version, runs.engine_version, runs.result_checksum,
			candidates.as_of, candidates.symbol, candidates.rank, candidates.weight,
			candidates.score, candidates.momentum, candidates.claim_support
		FROM portfolio_candidates AS candidates
		JOIN selected_runs ON selected_runs.id = candidates.backtest_run_id
		JOIN backtest_runs AS runs ON runs.id = candidates.backtest_run_id
		JOIN strategies ON strategies.id = candidates.strategy_id
		ORDER BY strategies.slug, strategies.version DESC, candidates.rank
		LIMIT $3`,
		nullableUUID(filter.StrategyID), nullableUUID(filter.BacktestID), filter.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	defer rows.Close()

	candidates := []domain.Candidate{}
	index := map[string]int{}
	for rows.Next() {
		var candidate domain.Candidate
		var engineVersion, checksum pgtype.Text
		if err := rows.Scan(
			&candidate.ID, &candidate.BacktestRunID, &candidate.StrategyID,
			&candidate.StrategySlug, &candidate.StrategyVersion, &engineVersion, &checksum,
			&candidate.AsOf, &candidate.Symbol, &candidate.Rank, &candidate.Weight,
			&candidate.Score, &candidate.Momentum, &candidate.ClaimSupport,
		); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		if engineVersion.Valid {
			value := engineVersion.String
			candidate.EngineVersion = &value
		}
		if checksum.Valid {
			value := checksum.String
			candidate.ResultChecksum = &value
		}
		candidate.Evidence = []domain.CandidateClaim{}
		index[candidate.ID] = len(candidates)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	if len(candidates) == 0 {
		return candidates, nil
	}

	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	if err := store.attachCandidateEvidence(ctx, candidates, index, ids); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (store *Store) attachCandidateEvidence(ctx context.Context, candidates []domain.Candidate, index map[string]int, ids []string) error {
	rows, err := store.pool.Query(ctx, `
		SELECT candidate_claims.candidate_id::text, candidate_claims.contribution, `+storedClaimColumns+`
		FROM candidate_claims
		JOIN research_claims ON research_claims.id = candidate_claims.claim_id
		JOIN document_pages ON document_pages.id = research_claims.page_id
		WHERE candidate_claims.candidate_id = ANY($1::uuid[])
		ORDER BY candidate_claims.candidate_id, research_claims.effective_at, research_claims.id`,
		ids,
	)
	if err != nil {
		return fmt.Errorf("list candidate evidence: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		candidateID, evidence, err := scanCandidateEvidence(rows)
		if err != nil {
			return fmt.Errorf("scan candidate evidence: %w", err)
		}
		position, ok := index[candidateID]
		if !ok {
			continue
		}
		candidates[position].Evidence = append(candidates[position].Evidence, evidence)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate candidate evidence: %w", err)
	}
	return nil
}

func scanCandidateEvidence(row pgx.Row) (string, domain.CandidateClaim, error) {
	var candidateID string
	var contribution float64
	var claim domain.StoredClaim
	var ticker pgtype.Text
	var horizonDays pgtype.Int4

	targets := append([]any{&candidateID, &contribution}, claimScanTargets(&claim, &ticker, &horizonDays)...)
	if err := row.Scan(targets...); err != nil {
		return "", domain.CandidateClaim{}, err
	}
	applyNullableClaimFields(&claim, ticker, horizonDays)
	return candidateID, domain.CandidateClaim{Claim: claim, Contribution: contribution}, nil
}

func nullableUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}
