package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type apiKeyRouteStickyStore struct {
	rdb *redis.Client
}

func NewAPIKeyRouteStickyStore(rdb *redis.Client) service.APIKeyRouteStickyStore {
	return &apiKeyRouteStickyStore{rdb: rdb}
}

func (s *apiKeyRouteStickyStore) Get(ctx context.Context, apiKeyID int64, sessionHash string) (*service.APIKeyRouteStickyBinding, error) {
	if sessionHash == "" {
		return nil, nil
	}
	payload, err := s.rdb.Get(ctx, apiKeyRouteStickyKey(apiKeyID, sessionHash)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var binding service.APIKeyRouteStickyBinding
	if err := json.Unmarshal(payload, &binding); err != nil {
		return nil, err
	}
	return &binding, nil
}

func (s *apiKeyRouteStickyStore) Set(ctx context.Context, apiKeyID int64, sessionHash string, binding service.APIKeyRouteStickyBinding, ttl time.Duration) error {
	if sessionHash == "" {
		return nil
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, apiKeyRouteStickyKey(apiKeyID, sessionHash), payload, ttl).Err()
}

func (s *apiKeyRouteStickyStore) Refresh(ctx context.Context, apiKeyID int64, sessionHash string, ttl time.Duration) error {
	if sessionHash == "" {
		return nil
	}
	return s.rdb.Expire(ctx, apiKeyRouteStickyKey(apiKeyID, sessionHash), ttl).Err()
}

func (s *apiKeyRouteStickyStore) Delete(ctx context.Context, apiKeyID int64, sessionHash string) error {
	if sessionHash == "" {
		return nil
	}
	return s.rdb.Del(ctx, apiKeyRouteStickyKey(apiKeyID, sessionHash)).Err()
}

func apiKeyRouteStickyKey(apiKeyID int64, sessionHash string) string {
	return fmt.Sprintf("route:sticky:key:%d:%s", apiKeyID, sessionHash)
}
