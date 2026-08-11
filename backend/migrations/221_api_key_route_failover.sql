ALTER TABLE api_keys
  ADD COLUMN IF NOT EXISTS route_config_version BIGINT NOT NULL DEFAULT 1,
  ADD COLUMN IF NOT EXISTS failover_risk_acknowledged_at TIMESTAMPTZ NULL;

CREATE TABLE IF NOT EXISTS api_key_route_failover_targets (
  id BIGSERIAL PRIMARY KEY,
  api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  target_group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  priority SMALLINT NOT NULL CHECK (priority BETWEEN 1 AND 5),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (api_key_id, target_group_id),
  UNIQUE (api_key_id, priority)
);

CREATE INDEX IF NOT EXISTS api_key_route_failover_targets_group_idx
  ON api_key_route_failover_targets (target_group_id);

ALTER TABLE usage_logs
  ADD COLUMN IF NOT EXISTS source_group_id BIGINT NULL,
  ADD COLUMN IF NOT EXISTS route_fallback_used BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS route_attempt_count SMALLINT NOT NULL DEFAULT 1,
  ADD COLUMN IF NOT EXISTS route_fallback_reason VARCHAR(64) NULL,
  ADD COLUMN IF NOT EXISTS route_sticky_hit BOOLEAN NOT NULL DEFAULT FALSE;
