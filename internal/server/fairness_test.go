package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestClientIPTrustedProxy(t *testing.T) {
	trusted := parseCIDRs([]string{"10.0.0.0/8"})
	// Peer is the trusted proxy → trust the rightmost X-Forwarded-For entry.
	r := httptest.NewRequest("POST", "/ui/submit", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.7")
	if got := clientIP(r, trusted); got != "203.0.113.7" {
		t.Errorf("trusted proxy: clientIP = %q, want 203.0.113.7", got)
	}

	// Peer is NOT trusted → ignore the (possibly spoofed) header, use the peer.
	r2 := httptest.NewRequest("POST", "/ui/submit", nil)
	r2.RemoteAddr = "8.8.8.8:1234"
	r2.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := clientIP(r2, trusted); got != "8.8.8.8" {
		t.Errorf("untrusted peer: clientIP = %q, want 8.8.8.8", got)
	}
}

func TestIPLimiterBurstThenThrottle(t *testing.T) {
	l := newIPLimiter(60, 3) // 1/sec, burst 3
	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}
	if l.allow("1.2.3.4") {
		t.Errorf("4th request should be throttled")
	}
	// A different IP has its own bucket.
	if !l.allow("5.6.7.8") {
		t.Errorf("distinct IP should be allowed")
	}
	// Disabled limiter allows everything.
	off := newIPLimiter(0, 0)
	for i := 0; i < 100; i++ {
		if !off.allow("x") {
			t.Fatalf("disabled limiter should allow all")
		}
	}
}

// monotonicNow returns a nowFn producing strictly-increasing unix seconds, so
// created_at ordering is deterministic in tests.
func monotonicNow() func() int64 {
	var n int64
	return func() int64 { n++; return n }
}

func TestQueueListAndGC(t *testing.T) {
	q, err := OpenQueue(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	q.nowFn = monotonicNow()
	ctx := context.Background()

	// Two terminal (done) jobs and one still-queued job.
	oldID, _ := q.Enqueue(ctx, KindLocus, "s", "", "1.1.1.1", []byte("a"))
	newID, _ := q.Enqueue(ctx, KindLocus, "s", "", "2.2.2.2", []byte("b"))
	queuedID, _ := q.Enqueue(ctx, KindLocus, "s", "", "3.3.3.3", []byte("c"))
	// Mark the first two finished at t=10 and t=100.
	if _, err := q.db.Exec(`UPDATE job SET status=?, finished_at=? WHERE id=?`, StatusDone, 10, oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.db.Exec(`UPDATE job SET status=?, finished_at=? WHERE id=?`, StatusDone, 100, newID); err != nil {
		t.Fatal(err)
	}

	// List by status.
	done, err := q.List(ctx, StatusDone, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 {
		t.Fatalf("List(done) = %d jobs, want 2", len(done))
	}
	if q, _ := q.List(ctx, StatusQueued, 50, 0); len(q) != 1 {
		t.Fatalf("List(queued) = %d, want 1", len(q))
	}

	// GC with cutoff=50 removes only the old done job; leaves the recent done and
	// the still-queued job untouched.
	n, err := q.DeleteOlderThan(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("DeleteOlderThan removed %d, want 1", n)
	}
	if _, ok, _ := q.Get(ctx, oldID); ok {
		t.Errorf("old done job should be gone")
	}
	if _, ok, _ := q.Get(ctx, newID); !ok {
		t.Errorf("recent done job should remain")
	}
	if _, ok, _ := q.Get(ctx, queuedID); !ok {
		t.Errorf("queued job must never be GC'd")
	}
	// Input + result blobs of the GC'd job are gone too.
	if _, ok, _ := q.Result(ctx, oldID); ok {
		t.Errorf("GC'd job result blob should be gone")
	}
}

func TestFairClaimRoundRobin(t *testing.T) {
	q, err := OpenQueue(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	q.nowFn = monotonicNow()
	q.SetMaxJobsPerIP(0) // unlimited running, so only fairness ordering is exercised
	ctx := context.Background()

	// IP A enqueues 3 jobs before IP B enqueues 3.
	for i := 0; i < 3; i++ {
		q.Enqueue(ctx, KindLocus, "s", "", "10.0.0.1", []byte("a"))
	}
	for i := 0; i < 3; i++ {
		q.Enqueue(ctx, KindLocus, "s", "", "10.0.0.2", []byte("b"))
	}

	// Claim (without completing) — jobs stay running, deprioritizing the busier IP.
	var seq []string
	for i := 0; i < 6; i++ {
		job, _, ok, err := q.claimNext(ctx)
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		seq = append(seq, job.ClientIP)
	}
	// Despite A being enqueued entirely first, fair scheduling must interleave.
	alt := 0
	for i := 1; i < len(seq); i++ {
		if seq[i] != seq[i-1] {
			alt++
		}
	}
	if alt < 4 {
		t.Errorf("expected round-robin interleaving across IPs, got sequence %v", seq)
	}
}

func TestFairClaimPerIPCap(t *testing.T) {
	q, err := OpenQueue(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	q.nowFn = monotonicNow()
	q.SetMaxJobsPerIP(1)
	ctx := context.Background()

	// Two IPs, two jobs each; cap of 1 running per IP.
	q.Enqueue(ctx, KindLocus, "s", "", "10.0.0.1", []byte("a"))
	q.Enqueue(ctx, KindLocus, "s", "", "10.0.0.1", []byte("a"))
	q.Enqueue(ctx, KindLocus, "s", "", "10.0.0.2", []byte("b"))
	q.Enqueue(ctx, KindLocus, "s", "", "10.0.0.2", []byte("b"))

	// First two claims: one per IP. Third: both IPs at cap → nothing claimable.
	if _, _, ok, _ := q.claimNext(ctx); !ok {
		t.Fatal("claim 1 should succeed")
	}
	if _, _, ok, _ := q.claimNext(ctx); !ok {
		t.Fatal("claim 2 should succeed")
	}
	if _, _, ok, _ := q.claimNext(ctx); ok {
		t.Errorf("claim 3 should be blocked — both IPs at their per-IP cap")
	}
}

func TestOpsEndpoints(t *testing.T) {
	s := testServer(t)

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/version", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/version status = %d", rec.Code)
	}

	// /v1/jobs requires a token.
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/jobs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/v1/jobs without token = %d, want 401", rec.Code)
	}

	tok, _ := MintToken(s.cfg.Server.MasterKey, 0)
	req := httptest.NewRequest("GET", "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/v1/jobs with token = %d, want 200", rec.Code)
	}
}

func TestRequireTokenFalseOpensV1(t *testing.T) {
	s := testServer(t)
	no := false
	s.cfg.Server.RequireToken = &no

	// /v1 is reachable with no Authorization header.
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/jobs", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/v1/jobs with require_token=false = %d, want 200 (open)", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/annotations", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/v1/annotations with require_token=false = %d, want 200", rec.Code)
	}

	// Default (nil) still requires a token.
	s.cfg.Server.RequireToken = nil
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/jobs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/v1/jobs default = %d, want 401", rec.Code)
	}
}

func TestUIDisabled(t *testing.T) {
	s := testServer(t)
	no := false
	s.cfg.Server.UIEnabled = &no

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("form page with ui_enabled=false = %d, want 404", rec.Code)
	}
	// /healthz still works with the UI off.
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz with ui off = %d, want 200", rec.Code)
	}
}

func TestThrottle429(t *testing.T) {
	s := testServer(t)
	s.limiter = newIPLimiter(60, 1) // burst 1

	post := func() int {
		req := httptest.NewRequest("POST", "/ui/submit", nil)
		req.RemoteAddr = "9.9.9.9:1000"
		rec := httptest.NewRecorder()
		s.throttle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(); code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	if code := post(); code != http.StatusTooManyRequests {
		t.Errorf("second request = %d, want 429", code)
	}
}
