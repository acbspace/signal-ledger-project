-- momentum-claims-v4 ranks momentum cross-sectionally onto [-1, 1] before the
-- research tilt is added, so `score` is no longer `momentum + weight * support`.
-- Storing the ranked value keeps a candidate answerable on its own terms: with
-- both columns a stored position still reproduces its own score, while `momentum`
-- stays the raw trailing return a reviewer can read off a price chart.
--
-- Nullable and without a default on purpose. Candidates written under earlier
-- engine versions had no ranked momentum, and inventing one for them would make
-- their rows claim an arithmetic that did not produce them.
ALTER TABLE portfolio_candidates
    ADD COLUMN IF NOT EXISTS momentum_rank DOUBLE PRECISION;
