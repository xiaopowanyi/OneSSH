-- CleanupMetrics deletes by timestamp across all hosts. The table's composite
-- primary key starts with host_id, so it cannot efficiently serve that query.
CREATE INDEX IF NOT EXISTS idx_metrics_ts ON metrics(ts);
