# ADR 0001: Go owns state; Python owns analysis

## Status

Accepted.

## Decision

Use Go for the application API, job lifecycle, durable state, and orchestration.
Use a stateless Python service for OpenBB, PDF parsing, and numerical backtests.
The services communicate over versioned HTTP/JSON requests.

## Consequences

- Go remains the main implementation language and demonstrates concurrency,
  leases, idempotency, persistence, and observability.
- Python retains access to the quantitative/data ecosystem without leaking
  application state across language boundaries.
- The contract boundary is easy to test and may evolve to gRPC only if profiling
  establishes a real need.
