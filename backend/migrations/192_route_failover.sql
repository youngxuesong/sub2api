CREATE TABLE IF NOT EXISTS route_failover_policies (
    id BIGSERIAL PRIMARY KEY,
    source_group_id BIGINT NOT NULL UNIQUE REFERENCES groups(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    failure_threshold INTEGER NOT NULL DEFAULT 5 CHECK (failure_threshold > 0),
    success_threshold INTEGER NOT NULL DEFAULT 2 CHECK (success_threshold > 0),
    window_seconds INTEGER NOT NULL DEFAULT 60 CHECK (window_seconds > 0),
    open_cooldown_seconds INTEGER NOT NULL DEFAULT 60 CHECK (open_cooldown_seconds > 0),
    half_open_lease_seconds INTEGER NOT NULL DEFAULT 15 CHECK (half_open_lease_seconds > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS route_failover_targets (
    id BIGSERIAL PRIMARY KEY,
    policy_id BIGINT NOT NULL REFERENCES route_failover_policies(id) ON DELETE CASCADE,
    target_group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    priority INTEGER NOT NULL DEFAULT 0,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    model_mapping JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (policy_id, target_group_id)
);

CREATE INDEX IF NOT EXISTS route_failover_targets_policy_priority_idx
    ON route_failover_targets (policy_id, priority, id);

CREATE INDEX IF NOT EXISTS route_failover_targets_group_idx
    ON route_failover_targets (target_group_id);
