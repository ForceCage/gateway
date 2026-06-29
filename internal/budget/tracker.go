package budget

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "fc:budget"

// Tracker performs atomic budget checks and adjustments against Redis.
type Tracker struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Tracker {
	return &Tracker{rdb: rdb}
}

// key returns the fixed-window bucket key for the given agent, provider, and window.
// The window resets at multiples of the window duration since the Unix epoch.
func key(agentID, provider string, window time.Duration) string {
	bucket := time.Now().UnixNano() / int64(window)
	return fmt.Sprintf("%s:%s:%s:%d", keyPrefix, agentID, provider, bucket)
}

// checkAndReserve is a Lua script for atomic check-then-increment.
// Returns [allowed (0|1), new_value].
var checkAndReserve = redis.NewScript(`
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local limit   = tonumber(ARGV[1])
local amount  = tonumber(ARGV[2])
local ttl     = tonumber(ARGV[3])
if current + amount > limit then
  return {0, current}
end
local new = redis.call('INCRBYFLOAT', KEYS[1], amount)
redis.call('EXPIRE', KEYS[1], ttl)
return {1, new}
`)

// CheckAndReserve atomically checks whether adding amount would breach limit and,
// if not, reserves it. Returns (allowed, currentSpend, error).
func (t *Tracker) CheckAndReserve(ctx context.Context, agentID, provider string, limit, amount float64, window time.Duration) (bool, float64, error) {
	k := key(agentID, provider, window)
	// Keep the key alive for 2× the window to survive bucket rollovers.
	ttlSec := int(window.Seconds()) * 2

	res, err := checkAndReserve.Run(ctx, t.rdb, []string{k},
		fmt.Sprintf("%f", limit),
		fmt.Sprintf("%f", amount),
		ttlSec,
	).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("redis check-and-reserve: %w", err)
	}

	allowed := res[0] == 1
	// INCRBYFLOAT returns a string; we stored it as an int64 slice from Lua which
	// loses the fractional part. Re-fetch the actual float value.
	currentRaw, err := t.rdb.Get(ctx, k).Float64()
	if err != nil {
		currentRaw = 0
	}
	return allowed, currentRaw, nil
}

// Reconcile adjusts the reserved amount to the actual amount.
// If actual < reserved, it refunds the difference. If actual > reserved, it debits more.
func (t *Tracker) Reconcile(ctx context.Context, agentID, provider string, reserved, actual float64, window time.Duration) error {
	delta := actual - reserved
	if math.Abs(delta) < 0.000001 {
		return nil
	}
	k := key(agentID, provider, window)
	if err := t.rdb.IncrByFloat(ctx, k, delta).Err(); err != nil {
		return fmt.Errorf("redis reconcile: %w", err)
	}
	return nil
}

// CurrentSpend returns the current spend for agentID+provider in the active window.
func (t *Tracker) CurrentSpend(ctx context.Context, agentID, provider string, window time.Duration) (float64, error) {
	k := key(agentID, provider, window)
	v, err := t.rdb.Get(ctx, k).Float64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}
