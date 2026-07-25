# Architecture

SignalLedger is intentionally split into two services with one owner for durable state.

```text
Browser / API client
        |
        v
  Go API + Go worker ---- HTTP ----> Python quant service
        |
        v
PostgreSQL + pgvector + document volume
```

## Ownership

- Go owns HTTP APIs, job leasing, persistence, document metadata, strategy versions,
  run history, and user-facing results.
- Python owns stateless PDF extraction, OpenBB calls, feature calculations, and
  deterministic backtest execution.
- PostgreSQL is the source of truth. Python never writes directly to it.

## Reproducibility rules

Every backtest must record:

1. The immutable strategy version.
2. The document cutoff timestamp used to prevent look-ahead bias.
3. The market-data provider, symbols, date range, retrieval time, and checksum.
4. Transaction-cost and risk parameters.

## Research-to-strategy flow

1. `POST /v1/documents` stores the PDF and enqueues `extract_document`.
2. The worker calls the Python service; claims pass a verbatim-evidence gate
   (LLM adapter) or the keyword heuristic, then land as `pending`.
3. A human reviews claims via `PATCH /v1/claims/{id}` (accept/reject).
4. `POST /v1/strategies/draft` deterministically proposes a spec from accepted
   claims — ticker claims become an equity momentum universe; macro claims net
   per theme into long-only ETF proxies (rates→IEF/TLT, oil→USO/XLE, USD→UUP,
   credit→LQD/HYG, equities→SPY). Nothing is persisted.
5. `POST /v1/strategies` validates the spec, verifies every cited claim is
   accepted, and commits the next immutable `(slug, version)` with citations in
   `strategy_claims`.

## Market data snapshots

`POST /v1/market-data/snapshots` enqueues `fetch_market_data`. The worker asks
Python for normalized daily bars (yfinance), writes a canonical sorted CSV to
the shared volume under `market-data/`, and records the snapshot row with a
SHA-256 checksum. Backtests consume snapshots by storage key exactly like
documents.

## Backtests

`POST /v1/backtests` validates that the strategy exists, that the snapshot is
ready (it has a storage key and checksum), and that the snapshot's symbols cover
the strategy's universe — a gap is a `409` naming the missing symbols rather than
a job that fails later in the worker. It then commits a queued `backtest_runs` row
and its `run_backtest` job in one transaction.

The worker loads the immutable spec and the snapshot pointer and calls Python with
everything needed — the service stays stateless and reads the frozen CSV read-only
by storage key. Before simulating it re-hashes the file and refuses to run if the
checksum does not match, so an altered snapshot can never silently change results.

The engine (`momentum-claims-v3`) ranks the universe by trailing-return momentum
and tilts that rank by research. The universe is `spec.universe.symbols` and
nothing else: a snapshot may carry more series than one strategy trades, and
anything beyond the committed universe is ignored rather than ranked, while a
universe symbol the snapshot cannot price is an error instead of a silent subset
(see [ADR 0007](decisions/0007-the-committed-universe-is-the-tradable-set.md)).
Every rebalance uses only bars at or before that day, so a future bar can never
change a past holding; ties break on symbol so ordering is total. Equal weights are capped by `max_position_weight`, turnover is
charged `transaction_cost_bps`, and an optional `stop_loss_pct` exits a position
to cash until the next rebalance.

Python returns scalar metrics plus the equity curve as CSV, checksummed over
exactly the bytes the worker then writes under `backtests/`. So `result_checksum`
is both the determinism proof and the stored artifact's hash: an identical run
reproduces an identical checksum. `engine_version` must change whenever the
simulation math changes.

## Research claims as point-in-time signals

The worker projects the strategy's cited claims onto the symbols its spec
trades — the claim's ticker, or the macro-theme ETF proxies used for drafting —
and sends them with the backtest request. Claims that a human has not accepted,
that resolve outside the universe, or that were effective after the run's
`document_cutoff_at` never leave Go.

The engine applies a second, independent gate at each rebalance: a signal counts
only from its `effective_at` until its horizon expires. A claim published between
two rebalances therefore moves the later one and cannot reach back to the earlier
one. Per symbol, net signed confidence is clamped to `[-1, 1]` and the ranking
score becomes `momentum + claim_signal_weight * claim_score`. Both sides sort
signals on a total key, so accumulation order — and the resulting checksum —
cannot drift.

Selection filters (`claim_confidence`, `claim_signal_weight`,
`claim_horizon_days`, `require_claim_support`) are a closed set validated in Go
and in the JSON contract, because a filter the engine does not read would
silently do nothing. See [ADR 0005](decisions/0005-research-claims-as-point-in-time-signals.md).

## Candidates and the paper portfolio

A candidate is never computed on request. A completed run's last rebalance *is*
the paper portfolio it proposes, so the engine returns that rebalance ranked and
attributed, and `CompleteBacktest` writes `portfolio_candidates` and
`candidate_claims` in the same transaction that completes the run. A ranking
therefore cannot exist without the run that proves it, and deleting a run
cascades its ranking away.

`GET /v1/candidates` serves the latest completed run of every strategy, narrowed
by `strategy_id` or pinned to an exact run by `backtest_id`. Each position
carries its run's reproducibility inputs and the page-cited claims behind it, so
"why do you hold this?" resolves to a page number and a quoted sentence. See
[ADR 0006](decisions/0006-candidates-are-a-projection-of-a-completed-run.md).

## Background work

The `jobs` table is the initial queue. The worker claims eligible rows with a
lease, executes the task (`extract_document`, `fetch_market_data`,
`run_backtest`), and retries transient errors with backoff. This keeps v1 simple
while providing a clear upgrade path to a separate broker if scale requires it.
