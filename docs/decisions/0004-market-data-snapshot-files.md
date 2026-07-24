# ADR 0004: Market data snapshots are canonical CSV files on the shared volume

## Status

Accepted.

## Decision

Market-data retrieval is a durable `fetch_market_data` job. The worker asks the
stateless Python service for normalized daily bars, serializes them to a
canonical CSV (fixed header, rows sorted by symbol then date, shortest
round-trip float formatting), writes the file to the shared document volume
under a `market-data/` storage key, and records a `market_data_snapshots` row
with the provider, parameters, retrieval time, and the SHA-256 of the CSV bytes.

## Consequences

- Snapshots follow the same storage-key pattern as PDFs, so the Python backtest
  engine (next milestone) can read them read-only by key with no new plumbing.
- The canonical serialization makes checksums reproducible: identical data
  always yields an identical checksum regardless of provider return order.
- Running retrieval through the job queue gives provider flakiness the existing
  lease/retry/backoff semantics instead of ad-hoc HTTP retries.
- The worker now mounts the shared volume read-write; Python remains read-only.
