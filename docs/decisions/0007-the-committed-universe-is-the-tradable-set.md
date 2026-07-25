# ADR 0007: The committed universe is the tradable set

## Status

Accepted.

## Context

`simulate` derived its tradable set from the snapshot CSV's columns and never read
`spec.universe.symbols`. Because market-data snapshots are a separate resource
that a strategy points at by ID, one snapshot can legitimately hold more series
than any single strategy trades — so the engine ranked, held, and proposed symbols
no reviewed spec had ever authorized.

Reproduced with a spec authorizing only `AAA`/`BBB` against a snapshot that also
carried `ZZZ`:

```
engine held at first rebalance : ['AAA', 'ZZZ']
candidates written to the DB   : ['ZZZ', 'AAA']
```

`ZZZ` became rank 1 of the paper portfolio with no claim, no citation, and no
evidence. That inverts the project's central invariant: a candidate is supposed to
answer "why do you hold this?" with a page number and a quoted sentence, and a
symbol outside the universe has no cited research behind it by construction.

The gap was asymmetric, which is why it survived. Go already restricted *claim
signals* to `spec.Universe.Symbols` in `BuildSignals`, so the universe gated the
evidence while leaving the trading unconstrained. Nothing validated that a
snapshot could even price the universe, either, so a mismatched pairing was
accepted silently and produced a plausible-looking portfolio of the wrong names.

## Decision

`spec.universe.symbols` is the tradable set, enforced at three layers that fail
independently.

**The engine decides.** `simulate` ranks the universe and nothing else. Extra
snapshot columns are ignored; a universe symbol the snapshot cannot price is a
`BacktestError` rather than a silent subset, because trading whatever happened to
be available would make a result depend on data availability instead of on the
reviewed spec. The universe is validated here rather than trusted — this service
is an independent HTTP boundary and must not rank a symbol just because a caller
got it into the snapshot. Claim signals naming a symbol outside the universe are
dropped, matching what Go already did, which also keeps `n_claim_signals` honest
about what could have moved the run.

**The API fails fast.** `POST /v1/backtests` rejects a snapshot whose symbols do
not cover the strategy's universe with `409 snapshot_universe_mismatch`, naming
the missing symbols. This check is advisory, not authoritative: the snapshot row
records the symbols that were *requested*, while the engine sees the frozen CSV's
actual columns. It exists so the common mistake — pairing a strategy with a
snapshot fetched for a different one — is an answerable error instead of a job
that dies minutes later in the worker.

**The worker guards the database.** Before completing a run, the worker checks the
proposed ranking against the universe it built the signals from and refuses to
persist a candidate naming anything else. This is the layer that protects the
stored invariant regardless of which engine produced the response.

`ENGINE_VERSION` moves to `momentum-claims-v3`. Selection results change for any
run whose snapshot carried symbols beyond its universe, and a version string that
maps to two behaviours would break the reproducibility contract more quietly than
the bug it fixes. Runs report `n_universe_symbols` so a summary states how many
symbols were authorized and priced.

## Consequences

- An unauthorized symbol cannot be held, cannot be proposed, and cannot reach
  `portfolio_candidates`. Every stored position traces to a cited page again.
- Snapshots stay shareable across strategies, which was the point of making them a
  separate resource. Only *gaps* are an error; surplus is ignored.
- A snapshot missing a universe symbol now fails loudly instead of quietly
  backtesting a subset. This will surface as errors on strategy/snapshot pairings
  that previously "worked", which is the correct outcome: those runs were ranking
  the wrong set of names.
- Existing `result_checksum` values are not comparable to `v3` runs. Re-running a
  known strategy under both versions and diffing the curves is the cheapest
  evidence of what this changed.
- Three layers overlap deliberately. The engine's check is the one that matters
  for correctness; the other two convert a silent wrong answer into a clear error
  and protect the database if a future engine regresses.
