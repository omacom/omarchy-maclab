-- The metric ids an attempt was asked to measure (spec section 22,
-- "Never repeated"), a JSON array: a benchmark that failed is run once more
-- at the Mac's next slot, and labd counts the attempts that asked for it.
ALTER TABLE perf_runs ADD COLUMN metrics TEXT NOT NULL DEFAULT '[]';
