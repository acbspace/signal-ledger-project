-- Evidence-backed candidate rankings. A completed backtest's last rebalance is
-- the paper portfolio that run proposes, so candidates are written in the same
-- transaction that completes the run and inherit its reproducibility inputs:
-- strategy version, snapshot, cutoff, engine version, and result checksum.

CREATE TABLE IF NOT EXISTS portfolio_candidates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    backtest_run_id UUID NOT NULL REFERENCES backtest_runs(id) ON DELETE CASCADE,
    strategy_id UUID NOT NULL REFERENCES strategies(id),
    symbol TEXT NOT NULL,
    rank INTEGER NOT NULL CHECK (rank > 0),
    weight DOUBLE PRECISION NOT NULL CHECK (weight > 0),
    score DOUBLE PRECISION NOT NULL,
    momentum DOUBLE PRECISION NOT NULL,
    claim_support DOUBLE PRECISION NOT NULL,
    as_of DATE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (backtest_run_id, symbol),
    UNIQUE (backtest_run_id, rank)
);

CREATE INDEX IF NOT EXISTS portfolio_candidates_strategy_idx
    ON portfolio_candidates (strategy_id, as_of DESC);

-- Why a candidate is a candidate. Claims are deletion-restricted while they back
-- a position, mirroring strategy_claims, so a ranking can always show its pages.
CREATE TABLE IF NOT EXISTS candidate_claims (
    candidate_id UUID NOT NULL REFERENCES portfolio_candidates(id) ON DELETE CASCADE,
    claim_id UUID NOT NULL REFERENCES research_claims(id) ON DELETE RESTRICT,
    contribution DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (candidate_id, claim_id)
);

CREATE INDEX IF NOT EXISTS candidate_claims_claim_idx ON candidate_claims (claim_id);
