package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRegistry() *registry {
	return newRegistry(256<<20, testMaxMsgBytes, 50000, 64)
}

func TestSnapshotRoundTrip(t *testing.T) {
	r := testRegistry()
	dq := mustGet(t, r, "")
	_ = dq.enqueue([]byte("one"))
	_ = dq.enqueue([]byte("two"))
	jq := mustGet(t, r, "jobs")
	_ = jq.enqueue([]byte("job-1"))

	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := r.persist(path); err != nil {
		t.Fatalf("persist: %v", err)
	}

	r2 := testRegistry()
	if err := r2.load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	dq2 := mustGet(t, r2, "")
	if dq2.length() != 2 {
		t.Fatalf("default queue len=%d want 2", dq2.length())
	}
	b, _ := dq2.dequeue()
	if string(b) != "one" {
		t.Fatalf("FIFO order lost: got %q", b)
	}
	jq2 := mustGet(t, r2, "jobs")
	b, _ = jq2.dequeue()
	if string(b) != "job-1" {
		t.Fatalf("named queue lost: got %q", b)
	}
}

func TestSnapshotIncludesLeased(t *testing.T) {
	r := testRegistry()
	q := mustGet(t, r, "")
	_ = q.enqueue([]byte("in-flight"))
	_ = q.enqueue([]byte("visible"))
	_, _, _ = q.take(time.Minute) // lease "in-flight", never ack

	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := r.persist(path); err != nil {
		t.Fatalf("persist: %v", err)
	}
	r2 := testRegistry()
	if err := r2.load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	q2 := mustGet(t, r2, "")
	if q2.length() != 2 {
		t.Fatalf("unacked lease lost in snapshot: len=%d", q2.length())
	}
}

func TestSnapshotPreservesEnqueueTime(t *testing.T) {
	r := testRegistry()
	q := mustGet(t, r, "")
	_ = q.enqueue([]byte("aged"))
	time.Sleep(30 * time.Millisecond)

	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := r.persist(path); err != nil {
		t.Fatalf("persist: %v", err)
	}
	r2 := testRegistry()
	if err := r2.load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if age := mustGet(t, r2, "").stats(time.Now()).headAgeSec; age < 0.03 {
		t.Fatalf("head age lost across snapshot: %f", age)
	}
}

func TestSnapshotDetectsCorruption(t *testing.T) {
	r := testRegistry()
	_ = mustGet(t, r, "").enqueue([]byte("payload"))
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := r.persist(path); err != nil {
		t.Fatalf("persist: %v", err)
	}
	data, _ := os.ReadFile(path)
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testRegistry().load(path); err == nil {
		t.Fatal("corrupt snapshot loaded without error")
	}
}

func TestSnapshotTruncated(t *testing.T) {
	r := testRegistry()
	_ = mustGet(t, r, "").enqueue([]byte("payload"))
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := r.persist(path); err != nil {
		t.Fatalf("persist: %v", err)
	}
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, data[:len(data)-6], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testRegistry().load(path); err == nil {
		t.Fatal("truncated snapshot loaded without error")
	}
}

func TestSnapshotLegacyJSON(t *testing.T) {
	legacy, _ := json.Marshal([][]byte{[]byte("old-1"), []byte("old-2")})
	path := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	r := testRegistry()
	if err := r.load(path); err != nil {
		t.Fatalf("legacy load: %v", err)
	}
	q := mustGet(t, r, "")
	if q.length() != 2 {
		t.Fatalf("legacy snapshot len=%d want 2", q.length())
	}
	b, _ := q.dequeue()
	if string(b) != "old-1" {
		t.Fatalf("legacy order lost: %q", b)
	}
}

func TestSnapshotRejectsOverCapacity(t *testing.T) {
	r := testRegistry()
	q := mustGet(t, r, "")
	_ = q.enqueue([]byte("a"))
	_ = q.enqueue([]byte("b"))
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := r.persist(path); err != nil {
		t.Fatalf("persist: %v", err)
	}
	small := newRegistry(256<<20, testMaxMsgBytes, 1, 64)
	if err := small.load(path); err == nil {
		t.Fatal("expected error loading snapshot over message capacity")
	}
	tiny := newRegistry(1, testMaxMsgBytes, 50000, 64)
	if err := tiny.load(path); err == nil {
		t.Fatal("expected error loading snapshot over byte cap")
	}
}

func TestSnapshotLoadReplacesEverything(t *testing.T) {
	src := testRegistry()
	_ = mustGet(t, src, "").enqueue([]byte("fresh"))
	path := filepath.Join(t.TempDir(), "snap.bin")
	if err := src.persist(path); err != nil {
		t.Fatalf("persist: %v", err)
	}

	r := testRegistry()
	_ = mustGet(t, r, "stale-queue").enqueue([]byte("stale"))
	if err := r.load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	names := make([]string, 0)
	for _, e := range r.all() {
		names = append(names, e.name)
	}
	if len(names) != 1 || names[0] != defaultQueueName {
		t.Fatalf("stale queues survived load: %v", names)
	}
}

func TestSnapshotMissingFile(t *testing.T) {
	if err := testRegistry().load(filepath.Join(t.TempDir(), "nope.bin")); !os.IsNotExist(err) {
		t.Fatalf("want not-exist error, got %v", err)
	}
}

func TestPersistLeavesNoTempFile(t *testing.T) {
	r := testRegistry()
	_ = mustGet(t, r, "").enqueue([]byte("x"))
	dir := t.TempDir()
	if err := r.persist(filepath.Join(dir, "snap.bin")); err != nil {
		t.Fatalf("persist: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}
