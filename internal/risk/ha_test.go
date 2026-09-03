package risk

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

func leaseStores(t *testing.T) map[string]LeaseStore {
	t.Helper()
	return map[string]LeaseStore{
		"memory": NewMemoryLeaseStore(),
		"file":   NewFileLeaseStore(filepath.Join(t.TempDir(), "lease.json")),
	}
}

// TestLeaseStoreSemantics runs the same table against both implementations, so
// the in-memory store cannot quietly diverge from the one that survives a
// restart.
func TestLeaseStoreSemantics(t *testing.T) {
	for name, store := range leaseStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			t0 := domain.SeedTime
			ttl := 10 * time.Second

			if l, err := store.Get(ctx); err != nil || l.Holder != "" {
				t.Fatalf("empty store returned %+v, %v", l, err)
			}

			a, err := store.Acquire(ctx, "node-a", t0, ttl)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			if a.Holder != "node-a" || a.Token != 1 {
				t.Fatalf("first lease = %+v, want node-a at token 1", a)
			}

			// A second node cannot take a live lease.
			if _, err := store.Acquire(ctx, "node-b", t0.Add(time.Second), ttl); !errors.Is(err, ErrLeaseHeld) {
				t.Errorf("err = %v, want ErrLeaseHeld", err)
			}
			// The holder renewing keeps its token.
			r, err := store.Renew(ctx, "node-a", a.Token, t0.Add(time.Second), ttl)
			if err != nil {
				t.Fatalf("Renew: %v", err)
			}
			if r.Token != a.Token {
				t.Errorf("renew changed the token: %d -> %d", a.Token, r.Token)
			}
			// A stale token cannot renew.
			if _, err := store.Renew(ctx, "node-a", a.Token+5, t0.Add(2*time.Second), ttl); !errors.Is(err, ErrNotLeader) {
				t.Errorf("renew with a wrong token: err = %v, want ErrNotLeader", err)
			}
			// Nor can a node that never held it.
			if _, err := store.Renew(ctx, "node-b", a.Token, t0.Add(2*time.Second), ttl); !errors.Is(err, ErrNotLeader) {
				t.Errorf("renew by a non-holder: err = %v, want ErrNotLeader", err)
			}

			// Once expired, the standby takes it with a strictly higher token.
			expired := r.Expires.Add(time.Millisecond)
			b, err := store.Acquire(ctx, "node-b", expired, ttl)
			if err != nil {
				t.Fatalf("Acquire after expiry: %v", err)
			}
			if b.Token <= a.Token {
				t.Errorf("token did not increase: %d then %d", a.Token, b.Token)
			}

			// Release by the wrong holder is refused; by the right one it frees
			// the lease without rewinding the token.
			if err := store.Release(ctx, "node-a", a.Token); !errors.Is(err, ErrNotLeader) {
				t.Errorf("release by a stale holder: err = %v, want ErrNotLeader", err)
			}
			if err := store.Release(ctx, "node-b", b.Token); err != nil {
				t.Fatalf("Release: %v", err)
			}
			c, err := store.Acquire(ctx, "node-a", expired.Add(time.Second), ttl)
			if err != nil {
				t.Fatalf("Acquire after release: %v", err)
			}
			if c.Token <= b.Token {
				t.Errorf("token rewound after a release: %d then %d", b.Token, c.Token)
			}
		})
	}
}

func TestLeaseTokensAreMonotonic(t *testing.T) {
	for name, store := range leaseStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := domain.SeedTime
			ttl := time.Second
			last := uint64(0)
			for i := 0; i < 10; i++ {
				holder := "node-a"
				if i%2 == 1 {
					holder = "node-b"
				}
				l, err := store.Acquire(ctx, holder, now, ttl)
				if err != nil {
					t.Fatalf("acquire %d: %v", i, err)
				}
				if l.Token <= last {
					t.Fatalf("token %d is not above %d", l.Token, last)
				}
				last = l.Token
				now = now.Add(2 * ttl) // let the lease lapse
			}
		})
	}
}

// TestFailoverFencesTheStaleLeader is the failure the whole design exists for.
//
// A leader takes the lease and writes. It then stops renewing — a pause, a GC
// stall, a partition; the log does not care which. The standby's lease expires
// out from under it, the standby acquires with a higher token and continues.
// When the old leader comes back it still believes it is the leader, and its
// write must be rejected on the token alone.
func TestFailoverFencesTheStaleLeader(t *testing.T) {
	for name, store := range leaseStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			clockA := obs.NewManualClock(domain.SeedTime)
			clockB := obs.NewManualClock(domain.SeedTime)
			ttl := 10 * time.Second

			a := NewElector(store, "node-a", clockA, nil, WithLeaseTTL(ttl))
			b := NewElector(store, "node-b", clockB, nil, WithLeaseTTL(ttl))
			log := NewEventLog()
			standby := NewStandby(log)

			// node-a wins and writes five events.
			if leader, err := a.TryAcquire(ctx); err != nil || !leader {
				t.Fatalf("node-a did not become leader: %v", err)
			}
			if leader, err := b.TryAcquire(ctx); err != nil || leader {
				t.Fatalf("node-b became leader alongside node-a: %v", err)
			}
			if !a.IsLeader() || b.IsLeader() {
				t.Fatalf("IsLeader: a=%v b=%v", a.IsLeader(), b.IsLeader())
			}
			tokenA := a.Token()
			for i := uint64(1); i <= 5; i++ {
				if err := log.Append(tokenA, transfer(i, "CP-ALPHA", "CP-BRAVO")); err != nil {
					t.Fatalf("leader append %d: %v", i, err)
				}
			}
			// The standby has been following, so it owes nothing.
			if got := standby.Catchup(); len(got) != 5 {
				t.Fatalf("standby caught up on %d events, want 5", len(got))
			}
			if standby.Applied() != 5 {
				t.Fatalf("standby applied %d, want 5", standby.Applied())
			}

			// node-a stops renewing. Time passes on node-b's clock only.
			clockB.Advance(ttl + time.Second)
			if leader, err := b.TryAcquire(ctx); err != nil || !leader {
				t.Fatalf("node-b did not take over: %v", err)
			}
			tokenB := b.Token()
			if tokenB <= tokenA {
				t.Fatalf("new leader's token %d is not above the old %d", tokenB, tokenA)
			}

			// Failover loses no work: the standby resumes at offset 5.
			if got := standby.Catchup(); len(got) != 0 {
				t.Errorf("standby replayed %d already-applied events", len(got))
			}
			if err := log.Append(tokenB, transfer(6, "CP-BRAVO", "CP-GOLF")); err != nil {
				t.Fatalf("new leader append: %v", err)
			}
			resumed := standby.Catchup()
			if len(resumed) != 1 || resumed[0].Seq != 6 {
				t.Fatalf("standby resumed with %+v, want only event 6", resumed)
			}

			// The stale leader wakes up still believing it leads, and is fenced.
			if !a.IsLeader() {
				t.Fatal("the stale leader should still believe it leads; that is the point")
			}
			err := log.Append(tokenA, transfer(7, "CP-ALPHA", "CP-DELTA"))
			if !errors.Is(err, ErrFenced) {
				t.Fatalf("stale write err = %v, want ErrFenced", err)
			}
			if log.Len() != 6 {
				t.Errorf("log length = %d, want 6 (the fenced write must not land)", log.Len())
			}

			// And it discovers it has lost the lease the next time it campaigns.
			clockA.Advance(ttl + time.Second)
			if leader, err := a.TryAcquire(ctx); err != nil {
				t.Fatalf("stale leader campaign: %v", err)
			} else if leader {
				t.Error("the stale leader reacquired a lease node-b still holds")
			}
			if a.IsLeader() || a.Token() != 0 {
				t.Errorf("stale leader still reports leadership: %v token=%d", a.IsLeader(), a.Token())
			}
			if got := log.HighestToken(); got != tokenB {
				t.Errorf("log highest token = %d, want %d", got, tokenB)
			}
		})
	}
}

func TestElectorResignHandsOver(t *testing.T) {
	store := NewMemoryLeaseStore()
	ctx := context.Background()
	clock := obs.NewManualClock(domain.SeedTime)

	var gained, lost int
	a := NewElector(store, "node-a", clock, nil, WithLeaseTTL(time.Minute),
		WithLeadershipChange(func(leader bool, _ uint64) {
			if leader {
				gained++
			} else {
				lost++
			}
		}))
	b := NewElector(store, "node-b", clock, nil, WithLeaseTTL(time.Minute))

	if leader, err := a.TryAcquire(ctx); err != nil || !leader {
		t.Fatalf("node-a: %v", err)
	}
	if leader, err := b.TryAcquire(ctx); err != nil || leader {
		t.Fatalf("node-b took a live lease: %v", err)
	}
	if err := a.Resign(ctx); err != nil {
		t.Fatalf("Resign: %v", err)
	}
	if a.IsLeader() {
		t.Error("a resigned node still reports leadership")
	}
	// The lease is free immediately, not a TTL later.
	if leader, err := b.TryAcquire(ctx); err != nil || !leader {
		t.Fatalf("node-b did not take over after a resignation: %v", err)
	}
	if gained != 1 || lost != 1 {
		t.Errorf("leadership callbacks: gained=%d lost=%d, want 1 and 1", gained, lost)
	}
	// Resigning twice is not an error.
	if err := a.Resign(ctx); err != nil {
		t.Errorf("second Resign: %v", err)
	}
}

func TestElectorCampaignShutsDownCleanly(t *testing.T) {
	settle(t)
	before := runtime.NumGoroutine()

	store := NewMemoryLeaseStore()
	e := NewElector(store, "node-a", obs.RealClock{}, nil,
		WithLeaseTTL(time.Second), WithRenewInterval(5*time.Millisecond), WithRetryInterval(5*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Campaign(ctx) }()

	deadline := time.After(2 * time.Second)
	for !e.IsLeader() {
		select {
		case <-deadline:
			t.Fatal("never became leader")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Campaign returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Campaign did not return")
	}
	if e.IsLeader() {
		t.Error("still leader after Campaign returned")
	}
	if l, err := store.Get(context.Background()); err != nil || l.Holder != "" {
		t.Errorf("lease not released on shutdown: %+v, %v", l, err)
	}

	settle(t)
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines: %d before, %d after", before, after)
	}
}

// TestElectorSingleLeaderUnderContention runs several nodes at the lease and
// checks that exactly one of them ever holds it.
func TestElectorSingleLeaderUnderContention(t *testing.T) {
	store := NewMemoryLeaseStore()
	clock := obs.NewManualClock(domain.SeedTime)
	nodes := make([]*Elector, 8)
	for i := range nodes {
		nodes[i] = NewElector(store, string(rune('a'+i)), clock, nil, WithLeaseTTL(time.Minute))
	}

	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n *Elector) {
			defer wg.Done()
			if _, err := n.TryAcquire(context.Background()); err != nil {
				t.Errorf("TryAcquire: %v", err)
			}
		}(n)
	}
	wg.Wait()

	leaders := 0
	for _, n := range nodes {
		if n.IsLeader() {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("%d leaders, want exactly 1", leaders)
	}
}

func TestEventLogFencingAndReplay(t *testing.T) {
	log := NewEventLog()
	if got := log.From(0); got != nil {
		t.Errorf("empty log returned %v", got)
	}
	for i := uint64(1); i <= 3; i++ {
		if err := log.Append(1, transfer(i, "CP-ALPHA", "CP-BRAVO")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if got := log.From(1); len(got) != 2 || got[0].Seq != 2 {
		t.Errorf("From(1) = %+v, want events 2 and 3", got)
	}
	if err := log.Append(0, transfer(4, "CP-ALPHA", "CP-BRAVO")); !errors.Is(err, ErrFenced) {
		t.Errorf("token 0 err = %v, want ErrFenced", err)
	}
	// The same token may keep writing; only a lower one is fenced.
	if err := log.Append(1, transfer(4, "CP-ALPHA", "CP-BRAVO")); err != nil {
		t.Errorf("same-token append: %v", err)
	}
	if log.Len() != 4 {
		t.Errorf("log length = %d, want 4", log.Len())
	}
}

func TestStandbyFollowStopsOnCancel(t *testing.T) {
	settle(t)
	before := runtime.NumGoroutine()

	log := NewEventLog()
	s := NewStandby(log)
	seen := make(chan int, 4)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.Follow(ctx, time.Millisecond, func(batch []domain.LedgerEvent) {
			select {
			case seen <- len(batch):
			default:
			}
		})
	}()

	if err := log.Append(1, transfer(1, "CP-ALPHA", "CP-BRAVO"), transfer(2, "CP-ALPHA", "CP-GOLF")); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-seen:
		if n == 0 {
			t.Error("follower reported an empty batch")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follower never saw the appended events")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Follow returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Follow did not return")
	}

	settle(t)
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines: %d before, %d after", before, after)
	}
}

func TestFileLeaseStoreSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.json")
	ctx := context.Background()

	first := NewFileLeaseStore(path)
	l, err := first.Acquire(ctx, "node-a", domain.SeedTime, time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// A fresh store over the same file — a restarted process — sees the lease
	// and does not reuse the token.
	second := NewFileLeaseStore(path)
	got, err := second.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Holder != "node-a" || got.Token != l.Token {
		t.Fatalf("reopened store sees %+v, want %+v", got, l)
	}
	next, err := second.Acquire(ctx, "node-b", domain.SeedTime.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("Acquire after expiry: %v", err)
	}
	if next.Token <= l.Token {
		t.Errorf("token rewound across a restart: %d then %d", l.Token, next.Token)
	}
}
