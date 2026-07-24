package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"signalledger/internal/domain"
)

// CreateStrategyWithCitations assigns the next version for the slug and stores
// the spec plus its claim citations in one transaction. An advisory lock keyed
// on the slug serializes concurrent submissions so versions stay gapless.
func (store *Store) CreateStrategyWithCitations(ctx context.Context, input domain.CreateStrategyInput) (domain.Strategy, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Strategy{}, fmt.Errorf("begin strategy transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('strategy:' || $1))`, input.Slug); err != nil {
		return domain.Strategy{}, fmt.Errorf("lock strategy slug: %w", err)
	}

	var accepted int
	err = tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM research_claims
		WHERE id = ANY($1::uuid[]) AND validation_status = 'accepted'`,
		input.ClaimIDs,
	).Scan(&accepted)
	if err != nil {
		return domain.Strategy{}, fmt.Errorf("verify cited claims: %w", err)
	}
	if accepted != len(input.ClaimIDs) {
		return domain.Strategy{}, domain.ErrClaimNotCitable
	}

	var strategy domain.Strategy
	var rawSpec string
	err = tx.QueryRow(ctx, `
		INSERT INTO strategies (slug, version, name, spec)
		SELECT $1,
		       COALESCE(MAX(version), 0) + 1,
		       $2,
		       jsonb_set($3::jsonb, '{version}', to_jsonb(COALESCE(MAX(version), 0) + 1))
		FROM strategies
		WHERE slug = $1
		RETURNING id::text, slug, version, name, spec::text, created_at`,
		input.Slug,
		input.Name,
		string(input.Spec),
	).Scan(&strategy.ID, &strategy.Slug, &strategy.Version, &strategy.Name, &rawSpec, &strategy.CreatedAt)
	if err != nil {
		return domain.Strategy{}, fmt.Errorf("insert strategy: %w", err)
	}
	strategy.Spec = []byte(rawSpec)

	if _, err := tx.Exec(ctx, `
		INSERT INTO strategy_claims (strategy_id, claim_id)
		SELECT $1::uuid, unnest($2::uuid[])`,
		strategy.ID,
		input.ClaimIDs,
	); err != nil {
		return domain.Strategy{}, fmt.Errorf("insert strategy citations: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Strategy{}, fmt.Errorf("commit strategy: %w", err)
	}
	return strategy, nil
}

func (store *Store) ListStrategies(ctx context.Context) ([]domain.Strategy, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id::text, slug, version, name, spec::text, created_at
		FROM strategies
		ORDER BY slug, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("list strategies: %w", err)
	}
	defer rows.Close()

	strategies := []domain.Strategy{}
	for rows.Next() {
		strategy, err := scanStrategy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan strategy: %w", err)
		}
		strategies = append(strategies, strategy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate strategies: %w", err)
	}
	return strategies, nil
}

func (store *Store) GetStrategyWithCitations(ctx context.Context, id string) (domain.Strategy, []domain.StoredClaim, error) {
	strategy, err := scanStrategy(store.pool.QueryRow(ctx, `
		SELECT id::text, slug, version, name, spec::text, created_at
		FROM strategies
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Strategy{}, nil, domain.ErrNotFound
	}
	if err != nil {
		return domain.Strategy{}, nil, fmt.Errorf("get strategy: %w", err)
	}

	rows, err := store.pool.Query(ctx, `
		SELECT `+storedClaimColumns+`
		FROM strategy_claims
		JOIN research_claims ON research_claims.id = strategy_claims.claim_id
		JOIN document_pages ON document_pages.id = research_claims.page_id
		WHERE strategy_claims.strategy_id = $1
		ORDER BY research_claims.effective_at, research_claims.id`,
		id,
	)
	if err != nil {
		return domain.Strategy{}, nil, fmt.Errorf("list strategy citations: %w", err)
	}
	defer rows.Close()

	citations, err := scanStoredClaims(rows)
	if err != nil {
		return domain.Strategy{}, nil, err
	}
	return strategy, citations, nil
}

func scanStrategy(row pgx.Row) (domain.Strategy, error) {
	var strategy domain.Strategy
	var rawSpec string
	err := row.Scan(
		&strategy.ID,
		&strategy.Slug,
		&strategy.Version,
		&strategy.Name,
		&rawSpec,
		&strategy.CreatedAt,
	)
	if err != nil {
		return domain.Strategy{}, err
	}
	strategy.Spec = []byte(rawSpec)
	return strategy, nil
}
