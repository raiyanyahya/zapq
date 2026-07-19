package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raiyanyahya/zapq/internal/frames"
)

func TestEnqueueDequeue(t *testing.T) {
	var stored []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/enqueue":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			stored, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
		case "/dequeue":
			if stored == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Write(stored)
			stored = nil
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, WithToken("tok"))
	ctx := context.Background()
	if err := c.Enqueue(ctx, "", []byte("hello")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	data, ok, err := c.Dequeue(ctx, "", 0)
	if err != nil || !ok || string(data) != "hello" {
		t.Fatalf("dequeue: %q ok=%v err=%v", data, ok, err)
	}
	_, ok, err = c.Dequeue(ctx, "", 0)
	if err != nil || ok {
		t.Fatalf("empty dequeue: ok=%v err=%v", ok, err)
	}
}

func TestNamedQueuePaths(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := New(srv.URL, WithRetries(0))
	_ = c.Enqueue(context.Background(), "jobs", []byte("x"))
	if gotPath != "/q/jobs/enqueue" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestEnqueueRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := New(srv.URL, WithRetries(5))
	if err := c.Enqueue(context.Background(), "", []byte("x")); err != nil {
		t.Fatalf("enqueue after retries: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestEnqueueGivesUpAfterRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(srv.URL, WithRetries(1))
	if err := c.Enqueue(context.Background(), "", []byte("x")); err != ErrQueueFull {
		t.Fatalf("want ErrQueueFull got %v", err)
	}
}

func TestUnauthorizedNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, WithRetries(5))
	if err := c.Enqueue(context.Background(), "", []byte("x")); err != ErrUnauthorized {
		t.Fatalf("want ErrUnauthorized got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("401 was retried %d times", calls.Load())
	}
}

func TestDequeueLease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dequeue":
			if r.URL.Query().Get("lease") != "30s" {
				t.Errorf("lease param missing: %s", r.URL.RawQuery)
			}
			w.Header().Set("X-Lease-Id", "abc123")
			w.Header().Set("X-Lease-Expires", time.Now().Add(30*time.Second).UTC().Format(time.RFC3339Nano))
			w.Write([]byte("work"))
		case "/ack":
			if r.URL.Query().Get("id") != "abc123" {
				t.Errorf("ack id = %q", r.URL.Query().Get("id"))
			}
			w.Write([]byte(`{"acked":true}`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()
	m, ok, err := c.DequeueLease(ctx, "", 0, 30*time.Second)
	if err != nil || !ok || string(m.Data) != "work" || m.LeaseID != "abc123" || m.Expires.IsZero() {
		t.Fatalf("lease dequeue: %+v ok=%v err=%v", m, ok, err)
	}
	if err := c.Ack(ctx, "", m.LeaseID); err != nil {
		t.Fatalf("ack: %v", err)
	}
}

func TestBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/enqueue/batch":
			msgs, err := frames.Read(r.Body, 1<<20)
			if err != nil || len(msgs) != 2 {
				t.Errorf("server got batch: %v (%d)", err, len(msgs))
			}
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"enqueued":2}`))
		case "/dequeue/batch":
			w.Header().Set("X-Lease-Ids", "id1,id2")
			frames.Write(w, [][]byte{[]byte("m1"), []byte("m2")})
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()
	if err := c.EnqueueBatch(ctx, "", [][]byte{[]byte("m1"), []byte("m2")}); err != nil {
		t.Fatalf("enqueue batch: %v", err)
	}
	msgs, err := c.DequeueBatch(ctx, "", 10, 0, time.Minute)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("dequeue batch: %v (%d)", err, len(msgs))
	}
	if string(msgs[0].Data) != "m1" || msgs[0].LeaseID != "id1" || msgs[1].LeaseID != "id2" {
		t.Fatalf("batch content: %+v", msgs)
	}
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := New(srv.URL).Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestBackoffHonorsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	c := New(srv.URL, WithRetries(50))
	start := time.Now()
	err := c.Enqueue(ctx, "", bytes.Repeat([]byte{'x'}, 8))
	if err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("retries ignored context deadline")
	}
}
