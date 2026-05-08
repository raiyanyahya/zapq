package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const maxBody = 128 * 1024

var version = "dev" // set via -ldflags at build time

// --------------------------- structured logger ------------------------------

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("rand.Read: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func logJSON(level, trace, msg string, kv ...any) {
	event := map[string]any{
		"time":  time.Now().Format(time.RFC3339Nano),
		"level": strings.ToUpper(level),
		"msg":   msg,
	}
	if trace != "" {
		event["trace_id"] = trace
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if key, ok := kv[i].(string); ok {
			event[key] = kv[i+1]
		}
	}
	b, _ := json.Marshal(event)
	fmt.Println(string(b))
}

// --------------------------- counters ---------------------------------------

type counters struct {
	enqueues      atomic.Uint64
	dequeues      atomic.Uint64
	dequeuesEmpty atomic.Uint64
	trace         atomic.Uint64
}

func (c *counters) newTraceID() string {
	v := c.trace.Add(1)
	return fmt.Sprintf("%d-%s", v, randHex(3))
}

// --------------------------- ring-buffer FIFO queue -------------------------

type queue struct {
	mu       sync.RWMutex
	buf      [][]byte
	head     int
	tail     int
	count    int
	bytes    int64
	maxBytes int64
}

func newQueue(maxBytes int64, maxMsg int) *queue {
	return &queue{
		buf:      make([][]byte, maxMsg),
		maxBytes: maxBytes,
	}
}

func (q *queue) enqueue(b []byte, c *counters) error {
	if len(b) > maxBody {
		return errors.New("payload too large")
	}
	q.mu.Lock()
	if q.count >= len(q.buf) || q.bytes+int64(len(b)) > q.maxBytes {
		q.mu.Unlock()
		return errors.New("queue full")
	}
	cp := append([]byte(nil), b...)
	q.buf[q.tail] = cp
	q.tail = (q.tail + 1) % len(q.buf)
	q.count++
	q.bytes += int64(len(cp))
	q.mu.Unlock()
	c.enqueues.Add(1)
	return nil
}

func (q *queue) dequeue(c *counters) ([]byte, bool) {
	q.mu.Lock()
	if q.count == 0 {
		q.mu.Unlock()
		c.dequeuesEmpty.Add(1)
		return nil, false
	}
	m := q.buf[q.head]
	q.buf[q.head] = nil
	q.head = (q.head + 1) % len(q.buf)
	q.count--
	q.bytes -= int64(len(m))
	q.mu.Unlock()
	c.dequeues.Add(1)
	return m, true
}

func (q *queue) clear() {
	q.mu.Lock()
	for q.count > 0 {
		q.buf[q.head] = nil
		q.head = (q.head + 1) % len(q.buf)
		q.count--
	}
	q.bytes = 0
	q.mu.Unlock()
}

func (q *queue) length() int {
	q.mu.RLock()
	c := q.count
	q.mu.RUnlock()
	return c
}

func (q *queue) size() int64 {
	q.mu.RLock()
	s := q.bytes
	q.mu.RUnlock()
	return s
}

func (q *queue) snapshot() [][]byte {
	snap := make([][]byte, 0, q.count)
	for i := 0; i < q.count; i++ {
		idx := (q.head + i) % len(q.buf)
		snap = append(snap, q.buf[idx])
	}
	return snap
}

func (q *queue) persist(p string) error {
	q.mu.Lock()
	snap := q.snapshot()
	q.mu.Unlock()

	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(f).Encode(snap); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (q *queue) load(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	var d [][]byte
	if err = json.NewDecoder(f).Decode(&d); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	var total int64
	for _, b := range d {
		total += int64(len(b))
	}
	q.mu.Lock()
	if len(d) > len(q.buf) {
		q.mu.Unlock()
		return errors.New("snapshot exceeds queue capacity")
	}
	q.head = 0
	for i, b := range d {
		q.buf[i] = b
	}
	q.tail = len(d) % len(q.buf)
	q.count = len(d)
	q.bytes = total
	q.mu.Unlock()
	return nil
}

// --------------------------- helpers ----------------------------------------

func parseSize(s string) (int64, error) {
	u := strings.ToUpper(strings.TrimSpace(s))
	switch {
	case strings.HasSuffix(u, "G"):
		n, err := strconv.ParseInt(strings.TrimSuffix(u, "G"), 10, 64)
		return n << 30, err
	case strings.HasSuffix(u, "M"):
		n, err := strconv.ParseInt(strings.TrimSuffix(u, "M"), 10, 64)
		return n << 20, err
	default:
		return strconv.ParseInt(u, 10, 64)
	}
}

func openFDs() (int, int) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return -1, -1
	}
	files, err := filepath.Glob("/proc/self/fd/*")
	if err != nil {
		return -1, int(lim.Cur)
	}
	return len(files), int(lim.Cur)
}

// --------------------------- server -----------------------------------------

type traceKey struct{}

func traceFrom(ctx context.Context) string {
	if v := ctx.Value(traceKey{}); v != nil {
		return v.(string)
	}
	return ""
}

type server struct {
	q         *queue
	c         *counters
	startTime time.Time
	dataDir   string
}

func (s *server) traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := r.Header.Get("X-Trace-Id")
		if tid == "" {
			tid = s.c.newTraceID()
		}
		ctx := context.WithValue(r.Context(), traceKey{}, tid)
		w.Header().Set("X-Trace-Id", tid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *server) sanitizePath(name string) (string, error) {
	clean := filepath.Clean(filepath.Join(s.dataDir, name))
	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", err
	}
	absDir, err := filepath.Abs(s.dataDir)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, absDir+string(os.PathSeparator)) && abs != absDir {
		return "", errors.New("path escapes data directory")
	}
	return abs, nil
}

// --- HTTP handlers ----------------------------------------------------------

func (s *server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	trace := traceFrom(r.Context())
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		logJSON("error", trace, "read body failed", "error", err.Error())
		return
	}
	if len(body) > maxBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		logJSON("warn", trace, "payload too large", "bytes", len(body))
		return
	}
	if err := s.q.enqueue(body, s.c); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		logJSON("warn", trace, "enqueue rejected", "reason", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) handleDequeue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	trace := traceFrom(r.Context())
	msg, ok := s.q.dequeue(s.c)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(msg)
	logJSON("info", trace, "dequeue ok", "bytes", len(msg))
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	open, limit := openFDs()
	m := struct {
		UptimeSec   int64  `json:"uptime_sec"`
		Enqueues    uint64 `json:"enqueues"`
		Dequeues    uint64 `json:"dequeues_ok"`
		DequeueMiss uint64 `json:"dequeues_empty"`
		Length      int    `json:"queue_length"`
		Bytes       int64  `json:"queue_bytes"`
		OpenFD      int    `json:"open_fds"`
		MaxFD       int    `json:"max_fds"`
		Goroutines  int    `json:"goroutines"`
		Version     string `json:"version"`
	}{
		int64(time.Since(s.startTime).Seconds()),
		s.c.enqueues.Load(),
		s.c.dequeues.Load(),
		s.c.dequeuesEmpty.Load(),
		s.q.length(),
		s.q.size(),
		open,
		limit,
		runtime.NumGoroutine(),
		version,
	}
	json.NewEncoder(w).Encode(m)
}

func (s *server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.q.clear()
	w.WriteHeader(http.StatusOK)
}

func (s *server) handlePersist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	path, err := s.sanitizePath(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := s.q.persist(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	path, err := s.sanitizePath(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := s.q.load(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Write([]byte("ok"))
}

func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"version": version})
}

// --------------------------- main -------------------------------------------

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	cert := flag.String("tls-cert", "", "TLS certificate file (optional)")
	key := flag.String("tls-key", "", "TLS key file (optional)")
	maxBytesFlag := flag.String("max-bytes", "", "queue max bytes (e.g. 2G, 512M)")
	maxMsgsFlag := flag.Int("max-msgs", 0, "queue max messages")
	dataDirFlag := flag.String("data-dir", ".", "directory for persist/load operations")
	flag.Parse()

	maxBytes := int64(256 << 20)
	maxMsgs := 50000

	if *maxBytesFlag != "" {
		if b, err := parseSize(*maxBytesFlag); err == nil {
			maxBytes = b
		} else {
			logJSON("error", "", "invalid --max-bytes", "error", err.Error())
			os.Exit(1)
		}
	} else if env := os.Getenv("QUEUE_MAX_BYTES"); env != "" {
		if b, err := parseSize(env); err == nil {
			maxBytes = b
		}
	}
	if *maxMsgsFlag != 0 {
		maxMsgs = *maxMsgsFlag
	} else if env := os.Getenv("QUEUE_MAX_MSGS"); env != "" {
		if m, err := strconv.Atoi(env); err == nil {
			maxMsgs = m
		}
	}

	dataDir, err := filepath.Abs(*dataDirFlag)
	if err != nil {
		logJSON("error", "", "invalid data-dir", "error", err.Error())
		os.Exit(1)
	}

	if gomem := os.Getenv("GOMEMLIMIT"); gomem == "" {
		suggest := int64(math.Ceil(float64(maxBytes) * 2))
		logJSON("warn", "", "GOMEMLIMIT not set; consider", "suggest_bytes", suggest)
	} else {
		logJSON("info", "", "GOMEMLIMIT detected", "value", gomem)
	}

	q := newQueue(maxBytes, maxMsgs)
	c := &counters{}
	s := &server{q: q, c: c, startTime: time.Now(), dataDir: dataDir}

	mux := http.NewServeMux()
	mux.HandleFunc("/enqueue", s.handleEnqueue)
	mux.HandleFunc("/dequeue", s.handleDequeue)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/clear", s.handleClear)
	mux.HandleFunc("/persist", s.handlePersist)
	mux.HandleFunc("/load", s.handleLoad)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/version", s.handleVersion)

	handler := s.traceMiddleware(mux)

	srv := &http.Server{
		Addr:         *addr,
		Handler:      handler,
		IdleTimeout:  120 * time.Second,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	logJSON("info", "", "queue listening",
		"addr", *addr,
		"tls", *cert != "" && *key != "",
		"max_bytes", maxBytes,
		"max_msgs", maxMsgs,
		"data_dir", dataDir,
		"version", version,
	)

	go func() {
		var err error
		if *cert != "" && *key != "" {
			err = srv.ListenAndServeTLS(*cert, *key)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logJSON("fatal", "", "listen failed", "error", err.Error())
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logJSON("info", "", "shutdown signal received", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logJSON("error", "", "graceful shutdown failed", "error", err.Error())
	}
	logJSON("info", "", "bye")
}
