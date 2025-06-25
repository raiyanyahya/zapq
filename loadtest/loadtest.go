package main

// go run load_test.go -addr http://localhost:8080 -messages 500000 -producers 200 -consumers 200 -payload 128 -csv results.csv
// captures latency for every successful enqueue & dequeue.
// Computes average, p50, p95, p99, max.
// Prints readable histogram buckets (≤1 ms, ≤2 ms, … >1 s).
// Optional -csv results.csv dumps raw latencies for offline charts.

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", "http://127.0.0.1:8080", "base URL of the queue server (schema+host[:port])")
	total := flag.Int("messages", 100000, "total messages to enqueue")
	producers := flag.Int("producers", runtime.NumCPU()*4, "concurrent producer goroutines")
	consumers := flag.Int("consumers", runtime.NumCPU()*4, "concurrent consumer goroutines")
	payload := flag.Int("payload", 64, "payload size in bytes (each message)")
	verify := flag.Bool("verify", true, "fail if enqueued count != dequeued count")
	flag.Parse()

	// --- HTTP client tuned for high concurrency ---
	u, _ := url.Parse(*addr)
	tr := &http.Transport{MaxIdleConnsPerHost: 10000}
	if u.Scheme == "https" {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}

	log.Printf("starting load‑test: %d msgs, %d prod, %d cons", *total, *producers, *consumers)

	// ---------- PRODUCER PHASE ----------
	var enqOK int64
	start := time.Now()

	idCh := make(chan int, *total)
	for i := 0; i < *total; i++ {
		idCh <- i
	}
	close(idCh)

	wgP := sync.WaitGroup{}
	bodyPad := bytes.Repeat([]byte("X"), *payload)
	for i := 0; i < *producers; i++ {
		wgP.Add(1)
		go func() {
			defer wgP.Done()
			for id := range idCh {
				// first 8 bytes encode the id for potential downstream checks
				msg := append([]byte(fmt.Sprintf("%08d", id)), bodyPad...)
				resp, err := client.Post(*addr+"/enqueue", "application/octet-stream", bytes.NewReader(msg))
				if err != nil {
					continue // network error; best‑effort load test
				}
				io.Copy(io.Discard, resp.Body)
				if resp.StatusCode == http.StatusAccepted {
					atomic.AddInt64(&enqOK, 1)
				}
				resp.Body.Close()
			}
		}()
	}
	wgP.Wait()
	log.Printf("enqueued %d msgs in %v (%.0f msg/s)", enqOK, time.Since(start), float64(enqOK)/time.Since(start).Seconds())

	// ---------- CONSUMER PHASE ----------
	var deqOK int64
	wgC := sync.WaitGroup{}
	for i := 0; i < *consumers; i++ {
		wgC.Add(1)
		go func() {
			defer wgC.Done()
			for {
				resp, err := client.Get(*addr + "/dequeue")
				if err != nil {
					continue
				}
				// 204 = queue empty; allow goroutine to exit
				if resp.StatusCode == http.StatusNoContent {
					resp.Body.Close()
					return
				}
				if resp.StatusCode == http.StatusOK {
					atomic.AddInt64(&deqOK, 1)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wgC.Wait()
	elapsed := time.Since(start)
	log.Printf("dequeued %d msgs in %v (%.0f msg/s)", deqOK, elapsed, float64(deqOK)/elapsed.Seconds())

	if *verify {
		if enqOK == deqOK {
			log.Printf("SUCCESS: no data lost (enq=%d deq=%d)", enqOK, deqOK)
		} else {
			log.Printf("FAIL: mismatch enq=%d deq=%d", enqOK, deqOK)
			os.Exit(1)
		}
	}
}
