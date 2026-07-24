package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"signalledger/internal/domain"
)

// These exercise the store against a real PostgreSQL, because its riskiest logic
// — the CompleteBacktest transaction and the latest-run-per-strategy candidate
// query — lives in SQL that unit tests cannot reach. They skip unless
// SIGNALLEDGER_TEST_DATABASE_URL points at a disposable database; CI provides one.
// The named database is truncated on every test, so never point this at real data.

func newTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("SIGNALLEDGER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SIGNALLEDGER_TEST_DATABASE_URL to run store integration tests")
	}

	store, err := New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(store.Close)

	applyMigrations(t, store)
	truncateAll(t, store)
	return store
}

func applyMigrations(t *testing.T, store *Store) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v (found %d)", err, len(files))
	}
	// Glob returns lexical order, which matches the numeric migration prefixes.
	for _, file := range files {
		sql, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read migration %s: %v", file, err)
		}
		if _, err := store.pool.Exec(context.Background(), string(sql)); err != nil {
			t.Fatalf("apply migration %s: %v", filepath.Base(file), err)
		}
	}
}

func truncateAll(t *testing.T, store *Store) {
	t.Helper()
	_, err := store.pool.Exec(context.Background(),
		`TRUNCATE documents, strategies, market_data_snapshots, jobs RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func randomHex(t *testing.T, bytes int) string {
	t.Helper()
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(buffer)
}

// seedAcceptedClaim creates a document, extracts one accepted claim, and returns
// its id — the evidence a strategy and its candidates can cite.
func seedAcceptedClaim(t *testing.T, store *Store, ticker string) string {
	t.Helper()
	ctx := context.Background()

	document, _, _, err := store.CreateDocumentWithExtractionJob(ctx, domain.CreateDocumentInput{
		Filename:   "research.pdf",
		StorageKey: "documents/" + randomHex(t, 16) + ".pdf",
		SHA256:     randomHex(t, 32),
		SizeBytes:  1024,
		MIMEType:   "application/pdf",
	})
	if err != nil {
		t.Fatalf("create document: %v", err)
	}

	tickerValue := ticker
	err = store.ReplaceExtraction(ctx, document.ID, domain.Extraction{
		Pages: []domain.Page{{Number: 1, Content: ticker + " should outperform on resilient demand."}},
		Claims: []domain.Claim{{
			PageNumber:    1,
			Ticker:        &tickerValue,
			Kind:          "fundamental",
			Direction:     "positive",
			Text:          ticker + " should outperform on resilient demand.",
			EvidenceQuote: ticker + " should outperform on resilient demand.",
			Confidence:    0.7,
			EffectiveAt:   time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		}},
	})
	if err != nil {
		t.Fatalf("replace extraction: %v", err)
	}

	claims, err := store.ListClaimsByDocument(ctx, document.ID)
	if err != nil || len(claims) != 1 {
		t.Fatalf("list claims: %v (got %d)", err, len(claims))
	}
	if _, err := store.SetClaimValidationStatus(ctx, claims[0].ID, "accepted"); err != nil {
		t.Fatalf("accept claim: %v", err)
	}
	return claims[0].ID
}

func seedStrategy(t *testing.T, store *Store, claimID string) domain.Strategy {
	t.Helper()
	slug := "strat-" + randomHex(t, 4)
	spec, _ := json.Marshal(map[string]any{
		"slug":    slug,
		"version": 1,
		"name":    "Test strategy",
		"universe": map[string]any{
			"name": "u", "asset_class": "etf", "symbols": []string{"XLE", "USO"},
		},
		"selection": map[string]any{"template": "macro-theme-etf", "filters": []any{}},
		"rebalance": map[string]any{"schedule": "monthly"},
		"risk":      map[string]any{"max_position_weight": 0.2, "transaction_cost_bps": 5},
	})
	strategy, err := store.CreateStrategyWithCitations(context.Background(), domain.CreateStrategyInput{
		Slug:     slug,
		Name:     "Test strategy",
		Spec:     spec,
		ClaimIDs: []string{claimID},
	})
	if err != nil {
		t.Fatalf("create strategy: %v", err)
	}
	return strategy
}

func seedSnapshot(t *testing.T, store *Store) domain.MarketDataSnapshot {
	t.Helper()
	snapshot, err := store.InsertMarketDataSnapshot(context.Background(), domain.CreateMarketDataSnapshotInput{
		Provider:    "test",
		Symbols:     []string{"XLE", "USO"},
		Interval:    "1d",
		StartDate:   "2024-01-02",
		EndDate:     "2026-03-02",
		StorageKey:  "market-data/" + randomHex(t, 16) + ".csv",
		Checksum:    randomHex(t, 32),
		RetrievedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	return snapshot
}

// completeRun creates a queued run, marks it running, and completes it with the
// given candidate set — the full path the worker takes.
func completeRun(t *testing.T, store *Store, strategyID, snapshotID string, candidates domain.CandidateSet, completedAt time.Time) domain.BacktestRun {
	t.Helper()
	ctx := context.Background()

	run, _, err := store.CreateBacktestWithRunJob(ctx, domain.CreateBacktestInput{
		StrategyID:       strategyID,
		SnapshotID:       snapshotID,
		DocumentCutoffAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Parameters:       json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create backtest: %v", err)
	}
	if err := store.SetBacktestStatus(ctx, run.ID, "running"); err != nil {
		t.Fatalf("set running: %v", err)
	}
	err = store.CompleteBacktest(ctx, run.ID, domain.BacktestResult{
		Summary:        json.RawMessage(`{"sharpe":1.2}`),
		EquityCurveCSV: "date,equity\n2026-03-02,1.00000000\n",
		ResultChecksum: randomHex(t, 32),
		EngineVersion:  "momentum-claims-v2",
		Candidates:     candidates,
	}, "backtests/"+randomHex(t, 16)+".csv")
	if err != nil {
		t.Fatalf("complete backtest: %v", err)
	}

	// completed_at drives latest-run selection; pin it so ordering is testable.
	if !completedAt.IsZero() {
		if _, err := store.pool.Exec(ctx, `UPDATE backtest_runs SET completed_at = $2 WHERE id = $1`, run.ID, completedAt); err != nil {
			t.Fatalf("pin completed_at: %v", err)
		}
	}
	return run
}

func candidateSet(claimID string) domain.CandidateSet {
	return domain.CandidateSet{
		AsOf: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		Positions: []domain.CandidatePosition{
			{Symbol: "XLE", Rank: 1, Weight: 0.2, Score: 0.31, Momentum: 0.24, ClaimSupport: 0.7,
				Evidence: []domain.CandidateEvidence{{ClaimID: claimID, Contribution: 0.7}}},
			{Symbol: "USO", Rank: 2, Weight: 0.2, Score: 0.12, Momentum: 0.12, ClaimSupport: 0.0},
		},
	}
}

func TestCompleteBacktestPersistsCandidatesAtomically(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	claimID := seedAcceptedClaim(t, store, "XLE")
	strategy := seedStrategy(t, store, claimID)
	snapshot := seedSnapshot(t, store)
	run := completeRun(t, store, strategy.ID, snapshot.ID, candidateSet(claimID), time.Time{})

	// The run is completed with its reproducibility pointers.
	stored, err := store.GetBacktestRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.Status != "completed" || stored.EngineVersion == nil || *stored.EngineVersion != "momentum-claims-v2" {
		t.Fatalf("run not completed as expected: %+v", stored)
	}
	if stored.ResultChecksum == nil || stored.ResultArtifactKey == nil {
		t.Fatalf("run is missing artifact pointers: %+v", stored)
	}

	candidates, err := store.ListCandidates(ctx, domain.CandidateFilter{})
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	first := candidates[0]
	if first.Symbol != "XLE" || first.Rank != 1 || first.StrategySlug != strategy.Slug {
		t.Fatalf("first candidate = %+v", first)
	}
	if first.EngineVersion == nil || *first.EngineVersion != "momentum-claims-v2" {
		t.Fatalf("candidate lost its run's engine version: %+v", first)
	}
	// The whole point: the position resolves to the page-cited claim.
	if len(first.Evidence) != 1 || first.Evidence[0].Claim.ID != claimID || first.Evidence[0].Contribution != 0.7 {
		t.Fatalf("evidence = %+v", first.Evidence)
	}
	if first.Evidence[0].Claim.PageNumber != 1 {
		t.Fatalf("evidence lost its page citation: %+v", first.Evidence[0].Claim)
	}
	if len(candidates[1].Evidence) != 0 {
		t.Fatalf("second candidate should have no evidence: %+v", candidates[1].Evidence)
	}
}

func TestCompleteBacktestOnlyAdvancesInProgressRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	claimID := seedAcceptedClaim(t, store, "XLE")
	strategy := seedStrategy(t, store, claimID)
	snapshot := seedSnapshot(t, store)
	run := completeRun(t, store, strategy.ID, snapshot.ID, candidateSet(claimID), time.Time{})

	// A retried job that finds the run already completed must not double-write.
	err := store.CompleteBacktest(ctx, run.ID, domain.BacktestResult{
		Summary:        json.RawMessage(`{}`),
		EquityCurveCSV: "date,equity\n",
		ResultChecksum: randomHex(t, 32),
		EngineVersion:  "momentum-claims-v2",
		Candidates:     candidateSet(claimID),
	}, "backtests/dupe.csv")
	if err == nil {
		t.Fatal("expected completing an already-completed run to fail")
	}

	candidates, err := store.ListCandidates(ctx, domain.CandidateFilter{})
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("a rejected re-complete changed the candidates: got %d", len(candidates))
	}
}

func TestCompleteBacktestRejectsUnrankedCandidates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	claimID := seedAcceptedClaim(t, store, "XLE")
	strategy := seedStrategy(t, store, claimID)
	snapshot := seedSnapshot(t, store)

	run, _, err := store.CreateBacktestWithRunJob(ctx, domain.CreateBacktestInput{
		StrategyID:       strategy.ID,
		SnapshotID:       snapshot.ID,
		DocumentCutoffAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Parameters:       json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create backtest: %v", err)
	}

	bad := domain.CandidateSet{
		AsOf:      time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		Positions: []domain.CandidatePosition{{Symbol: "XLE", Rank: 2, Weight: 0.2}},
	}
	if err := store.CompleteBacktest(ctx, run.ID, domain.BacktestResult{
		Summary: json.RawMessage(`{}`), EquityCurveCSV: "x", ResultChecksum: randomHex(t, 32),
		EngineVersion: "momentum-claims-v2", Candidates: bad,
	}, "backtests/x.csv"); err == nil {
		t.Fatal("expected non-dense ranks to be rejected")
	}

	// The rejection must leave nothing behind — the run stays runnable.
	stored, err := store.GetBacktestRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.Status == "completed" {
		t.Fatalf("run was marked completed despite invalid candidates")
	}
	candidates, err := store.ListCandidates(ctx, domain.CandidateFilter{})
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("invalid candidates leaked: got %d", len(candidates))
	}
}

func TestListCandidatesServesLatestRunPerStrategy(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	claimID := seedAcceptedClaim(t, store, "XLE")
	strategy := seedStrategy(t, store, claimID)
	snapshot := seedSnapshot(t, store)

	now := time.Now().UTC()
	older := completeRun(t, store, strategy.ID, snapshot.ID, candidateSet(claimID), now.Add(-48*time.Hour))
	newer := completeRun(t, store, strategy.ID, snapshot.ID, candidateSet(claimID), now)

	// Default: the strategy's latest completed run only.
	latest, err := store.ListCandidates(ctx, domain.CandidateFilter{})
	if err != nil {
		t.Fatalf("list latest: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("expected 2 candidates from one run, got %d", len(latest))
	}
	for _, candidate := range latest {
		if candidate.BacktestRunID != newer.ID {
			t.Fatalf("served a stale run %s, expected %s", candidate.BacktestRunID, newer.ID)
		}
	}

	// backtest_id pins an exact historical run.
	pinned, err := store.ListCandidates(ctx, domain.CandidateFilter{BacktestID: older.ID})
	if err != nil {
		t.Fatalf("list pinned: %v", err)
	}
	if len(pinned) != 2 || pinned[0].BacktestRunID != older.ID {
		t.Fatalf("backtest_id did not pin the older run: %+v", pinned)
	}
}

func TestListCandidatesFiltersAndLimits(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	snapshot := seedSnapshot(t, store)
	claimA := seedAcceptedClaim(t, store, "XLE")
	strategyA := seedStrategy(t, store, claimA)
	completeRun(t, store, strategyA.ID, snapshot.ID, candidateSet(claimA), time.Now().UTC())

	claimB := seedAcceptedClaim(t, store, "USO")
	strategyB := seedStrategy(t, store, claimB)
	completeRun(t, store, strategyB.ID, snapshot.ID, candidateSet(claimB), time.Now().UTC())

	// No filter spans both strategies.
	all, err := store.ListCandidates(ctx, domain.CandidateFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 candidates across 2 strategies, got %d", len(all))
	}

	// strategy_id narrows to one.
	oneStrategy, err := store.ListCandidates(ctx, domain.CandidateFilter{StrategyID: strategyA.ID})
	if err != nil {
		t.Fatalf("list one strategy: %v", err)
	}
	if len(oneStrategy) != 2 {
		t.Fatalf("expected 2 candidates for one strategy, got %d", len(oneStrategy))
	}
	for _, candidate := range oneStrategy {
		if candidate.StrategyID != strategyA.ID {
			t.Fatalf("filter leaked strategy %s", candidate.StrategyID)
		}
	}

	// limit caps the result.
	limited, err := store.ListCandidates(ctx, domain.CandidateFilter{Limit: 1})
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit not applied: got %d", len(limited))
	}
}

func TestListCandidatesIgnoresIncompleteRuns(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	claimID := seedAcceptedClaim(t, store, "XLE")
	strategy := seedStrategy(t, store, claimID)
	snapshot := seedSnapshot(t, store)

	// A queued run produces no candidates and must not be served.
	if _, _, err := store.CreateBacktestWithRunJob(ctx, domain.CreateBacktestInput{
		StrategyID:       strategy.ID,
		SnapshotID:       snapshot.ID,
		DocumentCutoffAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Parameters:       json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("create backtest: %v", err)
	}

	candidates, err := store.ListCandidates(ctx, domain.CandidateFilter{})
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("a run with no completed backtest produced candidates: %d", len(candidates))
	}
}
