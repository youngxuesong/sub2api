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

const (
	routeFailoverWindow    = time.Minute
	routeFailoverThreshold = 5
	routeFailoverCooldown  = time.Minute
	routeFailoverLease     = 15 * time.Second
	routeFailoverSuccesses = 2
)

var routeFailoverAllowScript = redis.NewScript(`
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
if state == 'open' then
  local opened = tonumber(redis.call('HGET', KEYS[1], 'opened_at') or '0')
  if tonumber(ARGV[1]) - opened < tonumber(ARGV[2]) then return {0, 0, 0} end
end
if state == 'open' or state == 'half_open' then
  local acquired = redis.call('SET', KEYS[2], ARGV[3], 'NX', 'PX', ARGV[4])
  if not acquired then return {0, 0, 0} end
  redis.call('HSET', KEYS[1], 'state', 'half_open')
  if state == 'open' then return {1, 1, 1} end
  return {1, 1, 0}
end
return {1, 0, 0}
`)

var routeFailoverFailureScript = redis.NewScript(`
if ARGV[5] ~= '' then
  if redis.call('GET', KEYS[3]) ~= ARGV[5] then return 0 end
  redis.call('DEL', KEYS[3])
end
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
if state == 'half_open' then
  if ARGV[5] == '' then return 0 end
  redis.call('HSET', KEYS[1], 'state', 'open', 'opened_at', ARGV[1], 'successes', 0)
  return 2
end
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', tonumber(ARGV[1]) - tonumber(ARGV[2]))
redis.call('ZADD', KEYS[2], ARGV[1], ARGV[4])
local failures = redis.call('ZCARD', KEYS[2])
redis.call('PEXPIRE', KEYS[2], tonumber(ARGV[2]) * 2)
if failures >= tonumber(ARGV[3]) then
  redis.call('HSET', KEYS[1], 'state', 'open', 'opened_at', ARGV[1], 'successes', 0)
  if state ~= 'open' then return 2 end
end
return 1
`)

var routeFailoverSuccessScript = redis.NewScript(`
if ARGV[4] ~= '' then
  if redis.call('GET', KEYS[3]) ~= ARGV[4] then return 0 end
  redis.call('DEL', KEYS[3])
end
local state = redis.call('HGET', KEYS[1], 'state') or 'closed'
if state == 'half_open' then
  if ARGV[4] == '' then return 0 end
  local successes = redis.call('HINCRBY', KEYS[1], 'successes', 1)
  if successes >= tonumber(ARGV[3]) then
    redis.call('HSET', KEYS[1], 'state', 'closed', 'successes', 0)
    redis.call('HDEL', KEYS[1], 'opened_at')
    redis.call('DEL', KEYS[2])
    return 2
  end
else
  redis.call('DEL', KEYS[2])
end
return 1
`)

type routeFailoverCircuit struct {
	rdb *redis.Client
	now func() time.Time
}

// NewRouteFailoverCircuit creates the Redis-backed route failover circuit.
// The service-level interface is intentionally adapted by the caller.
func NewRouteFailoverCircuit(rdb *redis.Client) *routeFailoverCircuit {
	return newRouteFailoverCircuit(rdb, time.Now)
}

func newRouteFailoverCircuit(rdb *redis.Client, now func() time.Time) *routeFailoverCircuit {
	return &routeFailoverCircuit{rdb: rdb, now: now}
}

func (c *routeFailoverCircuit) Allow(ctx context.Context, effectiveGroupID int64, requestedModel string) (allowed, halfOpen bool, leaseID string, err error) {
	stateKey, _, leaseKey := routeFailoverCircuitKeys(effectiveGroupID, requestedModel)
	leaseID, err = randomRouteFailoverLeaseID()
	if err != nil {
		return false, false, "", err
	}
	result, err := routeFailoverAllowScript.Run(ctx, c.rdb, []string{stateKey, leaseKey},
		c.now().UnixMilli(), routeFailoverCooldown.Milliseconds(), leaseID, routeFailoverLease.Milliseconds()).Int64Slice()
	if err != nil {
		return false, false, "", err
	}
	allowed, halfOpen = result[0] == 1, result[1] == 1
	if len(result) > 2 && result[2] == 1 {
		service.RecordAPIKeyRouteCircuitTransition(service.RouteCircuitHalfOpen)
	}
	if !halfOpen {
		leaseID = ""
	}
	return allowed, halfOpen, leaseID, nil
}

func (c *routeFailoverCircuit) RecordFailure(ctx context.Context, effectiveGroupID int64, requestedModel, leaseID string) error {
	stateKey, failuresKey, leaseKey := routeFailoverCircuitKeys(effectiveGroupID, requestedModel)
	now := c.now().UnixMilli()
	result, err := routeFailoverFailureScript.Run(ctx, c.rdb, []string{stateKey, failuresKey, leaseKey},
		now, routeFailoverWindow.Milliseconds(), routeFailoverThreshold,
		fmt.Sprintf("%d:%s", now, mustRandomSuffix()), leaseID).Int64()
	if err == nil && result == 2 {
		service.RecordAPIKeyRouteCircuitTransition(service.RouteCircuitOpened)
	}
	return err
}

func (c *routeFailoverCircuit) RecordSuccess(ctx context.Context, effectiveGroupID int64, requestedModel, leaseID string) error {
	stateKey, failuresKey, leaseKey := routeFailoverCircuitKeys(effectiveGroupID, requestedModel)
	result, err := routeFailoverSuccessScript.Run(ctx, c.rdb, []string{stateKey, failuresKey, leaseKey},
		c.now().UnixMilli(), routeFailoverWindow.Milliseconds(), routeFailoverSuccesses, leaseID).Int64()
	if err == nil && result == 2 {
		service.RecordAPIKeyRouteCircuitTransition(service.RouteCircuitClosed)
	}
	return err
}

func routeFailoverCircuitKeys(effectiveGroupID int64, requestedModel string) (string, string, string) {
	hash := sha256.Sum256([]byte(requestedModel))
	base := fmt.Sprintf("route_failover:group:%d:%s", effectiveGroupID, hex.EncodeToString(hash[:8]))
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
