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

// --------------------------- configurable caps ------------------------------

var (
	maxBytes int64 = 256 << 20 // overridden by --max-bytes or QUEUE_MAX_BYTES
	maxMsgs  int   = 50000     // overridden by --max-msgs  or QUEUE_MAX_MSGS
)

// --------------------------- runtime counters -------------------------------

var (
	startTime   = time.Now()
	enqueueCnt  uint64
	dequeueCnt  uint64
	dequeueMiss uint64
	traceCtr    uint64
)

// --------------------------- tiny structured logger -------------------------

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newTraceID() string { // monotonic counter with random suffix, collision-safe
	v := atomic.AddUint64(&traceCtr, 1)
	return fmt.Sprintf("%d-%s", v, randHex(3))
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
	// one compact line
	fmt.Println(string(b))
}

// --------------------------- FIFO queue -------------------------------------

type queue struct {
	mu    sync.Mutex
	data  [][]byte
	bytes int64
}

func (q *queue) enqueue(b []byte) error {
	if len(b) > maxBody {
		return errors.New("payload too large")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.data) >= maxMsgs || q.bytes+int64(len(b)) > maxBytes {
		return errors.New("queue full")
	}
	cp := append([]byte(nil), b...)
	q.data = append(q.data, cp)
	q.bytes += int64(len(cp))
	atomic.AddUint64(&enqueueCnt, 1)
	return nil
}

func (q *queue) dequeue() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.data) == 0 {
		atomic.AddUint64(&dequeueMiss, 1)
		return nil, false
	}
	m := q.data[0]
	q.data[0] = nil
	q.data = q.data[1:]
	q.bytes -= int64(len(m))
	atomic.AddUint64(&dequeueCnt, 1)
	return m, true
}

func (q *queue) clear() {
	q.mu.Lock()
	for i := range q.data {
		q.data[i] = nil
	}
	q.data = nil
	q.bytes = 0
	q.mu.Unlock()
}

func (q *queue) length() int {
	q.mu.Lock()
	l := len(q.data)
	q.mu.Unlock()
	return l
}

func (q *queue) size() int64 {
	q.mu.Lock()
	s := q.bytes
	q.mu.Unlock()
	return s
}

func (q *queue) persist(p string) error {
	q.mu.Lock()
	snap := make([][]byte, len(q.data))
	copy(snap, q.data)
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
	q.data = d
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
	files, _ := filepath.Glob("/proc/self/fd/*")
	return len(files), int(lim.Cur)
}

// context key for trace-id
type traceKey struct{}

func withTrace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := r.Header.Get("X-Trace-Id")
		if tid == "" {
			tid = newTraceID()
		}
		ctx := context.WithValue(r.Context(), traceKey{}, tid)
		w.Header().Set("X-Trace-Id", tid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func traceFrom(ctx context.Context) string {
	if v := ctx.Value(traceKey{}); v != nil {
		return v.(string)
	}
	return ""
}

// --------------------------- main -------------------------------------------

func main() {
	// ---------------- flags & env -------------------------------------------
	addr := flag.String("addr", ":8080", "listen address")
	cert := flag.String("tls-cert", "", "TLS certificate file (optional)")
	key := flag.String("tls-key", "", "TLS key file (optional)")
	maxBytesFlag := flag.String("max-bytes", "", "queue max bytes (e.g. 2G, 512M)")
	maxMsgsFlag := flag.Int("max-msgs", 0, "queue max messages")
	flag.Parse()

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

	// ---------------- GC hint ------------------------------------------------
	if gomem := os.Getenv("GOMEMLIMIT"); gomem == "" {
		suggest := int64(math.Ceil(float64(maxBytes) * 2))
		logJSON("warn", "", "GOMEMLIMIT not set; consider", "suggest_bytes", suggest)
	} else {
		logJSON("info", "", "GOMEMLIMIT detected", "value", gomem)
	}

	// ---------------- mux & middleware --------------------------------------
	q := &queue{}
	mux := http.NewServeMux()

	// enqueue
	mux.HandleFunc("/enqueue", func(w http.ResponseWriter, r *http.Request) {
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
		if err := q.enqueue(body); err != nil {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			logJSON("warn", trace, "enqueue rejected", "reason", err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	// dequeue
	mux.HandleFunc("/dequeue", func(w http.ResponseWriter, r *http.Request) {
		trace := traceFrom(r.Context())
		msg, ok := q.dequeue()
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(msg)
		logJSON("info", trace, "dequeue ok", "bytes", len(msg))
	})

	// metrics
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
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
		}{
			int64(time.Since(startTime).Seconds()),
			atomic.LoadUint64(&enqueueCnt),
			atomic.LoadUint64(&dequeueCnt),
			atomic.LoadUint64(&dequeueMiss),
			q.length(),
			q.size(),
			open,
			limit,
			runtime.NumGoroutine(),
		}
		json.NewEncoder(w).Encode(m)
	})

	// admin helpers
	mux.HandleFunc("/clear", func(w http.ResponseWriter, r *http.Request) {
		q.clear()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/persist", func(w http.ResponseWriter, r *http.Request) {
		file := r.URL.Query().Get("file")
		if file == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := q.persist(file); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/load", func(w http.ResponseWriter, r *http.Request) {
		file := r.URL.Query().Get("file")
		if file == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := q.load(file); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	// wrap with trace middleware
	handler := withTrace(mux)

	// server setup
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
	)

	// ---------------- start listening ---------------------------------------
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

	// graceful shutdown
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
