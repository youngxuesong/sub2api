CREATE INDEX CONCURRENTLY IF NOT EXISTS usage_logs_source_group_created_idx
  ON usage_logs (source_group_id, created_at DESC)
  WHERE source_group_id IS NOT NULL;
