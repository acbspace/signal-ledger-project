# SignalLedger

## Overview

SignalLedger is a personal research-to-portfolio platform. It ingests sell-side
research PDFs, extracts page-cited claims, maps them to explicit strategy
templates, retrieves reproducible market-data snapshots, and runs deterministic
paper backtests in which accepted claims act as point-in-time signals. Every
proposed position traces back to the exact page and sentence that justifies it.

The planned pipeline is complete end to end: upload a PDF → extract page-cited
claims → review (accept/reject) → draft and commit an immutable strategy version
→ snapshot the market data it needs → backtest it → read the ranked candidates it
proposes. Sample GS/BofA research PDFs live in
[`samples/research/`](samples/research/).

## Repository Structure

```
.
├── cmd
│   ├── api                 # Go HTTP API entrypoint
│   └── worker              # durable job worker entrypoint
├── internal
│   ├── backtests           # run-artifact storage
│   ├── config
│   ├── documents           # upload, dedup, metadata
│   ├── domain              # core types and validation
│   ├── httpapi             # request handlers
│   ├── jobs                # lease / execute / retry loop
│   ├── marketdata          # snapshot service and storage
│   ├── quant               # client for the Python quant service
│   ├── store               # PostgreSQL persistence
│   └── strategies          # drafts, specs, claim signals
├── python
│   ├── quant_service       # FastAPI: PDF, market data, backtest
│   └── tests
├── contracts               # cross-service JSON schemas
├── migrations              # PostgreSQL schema
├── docs
│   ├── decisions           # architecture decision records
│   └── roadmap.md          # planned work
├── samples/research        # sample research PDFs
├── scripts                 # end-to-end smoke test
├── compose.yaml
└── Dockerfile
```

## Technology Stack

| Component          | Technologies                                        |
| ------------------ | --------------------------------------------------- |
| API & worker       | Go, `net/http`, pgx                                 |
| Quant service      | Python, FastAPI, Polars, PyMuPDF                    |
| Persistence        | PostgreSQL (pgvector-ready)                          |
| Market data        | yfinance (OpenBB adapter planned)                   |
| Claim extraction   | Keyword heuristic, Anthropic LLM adapter (optional) |
| Local development   | Docker Compose                                       |

## Methodology

### Architecture

The system is split into two services with a single owner for durable state.

```text
Browser / API client
        |
        v
  Go API + Go worker ---- HTTP ----> Python quant service
        |
        v
PostgreSQL + pgvector + document volume
```

- **Go** owns HTTP APIs, job leasing, persistence, document metadata, strategy
  versions, run history, and user-facing results.
- **Python** owns stateless PDF extraction, market-data retrieval, and
  deterministic backtest execution — it never writes to the database.
- **PostgreSQL** is the single source of truth.

Durable work runs through a `jobs` table: the worker leases an eligible row,
executes the task (`extract_document`, `fetch_market_data`, `run_backtest`), and
retries transient failures with backoff. This keeps v1 simple while leaving a
clear upgrade path to a dedicated broker.

### Pipeline

1. `POST /v1/documents` stores the PDF and enqueues extraction.
2. The worker calls Python; claims pass a verbatim-evidence gate (LLM adapter) or
   a prose-filtered keyword heuristic, then land as `pending`.
3. A human accepts or rejects each claim via `PATCH /v1/claims/{id}`.
4. `POST /v1/strategies/draft` deterministically proposes a spec — ticker claims
   become an equity momentum universe; macro claims net per theme into long-only
   ETF proxies. Nothing is persisted.
5. `POST /v1/strategies` validates the spec, verifies every cited claim is
   accepted, and commits the next immutable `(slug, version)` with citations.
6. `POST /v1/market-data/snapshots` freezes the required daily bars to a
   checksummed CSV on the shared volume.
7. `POST /v1/backtests` runs the frozen snapshot through the momentum engine,
   tilted by the strategy's cited claims as point-in-time signals. The engine
   trades the committed `universe.symbols` and nothing else, so a shared snapshot
   cannot smuggle an unauthorized symbol into a strategy's portfolio.
8. `GET /v1/candidates` returns the ranked paper portfolio the run proposed, each
   position carrying its page-cited evidence.

### Reproducibility and no look-ahead

Reproducibility is enforced rather than assumed. Every backtest pins its
immutable strategy version, the document cutoff, the snapshot's SHA-256, and the
engine version; the engine re-hashes the frozen snapshot and refuses to run on
altered bytes. It returns the equity curve as the exact bytes its result checksum
is taken over, so an identical run reproduces an identical checksum.

The engine version names the simulation math, not the environment that ran it, so
each summary also records `python_version` and `polars_version` — floating-point
accumulation can move between library releases under unchanged math. Those
versions are fixed by a hashed lock (see [Dependencies](#dependencies)), and the
image refuses to install anything the lock does not pin.

Two independent point-in-time rules hold. A price decision at a rebalance uses
only bars at or before that day. A research claim counts only from its
`effective_at` until its horizon expires, and the worker additionally drops any
claim effective after the run's cutoff — so future research can never reach a past
decision. Signals are sorted on a total key before the engine sees them, keeping
floating-point accumulation order deterministic.

## HTTP API

| Service      | Endpoint                                                      | Behavior                                                       |
| ------------ | ------------------------------------------------------------ | ------------------------------------------------------------- |
| Go API       | `GET /healthz`                                               | Ready                                                          |
| Go API       | `POST /v1/documents`, `GET /v1/documents/{id}`               | Upload with dedup + async extraction                          |
| Go API       | `GET /v1/documents/{id}/claims`                             | List extracted claims                                          |
| Go API       | `PATCH /v1/claims/{id}`                                      | Accept / reject a claim                                        |
| Go API       | `POST /v1/strategies/draft`                                 | Stateless draft from accepted claims                          |
| Go API       | `POST /v1/strategies`, `GET /v1/strategies[/{id}]`          | Immutable versions with citations                             |
| Go API       | `POST /v1/market-data/snapshots`, `GET /…/{id}`             | Durable, checksummed snapshot jobs                            |
| Go API       | `POST /v1/backtests`, `GET /v1/backtests/{id}`             | Deterministic runs against a frozen snapshot                  |
| Go API       | `GET /v1/candidates`                                        | Ranked positions with page-cited evidence                     |
| Python quant | `GET /healthz`                                              | Ready                                                          |
| Python quant | `POST /v1/extract-claims`                                   | Heuristic or Anthropic adapter                                |
| Python quant | `POST /v1/market-data`                                      | Normalized yfinance daily bars                                |
| Python quant | `POST /v1/backtests`                                        | Momentum engine tilted by point-in-time claims                |

## Research claims in backtests

A backtest tilts its momentum ranking by the strategy's own cited claims. The
worker resolves each accepted claim to a symbol (its ticker, or macro-theme ETF
proxies), drops anything outside the universe or effective after the run's
`document_cutoff_at`, and the engine counts a claim only from its `effective_at`
until its horizon expires — so a claim published mid-run moves later rebalances
and never earlier ones.

The ranking is over each symbol's cross-sectional *rank* of trailing return,
spread evenly over [-1, 1], not the return itself. Adding a confidence to a
return would make a claim's real influence depend on how violent the universe
happened to be; in rank space `claim_signal_weight` means one thing everywhere —
the fraction of the momentum spread a full-confidence claim is worth. Momentum
gates still read the raw return, because a committed `momentum > 0.05` meant one.

Tune it with selection filters in the spec, or override per run with `parameters`:

| Filter                  | Default | Effect                                                                     |
| ----------------------- | ------- | -------------------------------------------------------------------------- |
| `claim_confidence`      | `0`     | Minimum confidence for a claim to become a signal                          |
| `claim_signal_weight`   | `0.25`  | Fraction of the momentum spread one unit of net confidence is worth        |
| `claim_horizon_days`    | `90`    | Horizon for claims that did not state one                                  |
| `require_claim_support` | `false` | Hold only symbols with net-positive support that day                       |
| `execution_lag_days`    | `1`     | Trading days between a decision and the close it fills at                  |
| `cash_policy`           | `cash`  | `cash` leaves the weight remainder uninvested; `extend` holds enough ranked names to fill the book without breaching the cap |
| `risk_free_rate`        | `0`     | Annual rate as a decimal, subtracted before `sharpe` and every alpha       |
| `benchmark_symbol`      | `SPY`   | Priced from the snapshot when present, never tradable                     |

Every run's summary reports `n_claim_signals`, `n_claim_supported_rebalances`,
and the applied weight, so you can tell whether research actually moved it.

### What a run is measured against

A total return on its own says whether the number is positive, not whether the
strategy did anything. Every run computes two baselines over its own window and
reports `alpha`, `beta`, `tracking_error` and `information_ratio` against each:

- `equal_weight_universe` — hold every symbol the spec authorizes, equally
  weighted, bought once. This is what the selection has to beat.
- `benchmark_symbol` (default `SPY`) — buy and hold one series. It has to be in
  the snapshot to be priced, so add it to the symbols you fetch; the summary
  reports `benchmark_symbol_priced` rather than quietly leaving the entry out.

Both start flat and buy at the strategy's first fill, so none of them gets a head
start over the lookback window the strategy spends in cash. The benchmark is read
and never traded, which keeps
[ADR 0007](docs/decisions/0007-the-committed-universe-is-the-tradable-set.md)
intact: only the committed universe is tradable.

### What a run charges itself for

`momentum-claims-v4` fixed four places where the simulation's accounting and its
behaviour disagreed — positions now drift between fills rather than being
silently rebalanced for free every day, a stop loss pays for its exit, the
uninvested remainder under a tight `max_position_weight` is reported as
`invested_fraction` instead of reading as a flat strategy, and no decision fills
on the bar that produced it. All four made the old numbers flattering; see
[ADR 0008](docs/decisions/0008-engine-accounting-and-the-v4-version-bump.md).

Checksums do not compare across that boundary, so the previous accounting stays
reachable for a diff:

```powershell
$body = @{
  strategy_id = $id; market_data_snapshot_id = $snapshot
  document_cutoff_at = "2026-03-01T00:00:00Z"
  parameters = @{ engine_version = "momentum-claims-v3" }
} | ConvertTo-Json
Invoke-RestMethod -Method Post http://localhost:8080/v1/backtests -Body $body -ContentType application/json
```

## Candidates

A completed backtest's last rebalance is the paper portfolio it proposes, so
candidates are written when the run completes rather than computed on request.
Every position carries the run that produced it — strategy version, snapshot
checksum, cutoff, engine version, result checksum — and the page-cited claims
behind it.

```powershell
# Latest completed run of every strategy
Invoke-RestMethod http://localhost:8080/v1/candidates

# One strategy, or an exact historical run
Invoke-RestMethod "http://localhost:8080/v1/candidates?strategy_id=$id"
Invoke-RestMethod "http://localhost:8080/v1/candidates?backtest_id=$run"
```

Each candidate's `evidence` array resolves a position down to a page number and
the sentence it was drawn from, with that claim's signed contribution. Its score
resolves too: `score = momentum_rank + claim_signal_weight * claim_support`,
where `momentum` stays the raw trailing return to check against a price chart and
`momentum_rank` is the value that actually entered the ranking.

## Running locally

Docker Desktop is the only required local runtime; the host needs neither Go nor
Python installed (the first build downloads both toolchains' dependencies).

```powershell
Copy-Item .env.example .env
docker compose config
docker compose up --build
```

When the containers are healthy:

```powershell
Invoke-RestMethod http://localhost:8080/healthz
Invoke-RestMethod http://localhost:8000/healthz
```

Stop the stack with `docker compose down`. Add `-v` only when you explicitly want
to erase local database and document-volume data.

### End-to-end smoke test

With the stack up, `scripts/smoke.py` walks the whole pipeline — upload, extract,
review, draft, commit, snapshot, backtest, candidates — and asserts the final
ranking is dense and every position cites a page:

```powershell
python scripts/smoke.py
```

It needs network access for market data, which is why CI cannot run it. Repeated
runs are safe: documents dedupe on their SHA-256 and the strategy slug is
suffixed.

### Comparing engine versions

`scripts/engine_diff.py` re-runs a completed backtest under both engine versions
and prints the metrics side by side. With no argument it picks the newest run;
both runs reuse its strategy, snapshot and cutoff, so the accounting is the only
difference.

```powershell
python scripts/engine_diff.py [backtest_id]
```

Run it against anything worth keeping before `momentum-claims-v3` is deleted.

### Dependencies

Python dependencies are split between intent and resolution:
`python/requirements.in` carries the bounds the project means, and
`python/requirements.txt` is the generated lock — every package pinned, every
wheel hashed, every transitive dependency listed. The image installs it with
`pip install --require-hashes`, which fails the build rather than silently
resolving something else.

Regenerate the lock after editing an `.in` file. It resolves inside the same
`python:3.12-slim` base the image builds on, so the pins match what actually gets
installed:

```powershell
sh python/make-lock.sh
```

Go modules are verified the same way: the Dockerfile's dependency layer copies
`go.sum` alongside `go.mod` and runs `go mod verify`.

### Linting

CI gates on `gofmt -l`, `go vet`, `golangci-lint`, `govulncheck`, `ruff check`,
`mypy`, and `pip-audit`, and builds every compose image. To run the two
containerized ones locally without installing anything:

```powershell
docker run --rm -v "${PWD}:/src" -w /src golangci/golangci-lint:v2.6.1 golangci-lint run ./...
```

`ruff` and `mypy` come from the dev lock (`python/requirements-dev.txt`) and read
their configuration from `python/pyproject.toml`.

### Store integration tests

The Go store tests include an integration suite that runs against a real
PostgreSQL. They skip unless `SIGNALLEDGER_TEST_DATABASE_URL` points at a
**disposable** database (they apply migrations and truncate it); CI provisions
one. `go test ./...` without it still passes — those tests simply skip.

## LLM claim extraction (optional)

The default extractor is a deterministic keyword heuristic. It gates candidate
sentences through a prose filter first, so page furniture — author bylines,
exhibit captions, chart axes, legal boilerplate — never surfaces as a claim. To
use the Anthropic adapter instead, set in `.env`:

```dotenv
LLM_PROVIDER=anthropic
LLM_API_KEY=sk-ant-...
LLM_MODEL=claude-opus-4-8
```

Every LLM claim passes a verbatim-evidence gate: the quoted evidence must appear
on the cited page or the claim is dropped. Expect roughly cents to tens of cents
per document at Opus-tier pricing. Long documents can take minutes, which the
default `QUANT_TIMEOUT=300s` / `JOB_LEASE_DURATION=10m` budgets cover.

## Project status

All seven milestones of the planned pipeline are complete:

1. Durable job leasing and document upload (Go). ✅
2. Page-level PDF extraction and claim review (heuristic + Anthropic adapter). ✅
3. Strategy system: drafts, immutable versions, claim citations. ✅
4. Market-data adapter and checksummed snapshots (yfinance). ✅
5. Deterministic daily backtests with no-look-ahead enforcement. ✅
6. Accepted research claims as point-in-time selection signals (`momentum-claims-v4`). ✅
7. Evidence-backed candidate rankings and a paper portfolio. ✅

The authoritative contracts are in [`contracts/`](contracts), the data model in
[`migrations/`](migrations), and design decisions in
[`docs/decisions/`](docs/decisions). Planned work is tracked in
[`docs/roadmap.md`](docs/roadmap.md).
```
