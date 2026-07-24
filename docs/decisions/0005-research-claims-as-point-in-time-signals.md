# ADR 0005: Research claims tilt selection as point-in-time signals

## Status

Accepted.

## Context

Through ADR 0004 the backtest engine was price-only: it ranked a universe by
trailing-return momentum and never read the research that justified the
strategy. That made the "research-to-portfolio" claim hollow — the evidence
chain stopped at the universe. It also left an open question the whole project
exists to answer honestly: how do you let a document influence a simulation
without letting it influence days that preceded it?

## Decision

An accepted claim becomes a *signal*: a signed confidence attached to a symbol,
live over a date window.

**The worker resolves claims to symbols.** A claim's ticker wins; otherwise the
macro-theme keyword map already used for drafting supplies ETF proxies. Only the
strategy's own cited claims are considered, and only for symbols the spec
actually trades — a claim citing something outside the universe stays evidence
without becoming a trading signal.

**Three gates run before the engine sees anything.** The claim must be accepted
by a human, must resolve into the universe, and must have been effective at or
before the run's `document_cutoff_at`. The last gate is the look-ahead barrier at
run granularity.

**The engine applies a second, independent gate per rebalance.** A signal counts
only from its `effective_at` until its horizon expires (`horizon_days`, or the
spec's `claim_horizon_days`, default 90). So a claim published between two
rebalances changes the later one and cannot reach back to the earlier one, even
though both are in the same run.

**The tilt is additive on the momentum score.** Per symbol per rebalance, net
signed confidence is clamped to `[-1, 1]` and the ranking score becomes
`momentum + claim_signal_weight * claim_score` (default weight `0.1`: a
full-confidence claim is worth ten points of trailing return). `claim_confidence`
sets the minimum confidence to qualify; `require_claim_support` restricts
holdings to symbols with net-positive support on that day.

**Filter fields are a closed set.** Go validation and the JSON contract both
enumerate the knobs the engine reads, because a misspelled filter that silently
does nothing is exactly the failure this project cannot tolerate.

`ENGINE_VERSION` moves to `momentum-claims-v2`.

## Consequences

- The evidence chain now runs end to end: a page-cited claim a human accepted
  changes which symbols a run holds, and `strategy_claims` still records why.
- Determinism survives. Signals are sorted on a total key by both sides, so
  floating-point accumulation order cannot vary; identical inputs still produce
  an identical `result_checksum`.
- Every run reports `n_claim_signals`, `n_claim_supported_rebalances`, and the
  weight applied, so "did research actually move this?" is answerable from the
  summary without reopening the artifact.
- Each rebalance in the holdings log records the claim support behind each
  position — the input the candidate-ranking milestone needs.
- Additive tilting is deliberately crude. It keeps the mapping from evidence to
  weight legible; a factor model would be more expressive and far harder to
  audit. Revisit when there is enough accepted-claim history to fit one.
- Long-only stays the rule: a negative claim demotes a symbol, it never shorts.
