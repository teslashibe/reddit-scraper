package redditscraper

import (
	"sync"
	"testing"
	"time"
)

// TestPickRoundRobin verifies that pick() distributes calls evenly
// across healthy clients. The pre-fix picker had a "first non-dead
// client wins on tie" bug that caused all concurrent callers to
// stampede onto a single client until its remaining budget diverged.
func TestPickRoundRobin(t *testing.T) {
	pool := &clientPool{}
	const n = 4
	for i := 0; i < n; i++ {
		pool.clients = append(pool.clients, &client{
			remaining:     100,
			minRequestGap: 10 * time.Millisecond,
		})
	}

	counts := make(map[*client]int)
	const calls = 400
	for i := 0; i < calls; i++ {
		c := pool.pick()
		counts[c]++
	}

	if len(counts) != n {
		t.Fatalf("expected pick to spread across all %d clients, got %d distinct", n, len(counts))
	}
	expected := calls / n
	tolerance := expected / 4 // ±25%
	for c, got := range counts {
		if got < expected-tolerance || got > expected+tolerance {
			t.Errorf("client %p got %d picks, expected ~%d (±%d)", c, got, expected, tolerance)
		}
	}
}

// TestPickSkipsDead ensures dead clients never get picked.
func TestPickSkipsDead(t *testing.T) {
	pool := &clientPool{}
	for i := 0; i < 3; i++ {
		pool.clients = append(pool.clients, &client{remaining: 100})
	}
	pool.clients[1].dead = true

	for i := 0; i < 50; i++ {
		c := pool.pick()
		if c == pool.clients[1] {
			t.Fatal("pick returned a dead client")
		}
	}
}

// TestPickFallsBackWhenAllInBackoff verifies we don't return nil when
// every client is currently backed off — we pick the one whose backoff
// expires soonest so the caller can still make forward progress.
func TestPickFallsBackWhenAllInBackoff(t *testing.T) {
	pool := &clientPool{}
	now := time.Now()
	pool.clients = append(pool.clients,
		&client{backoffUntil: now.Add(60 * time.Second)},
		&client{backoffUntil: now.Add(5 * time.Second)},
		&client{backoffUntil: now.Add(30 * time.Second)},
	)

	c := pool.pick()
	if c != pool.clients[1] {
		t.Errorf("expected fallback to client with earliest backoff (idx 1), got different client")
	}
}

// TestWaitForRateLimitReleasesLock is the regression test for the
// real performance bug. Before the fix, c.mu was held across
// time.Sleep, so concurrent callers serialized 1-by-1 behind the
// minRequestGap. After the fix, each caller reserves its own slot
// under the lock, releases, then sleeps independently — so 4
// concurrent calls with a 100ms gap finish in ~400ms total wall
// time (4 * gap), not 4 * (gap + lock-wait).
//
// We assert that elapsed <= ~1.5 * sequential time, which fails
// noticeably under the old serialized-lock behaviour where every
// caller waits for the previous one's full sleep.
func TestWaitForRateLimitReleasesLock(t *testing.T) {
	const (
		gap     = 100 * time.Millisecond
		callers = 4
	)
	c := &client{minRequestGap: gap}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.waitForRateLimit()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Reservation pattern: caller k waits k*gap.
	// Total wall = (callers-1) * gap (last reservation), plus minor scheduling.
	// Generous upper bound = callers * gap.
	upper := time.Duration(callers) * gap
	if elapsed > upper {
		t.Errorf("waitForRateLimit elapsed %v > upper bound %v — lock likely held across Sleep", elapsed, upper)
	}

	// And sanity: the last caller must have waited at least (callers-1)*gap
	// to actually preserve per-client rate limiting.
	minWall := time.Duration(callers-1) * gap
	if elapsed < minWall {
		t.Errorf("waitForRateLimit elapsed %v < %v — slot reservation broken (callers not spaced)", elapsed, minWall)
	}
}

// TestFetchMoreCommentsParallel sanity-checks that fetchMoreComments
// honours its budget under load. We can't easily mock the pool's
// HTTP layer here, but we can verify the early-exit path: an empty
// id list returns immediately and zero workers are spun up.
func TestFetchMoreCommentsEmpty(t *testing.T) {
	s := &Scraper{
		pool: &clientPool{clients: []*client{{}}},
	}
	got, err := s.fetchMoreComments("test", "abc", nil)
	if err != nil {
		t.Fatalf("expected nil error for empty ids, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil comments for empty ids, got %v", got)
	}
}

// TestPoolSnapshot verifies Snapshot returns one entry per client
// and reflects per-client state.
func TestPoolSnapshot(t *testing.T) {
	pool := &clientPool{
		clients: []*client{
			{remaining: 100, isProxied: false},
			{remaining: 50, isProxied: true, dead: true},
			{remaining: 75, isProxied: true, consecutiveErr: 2},
		},
	}
	snap := pool.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(snap))
	}
	if snap[0].IsProxied || !snap[1].IsProxied || !snap[2].IsProxied {
		t.Error("IsProxied did not round-trip")
	}
	if !snap[1].Dead {
		t.Error("Dead did not round-trip")
	}
	if snap[2].ConsecutiveErr != 2 {
		t.Error("ConsecutiveErr did not round-trip")
	}
	if snap[0].Remaining != 100 || snap[1].Remaining != 50 || snap[2].Remaining != 75 {
		t.Error("Remaining did not round-trip")
	}
}
