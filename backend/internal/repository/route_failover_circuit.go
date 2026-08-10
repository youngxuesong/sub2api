package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

var routeFailoverAllowScript = redis.NewScript(`
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
if state == 'open' then
  local opened = tonumber(redis.call('HGET', KEYS[1], 'opened_at') or '0')
  if tonumber(ARGV[1]) - opened < tonumber(ARGV[2]) then
    return {0, 0}
  end
end
if state == 'open' or state == 'half_open' then
  local acquired = redis.call('SET', KEYS[2], ARGV[3], 'NX', 'PX', ARGV[4])
  if not acquired then
    return {0, 0}
  end
  redis.call('HSET', KEYS[1], 'state', 'half_open')
  return {1, 1}
end
return {1, 0}
`)

var routeFailoverFailureScript = redis.NewScript(`
if ARGV[5] ~= '' then
  if redis.call('GET', KEYS[3]) ~= ARGV[5] then
    return 0
  end
  redis.call('DEL', KEYS[3])
end
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
if state == 'half_open' then
  if ARGV[5] == '' then
    return 0
  end
  redis.call('HSET', KEYS[1], 'state', 'open', 'opened_at', ARGV[1], 'successes', 0)
  redis.call('DEL', KEYS[2])
  return 1
end
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', tonumber(ARGV[1]) - tonumber(ARGV[2]))
redis.call('ZADD', KEYS[2], ARGV[1], ARGV[4])
local failures = redis.call('ZCARD', KEYS[2])
redis.call('PEXPIRE', KEYS[2], tonumber(ARGV[2]) * 2)
if failures >= tonumber(ARGV[3]) then
  redis.call('HSET', KEYS[1], 'state', 'open', 'opened_at', ARGV[1], 'successes', 0)
  return 1
end
return 0
`)

var routeFailoverSuccessScript = redis.NewScript(`
if ARGV[4] ~= '' then
  if redis.call('GET', KEYS[3]) ~= ARGV[4] then
    return 0
  end
  redis.call('DEL', KEYS[3])
end
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
if state == 'half_open' then
  if ARGV[4] == '' then
    return 0
  end
  local successes = redis.call('HINCRBY', KEYS[1], 'successes', 1)
  if successes >= tonumber(ARGV[3]) then
    redis.call('HSET', KEYS[1], 'state', 'closed', 'successes', 0)
    redis.call('HDEL', KEYS[1], 'opened_at')
    redis.call('DEL', KEYS[2])
  end
else
  redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', tonumber(ARGV[1]) - tonumber(ARGV[2]))
end
return 1
`)

type routeFailoverCircuit struct {
	rdb *redis.Client
	now func() time.Time
}

func NewRouteFailoverCircuit(rdb *redis.Client) service.RouteFailoverCircuit {
	return newRouteFailoverCircuit(rdb, time.Now)
}

func newRouteFailoverCircuit(rdb *redis.Client, now func() time.Time) *routeFailoverCircuit {
	return &routeFailoverCircuit{rdb: rdb, now: now}
}

func (c *routeFailoverCircuit) Allow(ctx context.Context, policy service.RouteFailoverPolicy, target service.RouteFailoverTarget, model string) (service.RouteFailoverPermit, error) {
	stateKey, _, leaseKey := routeFailoverCircuitKeys(policy.ID, target.ID, model)
	leaseID, err := randomRouteFailoverLeaseID()
	if err != nil {
		return service.RouteFailoverPermit{}, err
	}
	result, err := routeFailoverAllowScript.Run(ctx, c.rdb, []string{stateKey, leaseKey},
		c.now().UnixMilli(), durationMillis(policy.OpenCooldown, time.Minute), leaseID,
		durationMillis(policy.HalfOpenLease, 15*time.Second)).Int64Slice()
	if err != nil {
		return service.RouteFailoverPermit{}, err
	}
	permit := service.RouteFailoverPermit{Allowed: result[0] == 1, HalfOpen: result[1] == 1}
	if permit.HalfOpen {
		permit.LeaseID = leaseID
	}
	return permit, nil
}

func (c *routeFailoverCircuit) RecordFailure(ctx context.Context, policy service.RouteFailoverPolicy, target service.RouteFailoverTarget, model, leaseID string) error {
	stateKey, failuresKey, leaseKey := routeFailoverCircuitKeys(policy.ID, target.ID, model)
	now := c.now().UnixMilli()
	_, err := routeFailoverFailureScript.Run(ctx, c.rdb, []string{stateKey, failuresKey, leaseKey},
		now, durationMillis(policy.Window, time.Minute), positiveOr(policy.FailureThreshold, 5),
		fmt.Sprintf("%d:%s", now, mustRandomSuffix()), leaseID).Result()
	return err
}

func (c *routeFailoverCircuit) RecordSuccess(ctx context.Context, policy service.RouteFailoverPolicy, target service.RouteFailoverTarget, model, leaseID string) error {
	stateKey, failuresKey, leaseKey := routeFailoverCircuitKeys(policy.ID, target.ID, model)
	_, err := routeFailoverSuccessScript.Run(ctx, c.rdb, []string{stateKey, failuresKey, leaseKey},
		c.now().UnixMilli(), durationMillis(policy.Window, time.Minute), positiveOr(policy.SuccessThreshold, 2), leaseID).Result()
	return err
}

func routeFailoverCircuitKeys(policyID, targetID int64, model string) (string, string, string) {
	hash := sha256.Sum256([]byte(model))
	base := fmt.Sprintf("route_failover:%d:%d:%s", policyID, targetID, hex.EncodeToString(hash[:8]))
	return base + ":state", base + ":failures", base + ":lease"
}

func randomRouteFailoverLeaseID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func mustRandomSuffix() string {
	value, err := randomRouteFailoverLeaseID()
	if err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return value
}

func durationMillis(value, fallback time.Duration) int64 {
	if value <= 0 {
		value = fallback
	}
	return value.Milliseconds()
}

func positiveOr(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}
