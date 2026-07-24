-- Backtest result pointers. A backtest_runs row is created queued and completed
-- by the worker once the stateless quant service returns. Scalar metrics live in
-- the existing summary JSONB; these columns pin the engine version and a content
-- checksum so an identical run is provably reproducible.

ALTER TABLE backtest_runs
    ADD COLUMN IF NOT EXISTS engine_version TEXT,
    ADD COLUMN IF NOT EXISTS result_artifact_key TEXT,
    ADD COLUMN IF NOT EXISTS result_checksum CHAR(64);
