package main

import (
	"bytes"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

const smallBody = "hi"

func resetGlobals() {
	atomic.StoreUint64(&enqueueCnt, 0)
	atomic.StoreUint64(&dequeueCnt, 0)
	atomic.StoreUint64(&dequeueMiss, 0)
}

func TestEnqueueDequeue(t *testing.T) {
	resetGlobals()
	q := &queue{}
	if err := q.enqueue([]byte(smallBody)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if l := q.length(); l != 1 {
		t.Fatalf("want len=1 got %d", l)
	}
	b, ok := q.dequeue()
	if !ok || string(b) != smallBody {
		t.Fatalf("bad dequeue got=%q ok=%v", b, ok)
	}
	if _, ok := q.dequeue(); ok {
		t.Fatalf("expected empty after single dequeue")
	}
	if dequeueMiss != 1 {
		t.Fatalf("dequeueMiss counter want 1 got %d", dequeueMiss)
	}
}

func TestQueueFull(t *testing.T) {
	resetGlobals()
	old := maxMsgs
	maxMsgs = 2
	defer func() { maxMsgs = old }()
	q := &queue{}

	_ = q.enqueue([]byte("A"))
	_ = q.enqueue([]byte("B"))

	if err := q.enqueue([]byte("C")); err == nil {
		t.Fatal("expected queue full error")
	}
}

func TestPayloadTooLarge(t *testing.T) {
	resetGlobals()
	q := &queue{}
	huge := bytes.Repeat([]byte{'x'}, maxBody+1)
	if err := q.enqueue(huge); err == nil {
		t.Fatal("want payload too large error")
	}
}

func TestPersistAndLoad(t *testing.T) {
	resetGlobals()
	q := &queue{}
	_ = q.enqueue([]byte("one"))
	_ = q.enqueue([]byte("two"))

	tmp, err := os.CreateTemp("", "zapq*.json")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	if err := q.persist(tmp.Name()); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// fresh queue, load snapshot
	q2 := &queue{}
	if err := q2.load(tmp.Name()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if q2.length() != 2 || q2.size() != q.size() {
		t.Fatalf("load mismatch")
	}
}

func TestConcurrentSafety(t *testing.T) {
	resetGlobals()
	q := &queue{}
	const total = 1000
	var wg sync.WaitGroup
	wg.Add(200) // 100 producers + 100 consumers

	// producers
	for i := 0; i < 100; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < total/100; j++ {
				if err := q.enqueue([]byte("x")); err != nil {
					t.Errorf("enqueue err: %v", err)
				}
			}
		}()
	}

	// consumers
	for i := 0; i < 100; i++ {
		go func() {
			defer wg.Done()
			for {
				_, ok := q.dequeue()
				if !ok && atomic.LoadUint64(&dequeueCnt) >= total {
					return
				}
			}
		}()
	}

	wg.Wait()
	if enqueueCnt != total || dequeueCnt != total || q.length() != 0 {
		t.Fatalf("counters mismatch enq=%d deq=%d len=%d",
			enqueueCnt, dequeueCnt, q.length())
	}
}
