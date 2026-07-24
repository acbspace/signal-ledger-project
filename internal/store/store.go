package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"signalledger/internal/domain"
)

const maxStoredErrorLength = 2_000

// Store is the sole writer for SignalLedger's PostgreSQL state.
type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, databaseURL string) (*Store, error) {
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	configuration.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}

	pingContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingContext); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	return &Store{pool: pool}, nil
}

func (store *Store) Close() {
	store.pool.Close()
}

func (store *Store) Ping(ctx context.Context) error {
	return store.pool.Ping(ctx)
}

func (store *Store) CreateDocumentWithExtractionJob(ctx context.Context, input domain.CreateDocumentInput) (domain.Document, *domain.Job, bool, error) {
	metadata, err := json.Marshal(map[string]any{
		"mime_type":  input.MIMEType,
		"size_bytes": input.SizeBytes,
	})
	if err != nil {
		return domain.Document{}, nil, false, fmt.Errorf("encode document metadata: %w", err)
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Document{}, nil, false, fmt.Errorf("begin document transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	publishedAt := nullableTime(input.SourcePublishedAt)
	document, err := scanDocument(tx.QueryRow(ctx, `
		INSERT INTO documents (filename, storage_key, sha256, source_published_at, metadata)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		ON CONFLICT (sha256) DO NOTHING
		RETURNING id::text, filename, storage_key, sha256, status, uploaded_at, source_published_at`,
		input.Filename,
		input.StorageKey,
		input.SHA256,
		publishedAt,
		string(metadata),
	))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.Document{}, nil, false, fmt.Errorf("insert document: %w", err)
		}

		existing, existingErr := scanDocument(tx.QueryRow(ctx, `
			SELECT id::text, filename, storage_key, sha256, status, uploaded_at, source_published_at
			FROM documents
			WHERE sha256 = $1`, input.SHA256))
		if existingErr != nil {
			return domain.Document{}, nil, false, fmt.Errorf("load duplicate document: %w", existingErr)
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Document{}, nil, false, fmt.Errorf("commit duplicate document: %w", err)
		}
		return existing, nil, false, nil
	}

	payload, err := json.Marshal(map[string]string{"document_id": document.ID})
	if err != nil {
		return domain.Document{}, nil, false, fmt.Errorf("encode extraction job: %w", err)
	}
	var job domain.Job
	var rawPayload string
	err = tx.QueryRow(ctx, `
		INSERT INTO jobs (job_type, payload)
		VALUES ('extract_document', $1::jsonb)
		RETURNING id::text, job_type, payload::text, attempts, max_attempts`, string(payload)).Scan(
		&job.ID,
		&job.Type,
		&rawPayload,
		&job.Attempts,
		&job.MaxAttempts,
	)
	if err != nil {
		return domain.Document{}, nil, false, fmt.Errorf("insert extraction job: %w", err)
	}
	job.Payload = json.RawMessage(rawPayload)

	if err := tx.Commit(ctx); err != nil {
		return domain.Document{}, nil, false, fmt.Errorf("commit document transaction: %w", err)
	}
	return document, &job, true, nil
}

func (store *Store) GetDocument(ctx context.Context, id string) (domain.Document, error) {
	document, err := scanDocument(store.pool.QueryRow(ctx, `
		SELECT id::text, filename, storage_key, sha256, status, uploaded_at, source_published_at
		FROM documents
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Document{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Document{}, fmt.Errorf("get document: %w", err)
	}
	return document, nil
}

func (store *Store) LeaseNextJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*domain.Job, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin job lease transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	leaseUntil := time.Now().UTC().Add(leaseDuration)
	var job domain.Job
	var rawPayload string
	err = tx.QueryRow(ctx, `
		WITH next_job AS (
			SELECT id
			FROM jobs
			WHERE (state IN ('queued', 'retryable') AND available_at <= now())
			   OR (state = 'leased' AND leased_until <= now())
			ORDER BY available_at, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE jobs
		SET state = 'leased',
			attempts = attempts + 1,
			leased_until = $2,
			leased_by = $1,
			updated_at = now()
		FROM next_job
		WHERE jobs.id = next_job.id
		RETURNING jobs.id::text, jobs.job_type, jobs.payload::text, jobs.attempts, jobs.max_attempts`,
		workerID,
		leaseUntil,
	).Scan(&job.ID, &job.Type, &rawPayload, &job.Attempts, &job.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty job lease: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lease next job: %w", err)
	}
	job.Payload = json.RawMessage(rawPayload)

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit job lease: %w", err)
	}
	return &job, nil
}

func (store *Store) CompleteJob(ctx context.Context, id string) error {
	command, err := store.pool.Exec(ctx, `
		UPDATE jobs
		SET state = 'completed', leased_until = NULL, leased_by = NULL, updated_at = now()
		WHERE id = $1 AND state = 'leased'`, id)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("complete job: job is not leased")
	}
	return nil
}

// FailJob releases the lease and returns true when the job has exhausted its retry budget.
func (store *Store) FailJob(ctx context.Context, job domain.Job, cause error) (bool, error) {
	terminal := job.Attempts >= job.MaxAttempts
	state := "retryable"
	availableAt := time.Now().UTC().Add(retryDelay(job.Attempts))
	if terminal {
		state = "failed"
		availableAt = time.Now().UTC()
	}

	message := cause.Error()
	if len(message) > maxStoredErrorLength {
		message = message[:maxStoredErrorLength]
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE jobs
		SET state = $2,
			available_at = $3,
			leased_until = NULL,
			leased_by = NULL,
			last_error = $4,
			updated_at = now()
		WHERE id = $1 AND state = 'leased'`,
		job.ID,
		state,
		availableAt,
		message,
	)
	if err != nil {
		return false, fmt.Errorf("fail job: %w", err)
	}
	if command.RowsAffected() != 1 {
		return false, fmt.Errorf("fail job: job is not leased")
	}
	return terminal, nil
}

func (store *Store) SetDocumentStatus(ctx context.Context, documentID, status string) error {
	switch status {
	case "queued", "processing", "ready", "failed":
	default:
		return fmt.Errorf("invalid document status %q", status)
	}

	command, err := store.pool.Exec(ctx, `UPDATE documents SET status = $2 WHERE id = $1`, documentID, status)
	if err != nil {
		return fmt.Errorf("set document status: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

// ReplaceExtraction atomically replaces all extracted pages and claims for a document.
// Keeping this idempotent makes a retried worker safe after a partial failure.
func (store *Store) ReplaceExtraction(ctx context.Context, documentID string, extraction domain.Extraction) error {
	if err := extraction.Validate(); err != nil {
		return fmt.Errorf("validate extraction: %w", err)
	}

	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin extraction transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM document_pages WHERE document_id = $1`, documentID); err != nil {
		return fmt.Errorf("clear extracted pages: %w", err)
	}

	pageIDs := make(map[int]string, len(extraction.Pages))
	for _, page := range extraction.Pages {
		var pageID string
		err := tx.QueryRow(ctx, `
			INSERT INTO document_pages (document_id, page_number, content)
			VALUES ($1, $2, $3)
			RETURNING id::text`, documentID, page.Number, page.Content).Scan(&pageID)
		if err != nil {
			return fmt.Errorf("store page %d: %w", page.Number, err)
		}
		pageIDs[page.Number] = pageID
	}

	for _, claim := range extraction.Claims {
		_, err := tx.Exec(ctx, `
			INSERT INTO research_claims (
				document_id, page_id, ticker, claim_kind, direction, claim,
				evidence_quote, horizon_days, confidence, effective_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			documentID,
			pageIDs[claim.PageNumber],
			nullableString(claim.Ticker),
			claim.Kind,
			claim.Direction,
			claim.Text,
			claim.EvidenceQuote,
			nullableInt(claim.HorizonDays),
			claim.Confidence,
			claim.EffectiveAt,
		)
		if err != nil {
			return fmt.Errorf("store claim for page %d: %w", claim.PageNumber, err)
		}
	}

	command, err := tx.Exec(ctx, `UPDATE documents SET status = 'ready' WHERE id = $1`, documentID)
	if err != nil {
		return fmt.Errorf("mark document ready: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.ErrNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit extraction: %w", err)
	}
	return nil
}

// insertJob enqueues a job inside an existing transaction so a resource and its
// job are created atomically. Callers commit the surrounding transaction.
func insertJob(ctx context.Context, tx pgx.Tx, jobType string, payload []byte) (*domain.Job, error) {
	var job domain.Job
	var rawPayload string
	err := tx.QueryRow(ctx, `
		INSERT INTO jobs (job_type, payload)
		VALUES ($1, $2::jsonb)
		RETURNING id::text, job_type, payload::text, attempts, max_attempts`,
		jobType, string(payload),
	).Scan(&job.ID, &job.Type, &rawPayload, &job.Attempts, &job.MaxAttempts)
	if err != nil {
		return nil, fmt.Errorf("insert %s job: %w", jobType, err)
	}
	job.Payload = json.RawMessage(rawPayload)
	return &job, nil
}

func scanDocument(row pgx.Row) (domain.Document, error) {
	var document domain.Document
	var publishedAt pgtype.Timestamptz
	err := row.Scan(
		&document.ID,
		&document.Filename,
		&document.StorageKey,
		&document.SHA256,
		&document.Status,
		&document.UploadedAt,
		&publishedAt,
	)
	if err != nil {
		return domain.Document{}, err
	}
	if publishedAt.Valid {
		value := publishedAt.Time
		document.SourcePublishedAt = &value
	}
	return document, nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		return time.Second
	}
	exponent := attempts - 1
	if exponent > 6 {
		exponent = 6
	}
	return time.Duration(1<<exponent) * time.Second
}
