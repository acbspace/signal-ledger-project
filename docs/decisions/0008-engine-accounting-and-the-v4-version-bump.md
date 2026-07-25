# 8. Engine accounting, and the momentum-claims-v4 version bump

Date: 2026-07-25

## Status

Accepted. Supersedes the accounting described in
[ADR 0005](0005-research-claims-as-point-in-time-signals.md), which stands as to
*when* a claim counts; this record changes *how much* it counts and what the
book is charged for acting on it.

## Context

`momentum-claims-v3` produced reproducible numbers. Reproducible is not the same
as correct: a run could be replayed byte-for-byte and still describe a portfolio
that could not exist. Four defects in the accounting, and one in the research
tilt, all pushed the same direction — they flattered the strategy.

Each was reproduced against the engine before being changed:

**Positions were held at constant weight while prices moved.** The day loop
reapplied the last target weights to every daily return, which is a rebalance to
target every day, but turnover was charged only on rebalance dates. On a
thirty-day panel with two rebalances, twenty-eight days of implicit rebalancing
were free.

**A stop loss did not pay for its exit.** It set the weight to zero outside the
rebalance branch, so the forced sale cost nothing and never entered
`total_turnover`; because the zeroed weight persisted, the next rebalance had
nothing left to sell. Enabling a stop loss therefore *reduced* reported turnover
— 1.5 against 2.0 on the same panel — and left final equity slightly *higher*
than the same run without one.

**`max_position_weight` below `1/top_n` ran a mostly uninvested book, silently.**
`min(1/n, max_position_weight)` caps each position without redistributing, so a
0.2 cap with `top_n = 2` held 40% and reported nothing about the other 60%.
Total return scaled linearly with the cap — 0.0198 against 0.0500 — which reads
as a strategy that does not do much rather than one that was barely invested.

**Momentum and the fill read the same bar.** A strategy needed the closing price
to place the trade it filled at that close.

**The research tilt was measured in the wrong units.** `momentum + weight *
support` adds a confidence to a trailing return, so the same
`claim_signal_weight` reordered a quiet universe and did nothing to a violent
one. The tilt's real influence was the weight divided by whatever the assets
happened to be doing — which is not a parameter anybody chose.

## Decision

Fix all five at once and bump `ENGINE_VERSION` to `momentum-claims-v4` a single
time, rather than per fix. Intermediate versions would each name a set of
numbers nobody ran a strategy under and nobody would ever compare against.

* Holdings are carried as market values and drift with prices. Turnover is
  charged on the value actually traded when an order fills. The cost formula is
  unchanged — `transaction_cost_bps / 10_000 * turnover` — but the position it
  is measured against is now marked to market rather than assumed.
* A stop loss sends an order and is charged like any other trade. It is counted
  in `n_stop_loss_exits`.
* `cash_policy` is explicit: `cash` (the default, v3's behaviour) leaves the
  remainder uninvested, and `extend` lets the cap decide the position count so
  the book fills without breaching `max_position_weight`. Every summary reports
  `invested_fraction`.
* `execution_lag_days` defaults to 1. Every order — rebalance or stop exit — is
  decided on one close and filled on a later one.
* Momentum is ranked cross-sectionally onto [-1, 1] before the tilt is added,
  with tied returns sharing an average rank. `claim_signal_weight` now means one
  thing everywhere: the fraction of the momentum spread a full-confidence claim
  is worth.

Its default moves from 0.1 to 0.25 with that change. Carrying 0.1 across would
have cut research influence to about a twentieth of the ranking, which is the
opposite of what normalizing the tilt is for. Gates still read the raw trailing
return, because a committed `momentum > 0.05` meant a return.

Portfolio-level sums use `math.fsum`. v3 summed turnover over `set(target) |
set(weights)`, so its arithmetic depended in principle on set iteration order;
exact summation removes the question rather than papering over it with a sort.

### Keeping v3 reachable

`engine_version` is a run parameter. `momentum-claims-v3` selects a frozen second
simulator that reproduces the old accounting bit-for-bit, so a strategy can be
re-run under both and the curves diffed. Four recorded curve checksums, captured
before this change, are asserted in the test suite — including one asserting that
the stop-loss defect *still* reduces turnover under v3. That test is a guard, not
an endorsement: a preserved path whose defects have been tidied up no longer
reproduces the runs it exists to reproduce.

The preserved path rejects `execution_lag_days` and `cash_policy` rather than
ignoring them, since applying a v4 knob to v3's accounting would produce a curve
that never existed.

### Candidates carry both momentum numbers

`score` is no longer `momentum + weight * support`, so a stored position could no
longer reproduce its own score. Migration `000005` adds `momentum_rank`
alongside `momentum`: the ranked value that entered the scoring, next to the raw
trailing return a reviewer can check against a price chart. It is nullable, and
rows written under earlier engine versions keep a null rather than being given an
invented value.

## Consequences

Existing `result_checksum` values are not comparable across the boundary. Every
strategy worth keeping should be re-run and diffed before v3 is retired; nothing
in the pipeline rewrites historical runs, so old rows stay valid statements about
the accounting that produced them — which is what `engine_version` on every run
and every candidate is for.

Reported numbers get worse, and that is the point. Turnover rises because
drifting positions and forced exits are now charged. A capped book that looked
like a flat strategy now says it was 40% invested. A stop loss costs something.

The tilt's default strength changed, so a committed spec that never set
`claim_signal_weight` will tilt differently under v4. Specs are immutable and
runs are re-runnable, so this is visible rather than silent: both the weight and
the momentum scale are recorded in every run summary.

Two things this does not fix, both still open in the roadmap: nothing yet
validates that a snapshot's date range spans the lookback plus the test period,
and `sharpe` still assumes a zero risk-free rate without saying so.
