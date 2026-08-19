package guard

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterConcurrencyGate(t *testing.T) {
	l := NewLimiter(2, 0)
	ctx := context.Background()

	if err := l.Acquire(ctx, "k", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := l.Acquire(ctx, "k", time.Second); err != nil {
		t.Fatal(err)
	}
	if l.InFlight("k") != 2 {
		t.Fatalf("in-flight = %d", l.InFlight("k"))
	}
	// 第三个:排队超时。
	if err := l.Acquire(ctx, "k", 100*time.Millisecond); err != ErrQueueTimeout {
		t.Fatalf("expected queue timeout, got %v", err)
	}
	// 释放一个后可再进。
	l.Release("k")
	if err := l.Acquire(ctx, "k", time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestLimiterConcurrencyGateParallel(t *testing.T) {
	// 端到端:并发 2 时,同时最多 2 个 goroutine 处于临界区。
	l := NewLimiter(2, 0)
	var inflight, maxInflight int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Acquire(context.Background(), "k", 5*time.Second); err != nil {
				t.Error(err)
				return
			}
			defer l.Release("k")
			cur := atomic.AddInt64(&inflight, 1)
			for {
				old := atomic.LoadInt64(&maxInflight)
				if cur <= old || atomic.CompareAndSwapInt64(&maxInflight, old, cur) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt64(&inflight, -1)
		}()
	}
	wg.Wait()
	if maxInflight > 2 {
		t.Fatalf("max in-flight = %d, want <= 2", maxInflight)
	}
}

func TestLimiterRPM(t *testing.T) {
	l := NewLimiter(0, 3)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := l.Acquire(ctx, "k", time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if l.RPMCount("k") != 3 {
		t.Fatalf("rpm = %d", l.RPMCount("k"))
	}
	// 第 4 个:RPM 满排队 → 超时。
	if err := l.Acquire(ctx, "k", 80*time.Millisecond); err != ErrQueueTimeout {
		t.Fatalf("expected rpm queue timeout, got %v", err)
	}
}

func TestSessionPool(t *testing.T) {
	p := NewSessionPool("", 3, 50*time.Millisecond, 100*time.Millisecond)

	// 同 seed 稳定;不同 seed 可能落不同槽。
	a1 := p.Assign("k", "seed-a")
	a2 := p.Assign("k", "seed-a")
	if a1 != a2 {
		t.Fatalf("same seed must be stable: %s vs %s", a1, a2)
	}
	total := map[string]struct{}{a1: {}}
	for _, s := range []string{"seed-b", "seed-c", "seed-d", "seed-e"} {
		total[p.Assign("k", s)] = struct{}{}
	}
	if len(total) > 3 {
		t.Fatalf("pool must cap at 3 sessions, got %d", len(total))
	}
	if p.ActiveSessions("k") > 3 {
		t.Fatal("active sessions exceeded pool size")
	}

	// 不同 key 独立。
	b := p.Assign("other", "seed-a")
	if b == "" {
		t.Fatal("pool must assign")
	}

	// 槽位寿命到期后轮换出新 session。
	time.Sleep(120 * time.Millisecond)
	a3 := p.Assign("k", "seed-a")
	if a3 == a1 {
		t.Fatal("expired slot must rotate to a new session id")
	}

	// 未启用(size=0)返回空。
	off := NewSessionPool("", 0, time.Minute, time.Minute)
	if got := off.Assign("k", "s"); got != "" {
		t.Fatalf("disabled pool should return empty, got %s", got)
	}
}

func TestUsageTrackerBudget(t *testing.T) {
	dir := t.TempDir()
	u := NewUsageTracker(dir+"/usage.json", 1000)

	if u.Exceeded("k") {
		t.Fatal("fresh key must not exceed")
	}
	u.AddUsage("k", 600, 100, 0, 0)
	u.AddUsage("k", 0, 300, 0, 0)
	if !u.Exceeded("k") {
		t.Fatal("600+100+300 >= 1000 budget must exceed")
	}
	// 请求计数与快照。
	u.CountRequest("k")
	snap := u.SnapshotToday()
	if len(snap) != 1 || snap[0].Key != "k" || snap[0].TokensToday != 1000 || snap[0].RequestsMade != 1 {
		t.Fatalf("snapshot wrong: %+v", snap)
	}
	// 其他 key 不受影响。
	if u.Exceeded("k2") {
		t.Fatal("other key must not be affected")
	}
	// 持久化往返。
	u2 := NewUsageTracker(dir+"/usage.json", 0)
	if snap2 := u2.SnapshotToday(); len(snap2) != 1 || snap2[0].TokensToday != 1000 {
		t.Fatalf("persist roundtrip wrong: %+v", snap2)
	}
}
