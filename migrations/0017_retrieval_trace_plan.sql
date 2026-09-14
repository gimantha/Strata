-- Preserve the actual retrieval strategy with each trace. Candidate scores without the
-- modes, rewrites, ranks, and inferred temporal intent that produced them cannot explain a
-- miss or distinguish a planner defect from an index defect.
ALTER TABLE retrieval_traces
    ADD COLUMN plan jsonb NOT NULL DEFAULT '{}'::jsonb;
