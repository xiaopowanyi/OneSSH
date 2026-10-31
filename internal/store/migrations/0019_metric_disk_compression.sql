-- Tracks the resumable, lossless conversion of historical disks_json values.
CREATE TABLE IF NOT EXISTS metric_disk_storage_state (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  cursor_host_id INTEGER NOT NULL DEFAULT -1,
  cursor_ts INTEGER NOT NULL DEFAULT -1,
  compression_done INTEGER NOT NULL DEFAULT 0,
  compaction_done INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO metric_disk_storage_state(id) VALUES(1);
