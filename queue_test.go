package main

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const testMaxMsgBytes = 128 << 10

func newTestQueue(capacity int) *queue {
	return newQueue(256<<20, testMaxMsgBytes, capacity)
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"100", 100},
		{"64K", 64 << 10},
		{"1M", 1 << 20},
		{"1G", 1 << 30},
		{" 4M ", 4 << 20},
		{"128k", 128 << 10},
		{"0", 0},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.in)
		if err != nil {
			t.Errorf("parseSize(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseSize(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseSizeInvalid(t *testing.T) {
	for _, in := range []string{"abc", "-1", "-2G", "1.5M", ""} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q): expected error", in)
		}
	}
}

func TestEnqueueDequeue(t *testing.T) {
	q := newTestQueue(50000)
	if err := q.enqueue([]byte("hi")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if l := q.length(); l != 1 {
		t.Fatalf("want len=1 got %d", l)
	}
	b, ok := q.dequeue()
	if !ok || string(b) != "hi" {
		t.Fatalf("bad dequeue got=%q ok=%v", b, ok)
	}
	if _, ok := q.dequeue(); ok {
		t.Fatal("expected empty after single dequeue")
	}
}

func TestQueueFull(t *testing.T) {
	q := newTestQueue(2)
	_ = q.enqueue([]byte("A"))
	_ = q.enqueue([]byte("B"))
	if err := q.enqueue([]byte("C")); err != errQueueFull {
		t.Fatalf("want errQueueFull got %v", err)
	}
}

func TestMaxBytesFull(t *testing.T) {
	q := newQueue(10, testMaxMsgBytes, 50000)
	_ = q.enqueue([]byte("1234567890"))
	if err := q.enqueue([]byte("x")); err != errQueueFull {
		t.Fatalf("want errQueueFull got %v", err)
	}
}

func TestPayloadTooLarge(t *testing.T) {
	q := newTestQueue(50000)
	huge := bytes.Repeat([]byte{'x'}, testMaxMsgBytes+1)
	if err := q.enqueue(huge); err != errTooLarge {
		t.Fatalf("want errTooLarge got %v", err)
	}
}

func TestQueueClear(t *testing.T) {
	q := newTestQueue(50000)
	_ = q.enqueue([]byte("one"))
	_ = q.enqueue([]byte("two"))
	_, _, _ = q.take(time.Minute) // leased messages count as dropped too
	if dropped := q.clear(); dropped != 2 {
		t.Fatalf("clear reported %d dropped, want 2", dropped)
	}
	if q.length() != 0 || q.size() != 0 {
		t.Fatalf("queue not empty after clear: len=%d bytes=%d", q.length(), q.size())
	}
}

func TestWrapAround(t *testing.T) {
	q := newTestQueue(3)
	for round := 0; round < 10; round++ {
		for i := 0; i < 3; i++ {
			if err := q.enqueue([]byte{byte(round), byte(i)}); err != nil {
				t.Fatalf("round %d enqueue %d: %v", round, i, err)
			}
		}
		for i := 0; i < 3; i++ {
			b, ok := q.dequeue()
			if !ok || b[0] != byte(round) || b[1] != byte(i) {
				t.Fatalf("round %d dequeue %d: got %v ok=%v", round, i, b, ok)
			}
		}
	}
}

// --- leases -----------------------------------------------------------------

func TestLeaseAckFlow(t *testing.T) {
	q := newTestQueue(8)
	_ = q.enqueue([]byte("work"))
	m, g, ok := q.take(time.Minute)
	if !ok || g == nil || string(m.data) != "work" {
		t.Fatalf("take with lease failed: ok=%v g=%v", ok, g)
	}
	if q.length() != 0 {
		t.Fatal("leased message still visible")
	}
	if !q.ack(g.id) {
		t.Fatal("ack of valid lease failed")
	}
	if q.ack(g.id) {
		t.Fatal("double ack succeeded")
	}
	if q.stats(time.Now()).leased != 0 {
		t.Fatal("lease not released after ack")
	}
}

func TestLeaseExpiryRequeues(t *testing.T) {
	q := newTestQueue(8)
	_ = q.enqueue([]byte("retry-me"))
	_, g, _ := q.take(10 * time.Millisecond)

	if n := q.expire(time.Now()); n != 0 {
		t.Fatalf("lease expired too early: %d", n)
	}
	time.Sleep(20 * time.Millisecond)
	if n := q.expire(time.Now()); n != 1 {
		t.Fatalf("expected 1 expiry, got %d", n)
	}
	b, ok := q.dequeue()
	if !ok || string(b) != "retry-me" {
		t.Fatalf("expired message not requeued: %q ok=%v", b, ok)
	}
	if q.ack(g.id) {
		t.Fatal("ack after expiry should fail")
	}
	if q.c.expirations.Load() != 1 {
		t.Fatalf("expirations counter = %d", q.c.expirations.Load())
	}
}

func TestLeasedCountsAgainstCapacity(t *testing.T) {
	q := newTestQueue(1)
	_ = q.enqueue([]byte("a"))
	_, g, _ := q.take(time.Minute)
	if err := q.enqueue([]byte("b")); err != errQueueFull {
		t.Fatalf("leased message should hold capacity: %v", err)
	}
	q.ack(g.id)
	if err := q.enqueue([]byte("b")); err != nil {
		t.Fatalf("enqueue after ack: %v", err)
	}
}

func TestExpiredRequeueAtFront(t *testing.T) {
	q := newTestQueue(8)
	_ = q.enqueue([]byte("first"))
	_, _, _ = q.take(time.Nanosecond)
	_ = q.enqueue([]byte("second"))
	time.Sleep(time.Millisecond)
	q.expire(time.Now())
	b, _ := q.dequeue()
	if string(b) != "first" {
		t.Fatalf("expired lease should requeue at the front, got %q", b)
	}
}

// --- batches ----------------------------------------------------------------

func TestEnqueueBatchAllOrNothing(t *testing.T) {
	q := newTestQueue(2)
	err := q.enqueueBatch([][]byte{[]byte("a"), []byte("b"), []byte("c")})
	if err != errQueueFull {
		t.Fatalf("want errQueueFull got %v", err)
	}
	if q.length() != 0 {
		t.Fatal("partial batch was enqueued")
	}
	if err := q.enqueueBatch([][]byte{[]byte("a"), []byte("b")}); err != nil {
		t.Fatalf("fitting batch rejected: %v", err)
	}
	if q.length() != 2 {
		t.Fatalf("len=%d want 2", q.length())
	}
}

func TestTakeBatch(t *testing.T) {
	q := newTestQueue(16)
	for i := 0; i < 5; i++ {
		_ = q.enqueue([]byte{byte(i)})
	}
	msgs, ids := q.takeBatch(3, 0)
	if len(msgs) != 3 || ids != nil {
		t.Fatalf("takeBatch: %d msgs, ids=%v", len(msgs), ids)
	}
	for i, m := range msgs {
		if m.data[0] != byte(i) {
			t.Fatalf("batch order broken at %d", i)
		}
	}
	msgs, ids = q.takeBatch(10, time.Minute)
	if len(msgs) != 2 || len(ids) != 2 {
		t.Fatalf("leased batch: %d msgs %d ids", len(msgs), len(ids))
	}
	if q.stats(time.Now()).leased != 2 {
		t.Fatal("batch leases not registered")
	}
}

// --- head age ---------------------------------------------------------------

func TestHeadAge(t *testing.T) {
	q := newTestQueue(8)
	if age := q.stats(time.Now()).headAgeSec; age != 0 {
		t.Fatalf("empty queue head age = %f", age)
	}
	_ = q.enqueue([]byte("old"))
	time.Sleep(30 * time.Millisecond)
	if age := q.stats(time.Now()).headAgeSec; age < 0.03 {
		t.Fatalf("head age %f, want >= 0.03", age)
	}
}

// --- waiting ----------------------------------------------------------------

func TestTakeWaitDeliversLater(t *testing.T) {
	q := newTestQueue(8)
	done := make(chan []byte, 1)
	go func() {
		m, _, ok := q.takeWait(context.Background(), 2*time.Second, 0)
		if !ok {
			done <- nil
			return
		}
		done <- m.data
	}()
	time.Sleep(50 * time.Millisecond)
	if err := q.enqueue([]byte("late")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case m := <-done:
		if string(m) != "late" {
			t.Fatalf("got %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never woke up")
	}
}

func TestTakeWaitTimeout(t *testing.T) {
	q := newTestQueue(8)
	start := time.Now()
	_, _, ok := q.takeWait(context.Background(), 50*time.Millisecond, 0)
	if ok {
		t.Fatal("expected timeout on empty queue")
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("returned before the wait elapsed")
	}
}

func TestTakeWaitContextCancel(t *testing.T) {
	q := newTestQueue(8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, _, ok := q.takeWait(ctx, 5*time.Second, 0)
		done <- ok
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("expected no message after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not honor context cancellation")
	}
}

func TestTakeWaitMultipleWaiters(t *testing.T) {
	q := newTestQueue(8)
	const waiters = 4
	results := make(chan bool, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			_, _, ok := q.takeWait(context.Background(), 3*time.Second, 0)
			results <- ok
		}()
	}
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < waiters; i++ {
		if err := q.enqueue([]byte("m")); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	for i := 0; i < waiters; i++ {
		select {
		case ok := <-results:
			if !ok {
				t.Fatal("a waiter timed out despite available messages")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a waiter never returned")
		}
	}
}

func TestConcurrentSafety(t *testing.T) {
	q := newTestQueue(50000)
	const total = 1000
	var enq, deq sync.WaitGroup
	var mu sync.Mutex
	var dequeued, enqueued int64

	enq.Add(100)
	for i := 0; i < 100; i++ {
		go func() {
			defer enq.Done()
			for j := 0; j < total/100; j++ {
				if err := q.enqueue([]byte("x")); err != nil {
					t.Errorf("enqueue err: %v", err)
					return
				}
				mu.Lock()
				enqueued++
				mu.Unlock()
			}
		}()
	}

	deq.Add(100)
	for i := 0; i < 100; i++ {
		go func() {
			defer deq.Done()
			for {
				if _, ok := q.dequeue(); ok {
					mu.Lock()
					dequeued++
					mu.Unlock()
					continue
				}
				mu.Lock()
				finished := dequeued >= total
				mu.Unlock()
				if finished {
					return
				}
				time.Sleep(time.Microsecond)
			}
		}()
	}

	enq.Wait()
	deq.Wait()
	if enqueued != total || dequeued != total || q.length() != 0 {
		t.Fatalf("mismatch enq=%d deq=%d len=%d", enqueued, dequeued, q.length())
	}
}

// --- registry ---------------------------------------------------------------

func TestRegistryGetCreates(t *testing.T) {
	r := newRegistry(1<<20, 1<<10, 16, 4)
	q1, err := r.get("jobs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	q2, err := r.get("jobs")
	if err != nil || q1 != q2 {
		t.Fatal("get did not return the same queue")
	}
	if d, err := r.get(""); err != nil || d != mustGet(t, r, defaultQueueName) {
		t.Fatal("empty name should resolve to the default queue")
	}
}

func mustGet(t *testing.T, r *registry, name string) *queue {
	t.Helper()
	q, err := r.get(name)
	if err != nil {
		t.Fatalf("get(%q): %v", name, err)
	}
	return q
}

func TestRegistryRejectsBadNames(t *testing.T) {
	r := newRegistry(1<<20, 1<<10, 16, 4)
	for _, name := range []string{"a b", "x/y", "..;", string(bytes.Repeat([]byte{'a'}, 65)), "ü"} {
		if _, err := r.get(name); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestRegistryMaxQueues(t *testing.T) {
	r := newRegistry(1<<20, 1<<10, 16, 2) // default counts as one
	if _, err := r.get("second"); err != nil {
		t.Fatalf("second queue: %v", err)
	}
	if _, err := r.get("third"); err != errTooManyQueues {
		t.Fatalf("want errTooManyQueues got %v", err)
	}
	// existing queues still resolve at the limit
	if _, err := r.get("second"); err != nil {
		t.Fatalf("existing queue blocked: %v", err)
	}
}

// --- config -----------------------------------------------------------------

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.addr != ":8080" || cfg.maxBytes != 256<<20 || cfg.maxMsgs != 50000 ||
		cfg.maxMsgBytes != 128<<10 || cfg.maxQueues != 64 || cfg.maxWaiters != 1024 ||
		cfg.maxLease != 5*time.Minute {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigFlagsBeatEnv(t *testing.T) {
	t.Setenv("QUEUE_MAX_BYTES", "1G")
	cfg, err := loadConfig([]string{"--max-bytes", "2G"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.maxBytes != 2<<30 {
		t.Fatalf("flag did not override env: %d", cfg.maxBytes)
	}
}

func TestLoadConfigEnvTokens(t *testing.T) {
	t.Setenv("ZAPQ_API_TOKEN", "a")
	t.Setenv("ZAPQ_PRODUCER_TOKEN", "p")
	t.Setenv("ZAPQ_CONSUMER_TOKEN", "c")
	t.Setenv("ZAPQ_ADMIN_TOKEN", "adm")
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := tokens{api: "a", producer: "p", consumer: "c", admin: "adm"}
	if cfg.tokens != want {
		t.Fatalf("tokens not applied: %+v", cfg.tokens)
	}
}

func TestLoadConfigRejectsInvalid(t *testing.T) {
	cases := [][]string{
		{"--max-msgs", "-1"},
		{"--max-bytes", "-5M"},
		{"--max-msg-bytes", "1G", "--max-bytes", "1M"},
		{"--max-queues", "0"},
		{"--max-waiters", "0"},
		{"--max-lease", "10ms"},
		{"--snapshot-interval", "30s"}, // no snapshot-file
		{"--tls-cert", "cert.pem"},     // no key
		{"--log-level", "verbose"},
	}
	for _, args := range cases {
		if _, err := loadConfig(args); err == nil {
			t.Errorf("loadConfig(%v): expected error", args)
		}
	}
}

func TestLoadConfigInvalidEnv(t *testing.T) {
	t.Setenv("QUEUE_MAX_BYTES", "banana")
	if _, err := loadConfig(nil); err == nil {
		t.Fatal("expected error for invalid QUEUE_MAX_BYTES")
	}
}

func TestSnapshotFileResolvedInDataDir(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfig([]string{"--data-dir", dir, "--snapshot-file", "q.snap"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.snapshotFile != filepath.Join(dir, "q.snap") {
		t.Fatalf("snapshot file not resolved into data dir: %s", cfg.snapshotFile)
	}
}
