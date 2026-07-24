# ADR 0003: LLM claim extraction behind a verbatim-evidence gate

## Status

Accepted.

## Decision

Claim extraction supports two adapters selected by `LLM_PROVIDER`: the original
deterministic keyword heuristic (`disabled`, the default) and an Anthropic API
adapter (`anthropic`, model set by `LLM_MODEL`). Both return the same response
contract, so the Go side is unaware of which extractor produced a claim beyond
the recorded `extractor` label.

The LLM adapter enforces a verbatim-evidence gate: a claim is kept only when
its `evidence_quote` appears (whitespace-normalized) in the cited page's
extracted text. Claims that fail the gate are dropped, never repaired.

## Consequences

- Hallucinated evidence cannot reach the database, preserving the page-cited
  evidence guarantee that the strategy citation model depends on.
- Extraction cost is roughly cents to tens of cents per document at Opus-tier
  pricing; the provider can be disabled or the model downgraded per document
  batch via environment variables.
- LLM calls can take minutes for long documents, so the quant call timeout and
  job lease defaults were raised (`QUANT_TIMEOUT=300s`, `JOB_LEASE_DURATION=10m`).
- Transient provider failures surface as 502s and ride the job queue's
  retry/backoff; no bespoke retry logic lives in Python.
