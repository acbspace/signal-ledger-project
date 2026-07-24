# ADR 0002: Immutable strategy versions cite accepted claims

## Status

Accepted.

## Decision

Strategies are immutable rows keyed by `(slug, version)`; submitting a spec for
an existing slug creates the next version instead of mutating history. Every
version stores citations in `strategy_claims`, and only claims whose
`validation_status` is `accepted` may be cited. Cited claims are
deletion-restricted so a committed version can always show its evidence.

Drafting is stateless: `POST /v1/strategies/draft` deterministically proposes a
spec from accepted claims (ticker claims map to an equity momentum template;
macro claims net per theme into long-only ETF proxies) and persists nothing.
The human reviews or edits the proposal and commits it via `POST /v1/strategies`.

## Consequences

- Backtests can reference a strategy version forever without ambiguity, which
  the reproducibility rules require.
- The accepted-claims gate keeps unreviewed extractor output from silently
  becoming the rationale for a strategy.
- Because drafts are never persisted, there is no draft lifecycle to manage;
  the citation table is only written at commit time.
- The macro-theme-to-ETF map is a deliberately small, code-reviewed table; it
  trades coverage for auditability and can grow with the template set.
