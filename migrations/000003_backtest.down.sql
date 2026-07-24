ALTER TABLE backtest_runs
    DROP COLUMN IF EXISTS result_checksum,
    DROP COLUMN IF EXISTS result_artifact_key,
    DROP COLUMN IF EXISTS engine_version;
