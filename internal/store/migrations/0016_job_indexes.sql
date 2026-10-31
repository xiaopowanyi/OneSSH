CREATE INDEX IF NOT EXISTS idx_jobs_started_at_id ON jobs(started_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_jobs_status_started_at_id ON jobs(status, started_at DESC, id DESC);
