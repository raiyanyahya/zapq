package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	netpprof "net/http/pprof"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/raiyanyahya/zapq/internal/frames"
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("rand.Read: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// --------------------------- auth --------------------------------------------

// tokens holds the configured bearer tokens. api and admin grant everything;
// producer is enqueue-only, consumer is dequeue/ack-only. Producer and
// consumer tokens can also read metrics.
type tokens struct {
	api      string
	producer string
	consumer string
	admin    string
}

func (t tokens) enabled() bool {
	return t.api != "" || t.producer != "" || t.consumer != "" || t.admin != ""
}

type accessClass int

const (
	clsOpen accessClass = iota
	clsProduce
	clsConsume
	clsMetrics
	clsAdmin
)

func tokenEq(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (t tokens) allows(cls accessClass, tok string) bool {
	if cls == clsOpen {
		return true
	}
	if tokenEq(tok, t.api) || tokenEq(tok, t.admin) {
		return true
	}
	switch cls {
	case clsProduce:
		return tokenEq(tok, t.producer)
	case clsConsume:
		return tokenEq(tok, t.consumer)
	case clsMetrics:
		return tokenEq(tok, t.producer) || tokenEq(tok, t.consumer)
	}
	return false
}

// --------------------------- server ------------------------------------------

type traceKey struct{}

func traceFrom(ctx context.Context) string {
	if v := ctx.Value(traceKey{}); v != nil {
		return v.(string)
	}
	return ""
}

type server struct {
	reg        *registry
	tokens     tokens
	log        *slog.Logger
	startTime  time.Time
	dataDir    string
	maxWait    time.Duration
	maxLease   time.Duration
	maxWaiters int64
	waiters    atomic.Int64
	tcpConns   atomic.Int64
	trace      atomic.Uint64
}

func (s *server) newTraceID() string {
	return strconv.FormatUint(s.trace.Add(1), 10) + "-" + randHex(3)
}

// logger returns the server logger annotated with the request trace ID.
func (s *server) logger(ctx context.Context) *slog.Logger {
	if t := traceFrom(ctx); t != "" {
		return s.log.With("trace_id", t)
	}
	return s.log
}

// routes builds the full handler chain: trace -> request log -> mux, with
// per-route token enforcement by access class.
func (s *server) routes(pprofEnabled bool) http.Handler {
	mux := http.NewServeMux()
	// default queue (backward-compatible paths)
	mux.HandleFunc("POST /enqueue", s.protect(clsProduce, s.handleEnqueue))
	mux.HandleFunc("POST /enqueue/batch", s.protect(clsProduce, s.handleEnqueueBatch))
	mux.HandleFunc("GET /dequeue", s.protect(clsConsume, s.handleDequeue))
	mux.HandleFunc("GET /dequeue/batch", s.protect(clsConsume, s.handleDequeueBatch))
	mux.HandleFunc("POST /ack", s.protect(clsConsume, s.handleAck))
	mux.HandleFunc("POST /clear", s.protect(clsAdmin, s.handleClear))
	// named queues
	mux.HandleFunc("POST /q/{queue}/enqueue", s.protect(clsProduce, s.handleEnqueue))
	mux.HandleFunc("POST /q/{queue}/enqueue/batch", s.protect(clsProduce, s.handleEnqueueBatch))
	mux.HandleFunc("GET /q/{queue}/dequeue", s.protect(clsConsume, s.handleDequeue))
	mux.HandleFunc("GET /q/{queue}/dequeue/batch", s.protect(clsConsume, s.handleDequeueBatch))
	mux.HandleFunc("POST /q/{queue}/ack", s.protect(clsConsume, s.handleAck))
	mux.HandleFunc("POST /q/{queue}/clear", s.protect(clsAdmin, s.handleClear))
	// operations
	mux.HandleFunc("GET /queues", s.protect(clsMetrics, s.handleQueues))
	mux.HandleFunc("GET /metrics", s.protect(clsMetrics, s.handleMetrics))
	mux.HandleFunc("POST /persist", s.protect(clsAdmin, s.handlePersist))
	mux.HandleFunc("POST /load", s.protect(clsAdmin, s.handleLoad))
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /version", s.handleVersion)
	if pprofEnabled {
		mux.HandleFunc("GET /debug/pprof/", s.protect(clsAdmin, netpprof.Index))
		mux.HandleFunc("GET /debug/pprof/cmdline", s.protect(clsAdmin, netpprof.Cmdline))
		mux.HandleFunc("GET /debug/pprof/profile", s.protect(clsAdmin, netpprof.Profile))
		mux.HandleFunc("GET /debug/pprof/symbol", s.protect(clsAdmin, netpprof.Symbol))
		mux.HandleFunc("GET /debug/pprof/trace", s.protect(clsAdmin, netpprof.Trace))
	}

	var h http.Handler = mux
	h = s.logMiddleware(h)
	h = s.traceMiddleware(h)
	return h
}

// --------------------------- middleware ---------------------------------------

func (s *server) traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := r.Header.Get("X-Trace-Id")
		if tid == "" {
			tid = s.newTraceID()
		}
		ctx := context.WithValue(r.Context(), traceKey{}, tid)
		w.Header().Set("X-Trace-Id", tid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (s *server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.logger(r.Context()).Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", float64(time.Since(start).Microseconds())/1000,
		)
	})
}

// protect enforces the route's access class when any token is configured.
func (s *server) protect(cls accessClass, h http.HandlerFunc) http.HandlerFunc {
	if !s.tokens.enabled() || cls == clsOpen {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		var tok string
		if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
			tok = auth[len(prefix):]
		}
		if !s.tokens.allows(cls, tok) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="zapq"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			s.logger(r.Context()).Warn("unauthorized request", "path", r.URL.Path)
			return
		}
		h(w, r)
	}
}

func (s *server) sanitizePath(name string) (string, error) {
	abs, err := filepath.Abs(filepath.Join(s.dataDir, name))
	if err != nil {
		return "", err
	}
	absDir, err := filepath.Abs(s.dataDir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes data directory")
	}
	return abs, nil
}

// --------------------------- request helpers ----------------------------------

// queueFor resolves the {queue} path segment (default queue when absent),
// writing the error response itself when the name is bad or the limit hit.
func (s *server) queueFor(w http.ResponseWriter, r *http.Request) *queue {
	q, err := s.reg.get(r.PathValue("queue"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTooManyQueues) {
			status = http.StatusTooManyRequests
		}
		http.Error(w, err.Error(), status)
		return nil
	}
	return q
}

// durationParam parses an optional duration query parameter, clamping to
// max. The bool is false when the response has already been written.
func (s *server) durationParam(w http.ResponseWriter, r *http.Request, name string, max time.Duration) (time.Duration, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, true
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		http.Error(w, "invalid "+name+" duration (use e.g. "+name+"=5s)", http.StatusBadRequest)
		return 0, false
	}
	if d > max {
		d = max
	}
	return d, true
}

// reserveWaiter enforces the long-poll concurrency cap. The returned release
// func is non-nil when a slot was taken; ok=false means 503 was written.
func (s *server) reserveWaiter(w http.ResponseWriter) (release func(), ok bool) {
	if s.waiters.Add(1) > s.maxWaiters {
		s.waiters.Add(-1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many waiting consumers", http.StatusServiceUnavailable)
		return nil, false
	}
	return func() { s.waiters.Add(-1) }, true
}

func setLeaseHeaders(w http.ResponseWriter, g *leaseGrant) {
	if g != nil {
		w.Header().Set("X-Lease-Id", g.id)
		w.Header().Set("X-Lease-Expires", g.expires.UTC().Format(time.RFC3339Nano))
	}
}

// --------------------------- HTTP handlers ------------------------------------

func (s *server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	q := s.queueFor(w, r)
	if q == nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, q.maxMsgBytes+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		s.logger(r.Context()).Error("read body failed", "error", err.Error())
		return
	}
	if int64(len(body)) > q.maxMsgBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		s.logger(r.Context()).Warn("payload too large", "bytes", len(body))
		return
	}
	if err := q.enqueue(body); err != nil {
		s.writeEnqueueError(w, r, err)
		return
	}
	q.c.enqueues.Add(1)
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) handleEnqueueBatch(w http.ResponseWriter, r *http.Request) {
	q := s.queueFor(w, r)
	if q == nil {
		return
	}
	msgs, err := frames.Read(r.Body, q.maxMsgBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, frames.ErrMessageTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	if err := q.enqueueBatch(msgs); err != nil {
		s.writeEnqueueError(w, r, err)
		return
	}
	q.c.enqueues.Add(uint64(len(msgs)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]int{"enqueued": len(msgs)})
}

func (s *server) writeEnqueueError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errTooLarge) {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	} else {
		w.Header().Set("Retry-After", "1")
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	}
	s.logger(r.Context()).Warn("enqueue rejected", "reason", err.Error())
}

func (s *server) handleDequeue(w http.ResponseWriter, r *http.Request) {
	q := s.queueFor(w, r)
	if q == nil {
		return
	}
	wait, ok := s.durationParam(w, r, "wait", s.maxWait)
	if !ok {
		return
	}
	leaseDur, ok := s.durationParam(w, r, "lease", s.maxLease)
	if !ok {
		return
	}
	if wait > 0 {
		release, ok := s.reserveWaiter(w)
		if !ok {
			return
		}
		defer release()
	}
	m, g, ok := q.takeWait(r.Context(), wait, leaseDur)
	if !ok {
		q.c.dequeuesEmpty.Add(1)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	q.c.dequeues.Add(1)
	setLeaseHeaders(w, g)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(m.data)))
	w.Write(m.data)
}

func (s *server) handleDequeueBatch(w http.ResponseWriter, r *http.Request) {
	q := s.queueFor(w, r)
	if q == nil {
		return
	}
	max := 100
	if v := r.URL.Query().Get("max"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > frames.MaxMessages {
			http.Error(w, fmt.Sprintf("max must be 1..%d", frames.MaxMessages), http.StatusBadRequest)
			return
		}
		max = n
	}
	wait, ok := s.durationParam(w, r, "wait", s.maxWait)
	if !ok {
		return
	}
	leaseDur, ok := s.durationParam(w, r, "lease", s.maxLease)
	if !ok {
		return
	}
	if wait > 0 {
		release, ok := s.reserveWaiter(w)
		if !ok {
			return
		}
		defer release()
	}
	msgs, ids := q.takeBatchWait(r.Context(), wait, max, leaseDur)
	if len(msgs) == 0 {
		q.c.dequeuesEmpty.Add(1)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	q.c.dequeues.Add(uint64(len(msgs)))
	if len(ids) > 0 {
		w.Header().Set("X-Lease-Ids", strings.Join(ids, ","))
	}
	w.Header().Set("Content-Type", frames.ContentType)
	payloads := make([][]byte, len(msgs))
	for i, m := range msgs {
		payloads[i] = m.data
	}
	if err := frames.Write(w, payloads); err != nil {
		s.logger(r.Context()).Error("write batch failed", "error", err.Error())
	}
}

func (s *server) handleAck(w http.ResponseWriter, r *http.Request) {
	q := s.queueFor(w, r)
	if q == nil {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id parameter", http.StatusBadRequest)
		return
	}
	if !q.ack(id) {
		http.Error(w, "unknown lease: already acked, expired, or never issued", http.StatusNotFound)
		return
	}
	q.c.acks.Add(1)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"acked": true})
}

func (s *server) handleClear(w http.ResponseWriter, r *http.Request) {
	q := s.queueFor(w, r)
	if q == nil {
		return
	}
	dropped := q.clear()
	s.logger(r.Context()).Warn("queue cleared", "queue", r.PathValue("queue"), "dropped", dropped)
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleQueues(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Name    string  `json:"name"`
		Length  int     `json:"length"`
		Bytes   int64   `json:"bytes"`
		Leased  int     `json:"leased"`
		HeadAge float64 `json:"head_age_sec"`
	}
	now := time.Now()
	var out []entry
	for _, e := range s.reg.all() {
		st := e.q.stats(now)
		out = append(out, entry{e.name, st.length, st.bytes, st.leased, st.headAgeSec})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"queues": out})
}

func (s *server) handlePersist(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("file")
	if name == "" {
		http.Error(w, "missing file parameter", http.StatusBadRequest)
		return
	}
	path, err := s.sanitizePath(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := s.reg.persist(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		s.logger(r.Context()).Error("persist failed", "file", name, "error", err.Error())
		return
	}
	s.logger(r.Context()).Info("snapshot persisted", "file", name)
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleLoad(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("file")
	if name == "" {
		http.Error(w, "missing file parameter", http.StatusBadRequest)
		return
	}
	path, err := s.sanitizePath(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := s.reg.load(path); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		s.logger(r.Context()).Error("load failed", "file", name, "error", err.Error())
		return
	}
	s.logger(r.Context()).Info("snapshot loaded", "file", name)
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"version": version})
}
