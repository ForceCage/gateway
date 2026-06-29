package budget

import (
	"context"
	"fmt"
	"math"
	"strconv"
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

// Reservation records a successful reserve against a specific fixed-window bucket.
// It captures the exact key at reserve time so reconciliation and release always
// target the same bucket, even if the window rolls over before the response returns.
type Reservation struct {
	Key    string
	Amount float64
}

// key returns the fixed-window bucket key for the given agent, provider, and window.
// The window resets at multiples of the window duration since the Unix epoch.
func key(agentID, provider string, window time.Duration) string {
	bucket := time.Now().UnixNano() / int64(window)
	return fmt.Sprintf("%s:%s:%s:%d", keyPrefix, agentID, provider, bucket)
}

// checkAndReserve is a Lua script for atomic check-then-increment.
// Returns [allowed (0|1), current_or_new_value_as_string].
// The value is returned as a string so the float fraction survives the round trip
// (Redis truncates Lua numbers returned directly to integers).
var checkAndReserve = redis.NewScript(`
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local limit   = tonumber(ARGV[1])
local amount  = tonumber(ARGV[2])
local ttl     = tonumber(ARGV[3])
if current + amount > limit then
  return {0, tostring(current)}
end
local new = redis.call('INCRBYFLOAT', KEYS[1], amount)
redis.call('EXPIRE', KEYS[1], ttl)
return {1, new}
`)

// CheckAndReserve atomically checks whether adding amount would breach limit and,
// if not, reserves it. Returns (allowed, reservation, currentSpend, error).
// When not allowed, the returned Reservation is zero-valued and currentSpend reflects
// the spend that caused the block.
func (t *Tracker) CheckAndReserve(ctx context.Context, agentID, provider string, limit, amount float64, window time.Duration) (bool, Reservation, float64, error) {
	k := key(agentID, provider, window)
	// Keep the key alive for 2× the window (min 1s) to survive bucket rollovers.
	ttlSec := int(window.Seconds()) * 2
	if ttlSec < 1 {
		ttlSec = 1
	}

	res, err := checkAndReserve.Run(ctx, t.rdb, []string{k},
		strconv.FormatFloat(limit, 'f', -1, 64),
		strconv.FormatFloat(amount, 'f', -1, 64),
		ttlSec,
	).Slice()
	if err != nil {
		return false, Reservation{}, 0, fmt.Errorf("redis check-and-reserve: %w", err)
	}
	if len(res) != 2 {
		return false, Reservation{}, 0, fmt.Errorf("redis check-and-reserve: unexpected reply %v", res)
	}

	allowed := toInt64(res[0]) == 1
	value, err := strconv.ParseFloat(fmt.Sprint(res[1]), 64)
	if err != nil {
		return false, Reservation{}, 0, fmt.Errorf("redis check-and-reserve: parse value %q: %w", res[1], err)
	}

	if !allowed {
		return false, Reservation{}, value, nil
	}
	return true, Reservation{Key: k, Amount: amount}, value, nil
}

// Reconcile adjusts a reservation to the actual amount on its original bucket.
// If actual < reserved it refunds the difference; if actual > reserved it debits more.
func (t *Tracker) Reconcile(ctx context.Context, res Reservation, actual float64) error {
	delta := actual - res.Amount
	if math.Abs(delta) < 0.000001 {
		return nil
	}
	if err := t.rdb.IncrByFloat(ctx, res.Key, delta).Err(); err != nil {
		return fmt.Errorf("redis reconcile: %w", err)
	}
	return nil
}

// Release fully refunds a reservation on its original bucket. Used to roll back a
// reservation when a later policy in the same request blocks the call.
func (t *Tracker) Release(ctx context.Context, res Reservation) error {
	if res.Amount == 0 {
		return nil
	}
	if err := t.rdb.IncrByFloat(ctx, res.Key, -res.Amount).Err(); err != nil {
		return fmt.Errorf("redis release: %w", err)
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

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	default:
		return 0
	}
}
