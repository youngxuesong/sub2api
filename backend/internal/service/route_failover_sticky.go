package service

import (
	"context"
	"time"
)

const APIKeyRouteStickyTTL = time.Hour

type APIKeyRouteStickyBinding struct {
	RouteConfigVersion int64 `json:"route_config_version"`
	EffectiveGroupID   int64 `json:"effective_group_id"`
}

type APIKeyRouteStickyStore interface {
	Get(ctx context.Context, apiKeyID int64, sessionHash string) (*APIKeyRouteStickyBinding, error)
	Set(ctx context.Context, apiKeyID int64, sessionHash string, binding APIKeyRouteStickyBinding, ttl time.Duration) error
	Refresh(ctx context.Context, apiKeyID int64, sessionHash string, ttl time.Duration) error
	Delete(ctx context.Context, apiKeyID int64, sessionHash string) error
}
