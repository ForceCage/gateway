package budget

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testTracker(t *testing.T) *Tracker {
	t.Helper()
	opts, _ := redis.ParseURL("redis://localhost:6379")
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	rdb.FlushDB(context.Background())
	return New(rdb)
}

// Allowed requests must return cleanly (regression for the Int64Slice float-parse bug).
func TestCheckAndReserve_AllowReturnsValue(t *testing.T) {
	tr := testTracker(t)
	ctx := context.Background()

	allowed, res, spend, err := tr.CheckAndReserve(ctx, "a", "openai", 50.0, 0.0123, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatalf("expected allowed")
	}
	if res.Amount != 0.0123 {
		t.Fatalf("reservation amount = %v, want 0.0123", res.Amount)
	}
	if spend < 0.0122 || spend > 0.0124 {
		t.Fatalf("spend = %v, want ~0.0123", spend)
	}
}

func TestCheckAndReserve_BlocksOverLimit(t *testing.T) {
	tr := testTracker(t)
	ctx := context.Background()

	tr.CheckAndReserve(ctx, "a", "openai", 1.0, 0.9, time.Hour)
	allowed, _, spend, err := tr.CheckAndReserve(ctx, "a", "openai", 1.0, 0.9, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatalf("expected block: 0.9 + 0.9 > 1.0")
	}
	if spend < 0.89 || spend > 0.91 {
		t.Fatalf("reported spend = %v, want ~0.9", spend)
	}
}

// Release rolls back a reservation fully (regression for the partial-reservation leak).
func TestRelease_RefundsReservation(t *testing.T) {
	tr := testTracker(t)
	ctx := context.Background()

	_, res, _, _ := tr.CheckAndReserve(ctx, "a", "openai", 50.0, 0.10, time.Hour)
	if err := tr.Release(ctx, res); err != nil {
		t.Fatalf("release: %v", err)
	}
	spend, _ := tr.CurrentSpend(ctx, "a", "openai", time.Hour)
	if spend > 0.0000001 || spend < -0.0000001 {
		t.Fatalf("spend after release = %v, want 0", spend)
	}
}

// Reconcile must target the bucket captured at reserve time, not a recomputed one
// (regression for window-boundary corruption).
func TestReconcile_UsesCapturedBucket(t *testing.T) {
	tr := testTracker(t)
	ctx := context.Background()

	_, res, _, _ := tr.CheckAndReserve(ctx, "a", "openai", 50.0, 0.10, time.Second)
	time.Sleep(1100 * time.Millisecond) // cross the window boundary

	// Refund 0.05 (reserved 0.10, actual 0.05) — must land on the original bucket.
	if err := tr.Reconcile(ctx, res, 0.05); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The original bucket should now read 0.05; the current (new) window stays clean.
	newWindow, _ := tr.CurrentSpend(ctx, "a", "openai", time.Second)
	if newWindow < 0 {
		t.Fatalf("new window corrupted to %v", newWindow)
	}
}
