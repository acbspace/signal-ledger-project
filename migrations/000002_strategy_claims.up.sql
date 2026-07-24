-- Strategy evidence citations. Claims are deletion-restricted while cited so a
-- committed strategy version can always show the research it was built from.
CREATE TABLE IF NOT EXISTS strategy_claims (
    strategy_id UUID NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    claim_id UUID NOT NULL REFERENCES research_claims(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (strategy_id, claim_id)
);

CREATE INDEX IF NOT EXISTS strategy_claims_claim_idx ON strategy_claims (claim_id);
