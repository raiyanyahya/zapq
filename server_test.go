package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raiyanyahya/zapq/internal/frames"
)

func newTestServer(dataDir string) *server {
	return &server{
		reg:        newRegistry(256<<20, testMaxMsgBytes, 50000, 8),
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		startTime:  time.Now(),
		dataDir:    dataDir,
		maxWait:    2 * time.Second,
		maxLease:   time.Minute,
		maxWaiters: 1024,
	}
}

func startTestServer(t *testing.T, s *server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(s.routes(false))
	t.Cleanup(ts.Close)
	return ts
}

func defaultQ(t *testing.T, s *server) *queue {
	t.Helper()
	q, err := s.reg.get("")
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func doReq(t *testing.T, method, url, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHTTPEnqueueDequeue(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodPost, ts.URL+"/enqueue", "", strings.NewReader("hello"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue status: %d", resp.StatusCode)
	}

	resp = doReq(t, http.MethodGet, ts.URL+"/dequeue", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("dequeue status=%d body=%q", resp.StatusCode, body)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "5" {
		t.Fatalf("Content-Length = %q, want 5", cl)
	}
	if resp.Header.Get("X-Trace-Id") == "" {
		t.Fatal("missing X-Trace-Id header")
	}
	if resp.Header.Get("X-Lease-Id") != "" {
		t.Fatal("lease header present without lease param")
	}

	resp = doReq(t, http.MethodGet, ts.URL+"/dequeue", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty dequeue status: %d", resp.StatusCode)
	}
	q := defaultQ(t, s)
	if q.c.enqueues.Load() != 1 || q.c.dequeues.Load() != 1 || q.c.dequeuesEmpty.Load() != 1 {
		t.Fatalf("counters wrong: enq=%d deq=%d empty=%d",
			q.c.enqueues.Load(), q.c.dequeues.Load(), q.c.dequeuesEmpty.Load())
	}
}

func TestHTTPNamedQueues(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodPost, ts.URL+"/q/orders/enqueue", "", strings.NewReader("order-1"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("named enqueue: %d", resp.StatusCode)
	}

	// other queues don't see it
	resp = doReq(t, http.MethodGet, ts.URL+"/dequeue", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("default queue leaked: %d", resp.StatusCode)
	}

	resp = doReq(t, http.MethodGet, ts.URL+"/q/orders/dequeue", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "order-1" {
		t.Fatalf("named dequeue: status=%d body=%q", resp.StatusCode, body)
	}

	// invalid name is a 400
	resp = doReq(t, http.MethodPost, ts.URL+"/q/bad%20name/enqueue", "", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad queue name: %d", resp.StatusCode)
	}
}

func TestHTTPQueueLimit(t *testing.T) {
	s := newTestServer(t.TempDir())
	s.reg = newRegistry(256<<20, testMaxMsgBytes, 16, 2)
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodPost, ts.URL+"/q/one/enqueue", "", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first named queue: %d", resp.StatusCode)
	}
	resp = doReq(t, http.MethodPost, ts.URL+"/q/two/enqueue", "", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over queue limit: want 429 got %d", resp.StatusCode)
	}
}

func TestHTTPLeaseAndAck(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodPost, ts.URL+"/enqueue", "", strings.NewReader("work"))
	resp.Body.Close()

	resp = doReq(t, http.MethodGet, ts.URL+"/dequeue?lease=30s", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "work" {
		t.Fatalf("leased dequeue: %d %q", resp.StatusCode, body)
	}
	id := resp.Header.Get("X-Lease-Id")
	if id == "" || resp.Header.Get("X-Lease-Expires") == "" {
		t.Fatal("lease headers missing")
	}

	resp = doReq(t, http.MethodPost, ts.URL+"/ack?id="+id, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack: %d", resp.StatusCode)
	}

	// double ack fails
	resp = doReq(t, http.MethodPost, ts.URL+"/ack?id="+id, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("double ack: want 404 got %d", resp.StatusCode)
	}

	// missing id is a 400
	resp = doReq(t, http.MethodPost, ts.URL+"/ack", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ack without id: %d", resp.StatusCode)
	}
}

func TestHTTPBatch(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	var buf bytes.Buffer
	in := [][]byte{[]byte("b1"), []byte("b2"), []byte("b3")}
	if err := frames.Write(&buf, in); err != nil {
		t.Fatal(err)
	}
	resp := doReq(t, http.MethodPost, ts.URL+"/enqueue/batch", "", &buf)
	var enq map[string]int
	json.NewDecoder(resp.Body).Decode(&enq)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || enq["enqueued"] != 3 {
		t.Fatalf("batch enqueue: status=%d body=%v", resp.StatusCode, enq)
	}

	resp = doReq(t, http.MethodGet, ts.URL+"/dequeue/batch?max=2&lease=30s", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch dequeue: %d", resp.StatusCode)
	}
	out, err := frames.Read(resp.Body, testMaxMsgBytes)
	resp.Body.Close()
	if err != nil || len(out) != 2 {
		t.Fatalf("batch decode: %v (%d msgs)", err, len(out))
	}
	if string(out[0]) != "b1" || string(out[1]) != "b2" {
		t.Fatalf("batch order: %q %q", out[0], out[1])
	}
	ids := strings.Split(resp.Header.Get("X-Lease-Ids"), ",")
	if len(ids) != 2 {
		t.Fatalf("lease ids: %v", ids)
	}
	for _, id := range ids {
		r2 := doReq(t, http.MethodPost, ts.URL+"/ack?id="+id, "", nil)
		r2.Body.Close()
		if r2.StatusCode != http.StatusOK {
			t.Fatalf("batch ack %s: %d", id, r2.StatusCode)
		}
	}

	// remaining message, then empty
	resp = doReq(t, http.MethodGet, ts.URL+"/dequeue/batch", "", nil)
	out, _ = frames.Read(resp.Body, testMaxMsgBytes)
	resp.Body.Close()
	if len(out) != 1 || string(out[0]) != "b3" {
		t.Fatalf("remaining batch: %q", out)
	}
	resp = doReq(t, http.MethodGet, ts.URL+"/dequeue/batch", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty batch: %d", resp.StatusCode)
	}

	// malformed frames are a 400
	resp = doReq(t, http.MethodPost, ts.URL+"/enqueue/batch", "", bytes.NewReader([]byte{0, 0}))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed batch: %d", resp.StatusCode)
	}
	// bad max is a 400
	resp = doReq(t, http.MethodGet, ts.URL+"/dequeue/batch?max=0", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad max: %d", resp.StatusCode)
	}
}

func TestHTTPScopedTokens(t *testing.T) {
	s := newTestServer(t.TempDir())
	s.tokens = tokens{producer: "prod", consumer: "cons", admin: "adm"}
	ts := startTestServer(t, s)

	cases := []struct {
		method, path, token string
		body                string
		want                int
	}{
		// producer can enqueue, not dequeue or clear
		{http.MethodPost, "/enqueue", "prod", "x", http.StatusAccepted},
		{http.MethodGet, "/dequeue", "prod", "", http.StatusUnauthorized},
		{http.MethodPost, "/clear", "prod", "", http.StatusUnauthorized},
		// consumer can dequeue and ack, not enqueue
		{http.MethodGet, "/dequeue", "cons", "", http.StatusOK},
		{http.MethodPost, "/enqueue", "cons", "x", http.StatusUnauthorized},
		// both can read metrics
		{http.MethodGet, "/metrics", "prod", "", http.StatusOK},
		{http.MethodGet, "/metrics", "cons", "", http.StatusOK},
		{http.MethodGet, "/queues", "cons", "", http.StatusOK},
		// admin can do everything
		{http.MethodPost, "/enqueue", "adm", "x", http.StatusAccepted},
		{http.MethodPost, "/clear", "adm", "", http.StatusOK},
		// no token, wrong token
		{http.MethodPost, "/enqueue", "", "x", http.StatusUnauthorized},
		{http.MethodPost, "/enqueue", "wrong", "x", http.StatusUnauthorized},
		// probes stay open
		{http.MethodGet, "/health", "", "", http.StatusOK},
		{http.MethodGet, "/version", "", "", http.StatusOK},
	}
	for _, tt := range cases {
		var body io.Reader
		if tt.body != "" {
			body = strings.NewReader(tt.body)
		}
		resp := doReq(t, tt.method, ts.URL+tt.path, tt.token, body)
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s %s token=%q: want %d got %d", tt.method, tt.path, tt.token, tt.want, resp.StatusCode)
		}
	}
}

func TestHTTPApiTokenGrantsAll(t *testing.T) {
	s := newTestServer(t.TempDir())
	s.tokens = tokens{api: "s3cret"}
	ts := startTestServer(t, s)

	for _, tt := range []struct {
		method, path string
		body         string
		want         int
	}{
		{http.MethodPost, "/enqueue", "x", http.StatusAccepted},
		{http.MethodGet, "/dequeue", "", http.StatusOK},
		{http.MethodPost, "/clear", "", http.StatusOK},
		{http.MethodGet, "/metrics", "", http.StatusOK},
	} {
		var body io.Reader
		if tt.body != "" {
			body = strings.NewReader(tt.body)
		}
		resp := doReq(t, tt.method, ts.URL+tt.path, "s3cret", body)
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s %s: want %d got %d", tt.method, tt.path, tt.want, resp.StatusCode)
		}
	}
}

func TestHTTPWaiterCap(t *testing.T) {
	s := newTestServer(t.TempDir())
	s.maxWaiters = 1
	ts := startTestServer(t, s)

	started := make(chan struct{})
	go func() {
		close(started)
		resp, err := http.Get(ts.URL + "/dequeue?wait=2s")
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	time.Sleep(100 * time.Millisecond) // let the first waiter park

	resp := doReq(t, http.MethodGet, ts.URL+"/dequeue?wait=2s", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second waiter: want 503 got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("503 missing Retry-After")
	}

	// non-waiting dequeue is not throttled
	resp = doReq(t, http.MethodGet, ts.URL+"/dequeue", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("plain dequeue throttled: %d", resp.StatusCode)
	}
}

func TestHTTPDequeueWait(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	done := make(chan string, 1)
	go func() {
		resp, err := http.Get(ts.URL + "/dequeue?wait=2s")
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		done <- string(b)
	}()

	time.Sleep(100 * time.Millisecond)
	resp := doReq(t, http.MethodPost, ts.URL+"/enqueue", "", strings.NewReader("delayed"))
	resp.Body.Close()

	select {
	case got := <-done:
		if got != "delayed" {
			t.Fatalf("long poll got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll never returned")
	}
}

func TestHTTPDequeueBadParams(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	for _, url := range []string{"/dequeue?wait=banana", "/dequeue?wait=-5s", "/dequeue?lease=nope"} {
		resp := doReq(t, http.MethodGet, ts.URL+url, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400 got %d", url, resp.StatusCode)
		}
	}
}

func TestHTTPMethodNotAllowed(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/enqueue"},
		{http.MethodPost, "/dequeue"},
		{http.MethodGet, "/clear"},
		{http.MethodPost, "/metrics"},
		{http.MethodGet, "/ack"},
	}
	for _, tt := range tests {
		resp := doReq(t, tt.method, ts.URL+tt.path, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: want 405 got %d", tt.method, tt.path, resp.StatusCode)
		}
	}
}

func TestHTTPMetricsJSON(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodPost, ts.URL+"/q/jobs/enqueue", "", strings.NewReader("x"))
	resp.Body.Close()

	resp = doReq(t, http.MethodGet, ts.URL+"/metrics", "", nil)
	defer resp.Body.Close()
	var m map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	for _, field := range []string{"uptime_sec", "enqueues", "dequeues_ok", "dequeues_empty",
		"acks", "lease_expirations", "queue_length", "queue_bytes", "leased",
		"waiting_consumers", "goroutines", "version", "queues"} {
		if _, ok := m[field]; !ok {
			t.Errorf("metrics missing field %s", field)
		}
	}
	queues, ok := m["queues"].(map[string]interface{})
	if !ok || queues["jobs"] == nil || queues["default"] == nil {
		t.Fatalf("per-queue metrics missing: %v", m["queues"])
	}
}

func TestHTTPMetricsPrometheus(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=0.5,*/*;q=0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		"# TYPE zapq_enqueues_total counter",
		"zapq_queue_length ",
		"zapq_head_message_age_seconds{queue=\"default\"}",
		"zapq_waiting_consumers ",
		"zapq_build_info{version=",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("prometheus output missing %q", want)
		}
	}

	resp2, err := http.Get(ts.URL + "/metrics?format=prometheus")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body2), "zapq_uptime_seconds") {
		t.Errorf("format=prometheus not honored")
	}
}

func TestHTTPQueuesEndpoint(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodPost, ts.URL+"/q/a/enqueue", "", strings.NewReader("x"))
	resp.Body.Close()

	resp = doReq(t, http.MethodGet, ts.URL+"/queues", "", nil)
	defer resp.Body.Close()
	var out struct {
		Queues []struct {
			Name   string `json:"name"`
			Length int    `json:"length"`
		} `json:"queues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode queues: %v", err)
	}
	if len(out.Queues) != 2 || out.Queues[0].Name != "a" || out.Queues[0].Length != 1 {
		t.Fatalf("queues listing wrong: %+v", out.Queues)
	}
}

func TestHTTPClear(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodPost, ts.URL+"/enqueue", "", strings.NewReader("x"))
	resp.Body.Close()
	resp = doReq(t, http.MethodPost, ts.URL+"/clear", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear status: %d", resp.StatusCode)
	}
	if defaultQ(t, s).length() != 0 {
		t.Fatal("queue not cleared")
	}
}

func TestHTTPPersistLoad(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(dir)
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodPost, ts.URL+"/q/jobs/enqueue", "", strings.NewReader("persist-me"))
	resp.Body.Close()
	resp = doReq(t, http.MethodPost, ts.URL+"/persist?file=snap.bin", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("persist status: %d", resp.StatusCode)
	}

	s2 := newTestServer(dir)
	ts2 := startTestServer(t, s2)
	resp = doReq(t, http.MethodPost, ts2.URL+"/load?file=snap.bin", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("load status: %d", resp.StatusCode)
	}
	resp = doReq(t, http.MethodGet, ts2.URL+"/q/jobs/dequeue", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "persist-me" {
		t.Fatalf("persisted message lost: status=%d body=%q", resp.StatusCode, body)
	}
}

func TestHTTPLoadMissingFile(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)
	resp := doReq(t, http.MethodPost, ts.URL+"/load?file=missing.bin", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing snapshot: want 404 got %d", resp.StatusCode)
	}
}

func TestHTTPPersistPathEscape(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)
	resp := doReq(t, http.MethodPost, ts.URL+"/persist?file=../../../etc/passwd", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("path escape not blocked, got status %d", resp.StatusCode)
	}
}

func TestHTTPPayloadTooLarge(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)
	big := bytes.Repeat([]byte{'x'}, testMaxMsgBytes+1)
	resp := doReq(t, http.MethodPost, ts.URL+"/enqueue", "", bytes.NewReader(big))
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 got %d", resp.StatusCode)
	}
}

func TestHTTPQueueFullRetryAfter(t *testing.T) {
	s := newTestServer(t.TempDir())
	s.reg = newRegistry(256<<20, testMaxMsgBytes, 1, 8)
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodPost, ts.URL+"/enqueue", "", strings.NewReader("x"))
	resp.Body.Close()
	resp = doReq(t, http.MethodPost, ts.URL+"/enqueue", "", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429 got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 missing Retry-After header")
	}
}

func TestHTTPVersionAndHealth(t *testing.T) {
	s := newTestServer(t.TempDir())
	ts := startTestServer(t, s)

	resp := doReq(t, http.MethodGet, ts.URL+"/version", "", nil)
	var v map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	resp.Body.Close()
	if v["version"] == "" {
		t.Fatal("version empty")
	}

	resp = doReq(t, http.MethodGet, ts.URL+"/health", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("health: status=%d body=%q", resp.StatusCode, body)
	}
}

func TestTraceID(t *testing.T) {
	s := newTestServer(t.TempDir())
	id1 := s.newTraceID()
	id2 := s.newTraceID()
	if id1 == id2 {
		t.Fatal("trace IDs should be unique")
	}
	if !strings.Contains(id1, "-") {
		t.Fatal("trace ID missing separator")
	}
}

func TestSanitizePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &server{dataDir: dir}

	tests := []struct {
		name    string
		wantErr bool
	}{
		{"test.json", false},
		{"sub/data.json", false},
		{"../../../etc/passwd", true},
		{"..", true},
		{"/abs/path/file.json", false}, // joined under dataDir, stays inside
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
