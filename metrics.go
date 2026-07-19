package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type queueMetrics struct {
	Length      int     `json:"length"`
	Bytes       int64   `json:"bytes"`
	Leased      int     `json:"leased"`
	HeadAgeSec  float64 `json:"head_age_sec"`
	Enqueues    uint64  `json:"enqueues"`
	Dequeues    uint64  `json:"dequeues_ok"`
	DequeueMiss uint64  `json:"dequeues_empty"`
	Acks        uint64  `json:"acks"`
	Expirations uint64  `json:"lease_expirations"`
}

// metricsSnapshot keeps the original aggregate field names for backward
// compatibility and adds per-queue breakdowns.
type metricsSnapshot struct {
	UptimeSec   int64                   `json:"uptime_sec"`
	Enqueues    uint64                  `json:"enqueues"`
	Dequeues    uint64                  `json:"dequeues_ok"`
	DequeueMiss uint64                  `json:"dequeues_empty"`
	Acks        uint64                  `json:"acks"`
	Expirations uint64                  `json:"lease_expirations"`
	Length      int                     `json:"queue_length"`
	Bytes       int64                   `json:"queue_bytes"`
	Leased      int                     `json:"leased"`
	Waiting     int64                   `json:"waiting_consumers"`
	TCPConns    int64                   `json:"tcp_connections"`
	OpenFD      int                     `json:"open_fds"`
	MaxFD       int                     `json:"max_fds"`
	Goroutines  int                     `json:"goroutines"`
	Version     string                  `json:"version"`
	Queues      map[string]queueMetrics `json:"queues"`
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

func (s *server) metrics() metricsSnapshot {
	open, limit := openFDs()
	m := metricsSnapshot{
		UptimeSec:  int64(time.Since(s.startTime).Seconds()),
		Waiting:    s.waiters.Load(),
		TCPConns:   s.tcpConns.Load(),
		OpenFD:     open,
		MaxFD:      limit,
		Goroutines: runtime.NumGoroutine(),
		Version:    version,
		Queues:     make(map[string]queueMetrics),
	}
	now := time.Now()
	for _, e := range s.reg.all() {
		st := e.q.stats(now)
		qm := queueMetrics{
			Length:      st.length,
			Bytes:       st.bytes,
			Leased:      st.leased,
			HeadAgeSec:  st.headAgeSec,
			Enqueues:    e.q.c.enqueues.Load(),
			Dequeues:    e.q.c.dequeues.Load(),
			DequeueMiss: e.q.c.dequeuesEmpty.Load(),
			Acks:        e.q.c.acks.Load(),
			Expirations: e.q.c.expirations.Load(),
		}
		m.Queues[e.name] = qm
		m.Enqueues += qm.Enqueues
		m.Dequeues += qm.Dequeues
		m.DequeueMiss += qm.DequeueMiss
		m.Acks += qm.Acks
		m.Expirations += qm.Expirations
		m.Length += qm.Length
		m.Bytes += qm.Bytes
		m.Leased += qm.Leased
	}
	return m
}

// wantsPrometheus reports whether the client asked for the Prometheus text
// exposition format. Prometheus scrapers send Accept: text/plain (or
// application/openmetrics-text); JSON stays the default for everyone else.
func wantsPrometheus(r *http.Request) bool {
	if f := r.URL.Query().Get("format"); f != "" {
		return f == "prometheus"
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/plain") || strings.Contains(accept, "openmetrics")
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := s.metrics()
	if wantsPrometheus(r) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writePrometheus(w, m)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

func writePrometheus(w http.ResponseWriter, m metricsSnapshot) {
	// aggregate series keep their original names
	writeMetric(w, "zapq_uptime_seconds", "gauge", "Seconds since the server started.", float64(m.UptimeSec))
	writeMetric(w, "zapq_enqueues_total", "counter", "Total messages accepted.", float64(m.Enqueues))
	writeMetric(w, "zapq_dequeues_total", "counter", "Total messages delivered.", float64(m.Dequeues))
	writeMetric(w, "zapq_dequeues_empty_total", "counter", "Dequeue attempts that found the queue empty.", float64(m.DequeueMiss))
	writeMetric(w, "zapq_acks_total", "counter", "Total leases acknowledged.", float64(m.Acks))
	writeMetric(w, "zapq_lease_expirations_total", "counter", "Total leases that expired and were requeued.", float64(m.Expirations))
	writeMetric(w, "zapq_queue_length", "gauge", "Messages currently queued across all queues.", float64(m.Length))
	writeMetric(w, "zapq_queue_bytes", "gauge", "Bytes currently queued across all queues.", float64(m.Bytes))
	writeMetric(w, "zapq_leased", "gauge", "Messages currently out on lease.", float64(m.Leased))
	writeMetric(w, "zapq_waiting_consumers", "gauge", "Long-poll requests currently parked.", float64(m.Waiting))
	writeMetric(w, "zapq_tcp_connections", "gauge", "Open binary-protocol TCP connections.", float64(m.TCPConns))
	writeMetric(w, "zapq_open_fds", "gauge", "Open file descriptors.", float64(m.OpenFD))
	writeMetric(w, "zapq_max_fds", "gauge", "File descriptor limit.", float64(m.MaxFD))
	writeMetric(w, "zapq_goroutines", "gauge", "Live goroutines.", float64(m.Goroutines))

	// per-queue series (queue names are restricted to [A-Za-z0-9._-])
	labeled := func(name, kind, help string, get func(queueMetrics) float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		for qn, qm := range m.Queues {
			fmt.Fprintf(w, "%s{queue=%q} %g\n", name, qn, get(qm))
		}
	}
	labeled("zapq_queue_msgs", "gauge", "Messages currently queued.", func(q queueMetrics) float64 { return float64(q.Length) })
	labeled("zapq_queue_msg_bytes", "gauge", "Bytes currently queued.", func(q queueMetrics) float64 { return float64(q.Bytes) })
	labeled("zapq_queue_leased", "gauge", "Messages out on lease.", func(q queueMetrics) float64 { return float64(q.Leased) })
	labeled("zapq_head_message_age_seconds", "gauge", "Age of the oldest queued message.", func(q queueMetrics) float64 { return q.HeadAgeSec })
	labeled("zapq_queue_enqueues_total", "counter", "Messages accepted.", func(q queueMetrics) float64 { return float64(q.Enqueues) })
	labeled("zapq_queue_dequeues_total", "counter", "Messages delivered.", func(q queueMetrics) float64 { return float64(q.Dequeues) })
	labeled("zapq_queue_acks_total", "counter", "Leases acknowledged.", func(q queueMetrics) float64 { return float64(q.Acks) })
	labeled("zapq_queue_lease_expirations_total", "counter", "Leases expired and requeued.", func(q queueMetrics) float64 { return float64(q.Expirations) })

	fmt.Fprintf(w, "# HELP zapq_build_info Build information.\n# TYPE zapq_build_info gauge\nzapq_build_info{version=%q} 1\n", m.Version)
}

func writeMetric(w http.ResponseWriter, name, kind, help string, v float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %g\n", name, help, name, kind, name, v)
}
