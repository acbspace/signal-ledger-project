# ADR 0006: Candidates are a projection of a completed backtest run

## Status

Accepted.

## Context

`GET /v1/candidates` was the last declared-but-unimplemented boundary. The
obvious implementation — score the universe on demand when someone asks — would
have produced a ranking with no provenance: no pinned snapshot, no cutoff, no
engine version, nothing to re-run. That is exactly the artifact this project
exists to avoid.

## Decision

A candidate is not computed on request. A completed backtest's **last rebalance
is** the paper portfolio that run proposes, so the engine returns it alongside
the equity curve and the worker persists it in the same transaction that
completes the run.

`portfolio_candidates` holds one row per position — symbol, rank, weight, score,
momentum, claim support, `as_of` — and `candidate_claims` attributes each
position to the claims that were live behind it, with each claim's signed
contribution. Reading a candidate therefore reaches a page number and a quoted
sentence in a specific PDF.

Candidates deliberately stay out of the checksummed artifact. `result_checksum`
is the hash of the equity curve, which is the determinism proof; candidates are a
projection of the same simulation and belong in Postgres, where they can be
queried and joined. `engine_version` did not change: nothing about the
simulation math moved, only what the run reports.

`GET /v1/candidates` serves the latest completed run of every strategy by
default — the paper portfolio as a whole — narrowed by `strategy_id`, or pinned
to an exact historical run by `backtest_id`.

## Consequences

- Every position carries its run, and therefore that run's reproducibility
  inputs: strategy version, snapshot checksum, document cutoff, engine version,
  result checksum. "Why do you hold this?" is answerable down to a sentence.
- Candidates cannot drift from the run that justified them: one transaction
  writes both, and deleting a run cascades its ranking away.
- A ranking is only as fresh as its last backtest. Getting a current portfolio
  means running a current backtest — which is the honest cost of insisting every
  ranking be reproducible.
- Ranks are dense and validated on the way in, so a partial or reordered engine
  response is rejected rather than persisted.
- Claims backing a position are deletion-restricted, mirroring `strategy_claims`.
