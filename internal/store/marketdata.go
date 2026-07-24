package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"signalledger/internal/domain"
)

func (store *Store) CreateMarketDataJob(ctx context.Context, request domain.MarketDataRequest) (domain.Job, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return domain.Job{}, fmt.Errorf("encode market data job payload: %w", err)
	}

	var job domain.Job
	var rawPayload string
	err = store.pool.QueryRow(ctx, `
		INSERT INTO jobs (job_type, payload)
		VALUES ('fetch_market_data', $1::jsonb)
		RETURNING id::text, job_type, payload::text, attempts, max_attempts`,
		string(payload),
	).Scan(&job.ID, &job.Type, &rawPayload, &job.Attempts, &job.MaxAttempts)
	if err != nil {
		return domain.Job{}, fmt.Errorf("insert market data job: %w", err)
	}
	job.Payload = json.RawMessage(rawPayload)
	return job, nil
}

func (store *Store) InsertMarketDataSnapshot(ctx context.Context, input domain.CreateMarketDataSnapshotInput) (domain.MarketDataSnapshot, error) {
	metadata, err := json.Marshal(map[string]any{"bar_interval": input.Interval})
	if err != nil {
		return domain.MarketDataSnapshot{}, fmt.Errorf("encode snapshot metadata: %w", err)
	}

	snapshot, err := scanMarketDataSnapshot(store.pool.QueryRow(ctx, `
		INSERT INTO market_data_snapshots (
			provider, symbols, interval, start_date, end_date,
			storage_key, checksum, metadata, retrieved_at
		) VALUES ($1, $2, $3, $4::date, $5::date, $6, $7, $8::jsonb, $9)
		RETURNING id::text, provider, symbols, interval,
			start_date::text, end_date::text, storage_key, checksum, retrieved_at`,
		input.Provider,
		input.Symbols,
		input.Interval,
		input.StartDate,
		input.EndDate,
		input.StorageKey,
		input.Checksum,
		string(metadata),
		input.RetrievedAt,
	))
	if err != nil {
		return domain.MarketDataSnapshot{}, fmt.Errorf("insert market data snapshot: %w", err)
	}
	return snapshot, nil
}

func (store *Store) GetMarketDataSnapshot(ctx context.Context, id string) (domain.MarketDataSnapshot, error) {
	snapshot, err := scanMarketDataSnapshot(store.pool.QueryRow(ctx, `
		SELECT id::text, provider, symbols, interval,
			start_date::text, end_date::text, storage_key, checksum, retrieved_at
		FROM market_data_snapshots
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.MarketDataSnapshot{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.MarketDataSnapshot{}, fmt.Errorf("get market data snapshot: %w", err)
	}
	return snapshot, nil
}

func (store *Store) ListMarketDataSnapshots(ctx context.Context) ([]domain.MarketDataSnapshot, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id::text, provider, symbols, interval,
			start_date::text, end_date::text, storage_key, checksum, retrieved_at
		FROM market_data_snapshots
		ORDER BY retrieved_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list market data snapshots: %w", err)
	}
	defer rows.Close()

	snapshots := []domain.MarketDataSnapshot{}
	for rows.Next() {
		snapshot, err := scanMarketDataSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("scan market data snapshot: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate market data snapshots: %w", err)
	}
	return snapshots, nil
}

func scanMarketDataSnapshot(row pgx.Row) (domain.MarketDataSnapshot, error) {
	var snapshot domain.MarketDataSnapshot
	var storageKey, checksum pgtype.Text
	err := row.Scan(
		&snapshot.ID,
		&snapshot.Provider,
		&snapshot.Symbols,
		&snapshot.Interval,
		&snapshot.StartDate,
		&snapshot.EndDate,
		&storageKey,
		&checksum,
		&snapshot.RetrievedAt,
	)
	if err != nil {
		return domain.MarketDataSnapshot{}, err
	}
	if storageKey.Valid {
		value := storageKey.String
		snapshot.StorageKey = &value
	}
	if checksum.Valid {
		value := checksum.String
		snapshot.Checksum = &value
	}
	return snapshot, nil
}
