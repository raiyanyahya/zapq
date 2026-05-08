package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const smallBody = "hi"

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"100", 100},
		{"1M", 1 << 20},
		{"2M", 2 << 20},
		{"1G", 1 << 30},
		{"3G", 3 << 30},
		{" 4M ", 4 << 20},
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
	if _, err := parseSize("abc"); err == nil {
		t.Error("expected error for invalid input")
	}
}

func TestEnqueueDequeue(t *testing.T) {
	q := newQueue(256<<20, 50000)
	c := &counters{}
	if err := q.enqueue([]byte(smallBody), c); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if l := q.length(); l != 1 {
		t.Fatalf("want len=1 got %d", l)
	}
	if c.enqueues.Load() != 1 {
		t.Fatalf("enqueues counter want 1 got %d", c.enqueues.Load())
	}
	b, ok := q.dequeue(c)
	if !ok || string(b) != smallBody {
		t.Fatalf("bad dequeue got=%q ok=%v", b, ok)
	}
	if c.dequeues.Load() != 1 {
		t.Fatalf("dequeues counter want 1 got %d", c.dequeues.Load())
	}
	if _, ok := q.dequeue(c); ok {
		t.Fatal("expected empty after single dequeue")
	}
	if c.dequeuesEmpty.Load() != 1 {
		t.Fatalf("dequeuesEmpty counter want 1 got %d", c.dequeuesEmpty.Load())
	}
}

func TestQueueFull(t *testing.T) {
	q := newQueue(256<<20, 2)
	c := &counters{}

	_ = q.enqueue([]byte("A"), c)
	_ = q.enqueue([]byte("B"), c)

	if err := q.enqueue([]byte("C"), c); err == nil {
		t.Fatal("expected queue full error")
	}
}

func TestPayloadTooLarge(t *testing.T) {
	q := newQueue(256<<20, 50000)
	c := &counters{}
	huge := bytes.Repeat([]byte{'x'}, maxBody+1)
	if err := q.enqueue(huge, c); err == nil {
		t.Fatal("want payload too large error")
	}
}

func TestQueueClear(t *testing.T) {
	q := newQueue(256<<20, 50000)
	c := &counters{}
	_ = q.enqueue([]byte("one"), c)
	_ = q.enqueue([]byte("two"), c)
	q.clear()
	if q.length() != 0 {
		t.Fatalf("expected empty queue after clear, got len=%d", q.length())
	}
	if q.size() != 0 {
		t.Fatalf("expected zero bytes after clear, got %d", q.size())
	}
}

func TestPersistAndLoad(t *testing.T) {
	q := newQueue(256<<20, 50000)
	c := &counters{}
	_ = q.enqueue([]byte("one"), c)
	_ = q.enqueue([]byte("two"), c)

	tmp, err := os.CreateTemp("", "zapq*.json")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	if err := q.persist(tmp.Name()); err != nil {
		t.Fatalf("persist: %v", err)
	}

	q2 := newQueue(256<<20, 50000)
	if err := q2.load(tmp.Name()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if q2.length() != 2 || q2.size() != q.size() {
		t.Fatalf("load mismatch len=%d bytes=%d", q2.length(), q2.size())
	}
}

func TestConcurrentSafety(t *testing.T) {
	q := newQueue(256<<20, 50000)
	c := &counters{}
	const total = 1000
	var wg sync.WaitGroup
	wg.Add(200)

	for i := 0; i < 100; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < total/100; j++ {
				if err := q.enqueue([]byte("x"), c); err != nil {
					t.Errorf("enqueue err: %v", err)
				}
			}
		}()
	}

	for i := 0; i < 100; i++ {
		go func() {
			defer wg.Done()
			for {
				_, ok := q.dequeue(c)
				if !ok {
					if c.dequeues.Load() >= total {
						return
					}
					time.Sleep(time.Microsecond)
				}
			}
		}()
	}

	wg.Wait()
	if c.enqueues.Load() != total || c.dequeues.Load() != total || q.length() != 0 {
		t.Fatalf("counters mismatch enq=%d deq=%d len=%d",
			c.enqueues.Load(), c.dequeues.Load(), q.length())
	}
}

// --- HTTP-level tests ---

func newTestServer(dataDir string) *server {
	return &server{
		q:         newQueue(256<<20, 50000),
		c:         &counters{},
		startTime: time.Now(),
		dataDir:   dataDir,
	}
}

func TestHTTPEnqueueDequeue(t *testing.T) {
	s := newTestServer(".")
	mux := http.NewServeMux()
	mux.HandleFunc("/enqueue", s.handleEnqueue)
	mux.HandleFunc("/dequeue", s.handleDequeue)
	ts := httptest.NewServer(s.traceMiddleware(mux))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/enqueue", "application/octet-stream",
		strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue status: %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/dequeue")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dequeue status: %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/dequeue")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty dequeue status: %d", resp.StatusCode)
	}
}

func TestHTTPMethodNotAllowed(t *testing.T) {
	s := newTestServer(".")
	mux := http.NewServeMux()
	mux.HandleFunc("/enqueue", s.handleEnqueue)
	mux.HandleFunc("/dequeue", s.handleDequeue)
	mux.HandleFunc("/clear", s.handleClear)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/version", s.handleVersion)
	ts := httptest.NewServer(s.traceMiddleware(mux))
	defer ts.Close()

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/enqueue"},
		{http.MethodPost, "/dequeue"},
		{http.MethodGet, "/clear"},
		{http.MethodPost, "/metrics"},
		{http.MethodPost, "/health"},
		{http.MethodPost, "/version"},
	}
	for _, tt := range tests {
		req, _ := http.NewRequest(tt.method, ts.URL+tt.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: want %d got %d", tt.method, tt.path,
				http.StatusMethodNotAllowed, resp.StatusCode)
		}
	}
}

func TestHTTPMetrics(t *testing.T) {
	s := newTestServer(".")
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	ts := httptest.NewServer(s.traceMiddleware(mux))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var m map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	for _, field := range []string{"uptime_sec", "enqueues", "dequeues_ok",
		"dequeues_empty", "queue_length", "queue_bytes", "goroutines", "version"} {
		if _, ok := m[field]; !ok {
			t.Errorf("metrics missing field %s", field)
		}
	}
	if v, ok := m["version"]; !ok || v == "" {
		t.Errorf("version field empty or missing")
	}
}

func TestHTTPHealth(t *testing.T) {
	s := newTestServer(".")
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	ts := httptest.NewServer(s.traceMiddleware(mux))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status: %d", resp.StatusCode)
	}
}

func TestHTTPClear(t *testing.T) {
	s := newTestServer(".")
	mux := http.NewServeMux()
	mux.HandleFunc("/enqueue", s.handleEnqueue)
	mux.HandleFunc("/clear", s.handleClear)
	ts := httptest.NewServer(s.traceMiddleware(mux))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/enqueue", "application/octet-stream",
		strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp, err = http.Post(ts.URL+"/clear", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear status: %d", resp.StatusCode)
	}
	if s.q.length() != 0 {
		t.Fatal("queue not cleared")
	}
}

func TestHTTPPersistLoad(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(dir)
	mux := http.NewServeMux()
	mux.HandleFunc("/enqueue", s.handleEnqueue)
	mux.HandleFunc("/persist", s.handlePersist)
	mux.HandleFunc("/load", s.handleLoad)
	ts := httptest.NewServer(s.traceMiddleware(mux))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/enqueue", "application/octet-stream",
		strings.NewReader("persist-me"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp, err = http.Post(ts.URL+"/persist?file=snap.json", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("persist status: %d", resp.StatusCode)
	}

	s2 := newTestServer(dir)
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/load", s2.handleLoad)
	mux2.HandleFunc("/dequeue", s2.handleDequeue)
	ts2 := httptest.NewServer(s2.traceMiddleware(mux2))
	defer ts2.Close()

	resp, err = http.Post(ts2.URL+"/load?file=snap.json", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("load status: %d", resp.StatusCode)
	}

	resp, err = http.Get(ts2.URL + "/dequeue")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("could not dequeue persisted message")
	}
}

func TestHTTPPersistPathEscape(t *testing.T) {
	s := newTestServer(t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/persist", s.handlePersist)
	ts := httptest.NewServer(s.traceMiddleware(mux))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/persist?file=../../../etc/passwd",
		"application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("path escape not blocked, got status %d", resp.StatusCode)
	}
}

func TestHTTPPayloadTooLarge(t *testing.T) {
	s := newTestServer(".")
	mux := http.NewServeMux()
	mux.HandleFunc("/enqueue", s.handleEnqueue)
	ts := httptest.NewServer(s.traceMiddleware(mux))
	defer ts.Close()

	big := bytes.Repeat([]byte{'x'}, maxBody+1)
	resp, err := http.Post(ts.URL+"/enqueue", "application/octet-stream",
		bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 got %d", resp.StatusCode)
	}
}

func TestHTTPVersion(t *testing.T) {
	s := newTestServer(".")
	mux := http.NewServeMux()
	mux.HandleFunc("/version", s.handleVersion)
	ts := httptest.NewServer(s.traceMiddleware(mux))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("version status: %d", resp.StatusCode)
	}
}

func TestCounters(t *testing.T) {
	c := &counters{}
	if c.enqueues.Load() != 0 {
		t.Fatal("initial enqueues not zero")
	}
	c.enqueues.Add(5)
	if c.enqueues.Load() != 5 {
		t.Fatalf("enqueues after add: %d", c.enqueues.Load())
	}
	c.enqueues.Add(3)
	if c.enqueues.Load() != 8 {
		t.Fatalf("enqueues after second add: %d", c.enqueues.Load())
	}
}

func TestTraceID(t *testing.T) {
	c := &counters{}
	id1 := c.newTraceID()
	id2 := c.newTraceID()
	if id1 == id2 {
		t.Fatal("trace IDs should be unique")
	}
	if !strings.Contains(id1, "-") {
		t.Fatal("trace ID missing separator")
	}
}

func TestSanitizePath(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub")
	os.Mkdir(nested, 0o755)

	s := &server{dataDir: dir}

	tests := []struct {
		name    string
		wantErr bool
	}{
		{"test.json", false},
		{"sub/data.json", false},
		{"../../../etc/passwd", true},
		{"/abs/path/file.json", false},
	}
	for _, tt := range tests {
		path, err := s.sanitizePath(tt.name)
		if tt.wantErr && err == nil {
			t.Errorf("sanitizePath(%q): expected error, got %q", tt.name, path)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("sanitizePath(%q): unexpected error: %v", tt.name, err)
		}
	}
}

func TestMaxBytesFull(t *testing.T) {
	q := newQueue(10, 50000)
	c := &counters{}
	_ = q.enqueue([]byte("1234567890"), c)
	if err := q.enqueue([]byte("x"), c); err == nil {
		t.Fatal("expected queue full by bytes")
	}
}

func TestEmptyStatsSnapshot(t *testing.T) {
	q := newQueue(256<<20, 50000)
	if q.length() != 0 || q.size() != 0 {
		t.Fatal("new queue not empty")
	}
}

func TestServersAreIndependent(t *testing.T) {
	q1 := newQueue(256<<20, 50000)
	c1 := &counters{}
	q2 := newQueue(256<<20, 50000)
	c2 := &counters{}

	_ = q1.enqueue([]byte("a"), c1)
	_ = q2.enqueue([]byte("b"), c2)

	if q1.length() != 1 || q2.length() != 1 {
		t.Fatal("queues not independent")
	}
	if c1.enqueues.Load() != 1 || c2.enqueues.Load() != 1 {
		t.Fatal("counters not independent")
	}
}
