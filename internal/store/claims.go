package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"signalledger/internal/domain"
)

const storedClaimColumns = `
	research_claims.id::text,
	research_claims.document_id::text,
	document_pages.page_number,
	research_claims.ticker,
	research_claims.claim_kind,
	research_claims.direction,
	research_claims.claim,
	research_claims.evidence_quote,
	research_claims.horizon_days,
	research_claims.confidence,
	research_claims.effective_at,
	research_claims.validation_status,
	research_claims.created_at`

func (store *Store) ListClaimsByDocument(ctx context.Context, documentID string) ([]domain.StoredClaim, error) {
	if _, err := store.GetDocument(ctx, documentID); err != nil {
		return nil, err
	}

	rows, err := store.pool.Query(ctx, `
		SELECT `+storedClaimColumns+`
		FROM research_claims
		JOIN document_pages ON document_pages.id = research_claims.page_id
		WHERE research_claims.document_id = $1
		ORDER BY document_pages.page_number, research_claims.created_at, research_claims.id`,
		documentID,
	)
	if err != nil {
		return nil, fmt.Errorf("list claims for document: %w", err)
	}
	defer rows.Close()
	return scanStoredClaims(rows)
}

func (store *Store) GetClaimsByIDs(ctx context.Context, ids []string) ([]domain.StoredClaim, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := store.pool.Query(ctx, `
		SELECT `+storedClaimColumns+`
		FROM research_claims
		JOIN document_pages ON document_pages.id = research_claims.page_id
		WHERE research_claims.id = ANY($1::uuid[])
		ORDER BY research_claims.effective_at, research_claims.id`,
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("get claims by ids: %w", err)
	}
	defer rows.Close()
	return scanStoredClaims(rows)
}

func (store *Store) SetClaimValidationStatus(ctx context.Context, claimID, status string) (domain.StoredClaim, error) {
	if !domain.ValidClaimReviewStatus(status) {
		return domain.StoredClaim{}, fmt.Errorf("invalid claim validation status %q", status)
	}

	claim, err := scanStoredClaim(store.pool.QueryRow(ctx, `
		WITH updated AS (
			UPDATE research_claims
			SET validation_status = $2
			WHERE id = $1
			RETURNING *
		)
		SELECT
			updated.id::text,
			updated.document_id::text,
			document_pages.page_number,
			updated.ticker,
			updated.claim_kind,
			updated.direction,
			updated.claim,
			updated.evidence_quote,
			updated.horizon_days,
			updated.confidence,
			updated.effective_at,
			updated.validation_status,
			updated.created_at
		FROM updated
		JOIN document_pages ON document_pages.id = updated.page_id`,
		claimID,
		status,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.StoredClaim{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.StoredClaim{}, fmt.Errorf("set claim validation status: %w", err)
	}
	return claim, nil
}

func scanStoredClaims(rows pgx.Rows) ([]domain.StoredClaim, error) {
	claims := []domain.StoredClaim{}
	for rows.Next() {
		claim, err := scanStoredClaim(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claims: %w", err)
	}
	return claims, nil
}

func scanStoredClaim(row pgx.Row) (domain.StoredClaim, error) {
	var claim domain.StoredClaim
	var ticker pgtype.Text
	var horizonDays pgtype.Int4
	if err := row.Scan(claimScanTargets(&claim, &ticker, &horizonDays)...); err != nil {
		return domain.StoredClaim{}, err
	}
	applyNullableClaimFields(&claim, ticker, horizonDays)
	return claim, nil
}

// claimScanTargets returns Scan destinations matching storedClaimColumns, in
// order, so queries that select those columns alongside others can reuse them.
func claimScanTargets(claim *domain.StoredClaim, ticker *pgtype.Text, horizonDays *pgtype.Int4) []any {
	return []any{
		&claim.ID,
		&claim.DocumentID,
		&claim.PageNumber,
		ticker,
		&claim.Kind,
		&claim.Direction,
		&claim.Text,
		&claim.EvidenceQuote,
		horizonDays,
		&claim.Confidence,
		&claim.EffectiveAt,
		&claim.ValidationStatus,
		&claim.CreatedAt,
	}
}

func applyNullableClaimFields(claim *domain.StoredClaim, ticker pgtype.Text, horizonDays pgtype.Int4) {
	if ticker.Valid {
		value := ticker.String
		claim.Ticker = &value
	}
	if horizonDays.Valid {
		value := int(horizonDays.Int32)
		claim.HorizonDays = &value
	}
}
