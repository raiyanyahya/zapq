// go run load_test.go -addr http://localhost:8080 -messages 1000000 -csv results.csv
package main

import (
	"bytes"
	"encoding/csv"
	"flag"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var hdr = []string{"kind", "latency_ns"} // kind=enqueue|dequeue

func main() {
	addr := flag.String("addr", "http://localhost:8080", "server address")
	msgs := flag.Int("messages", 10_000_000, "messages to send")
	prod := flag.Int("producers", 200, "concurrent producers")
	cons := flag.Int("consumers", 200, "concurrent consumers")
	size := flag.Int("payload", 128, "bytes per message")
	out := flag.String("csv", "results.csv", "output CSV")
	flag.Parse()

	perProducer := (*msgs + *prod - 1) / *prod
	log.Printf("starting load-test: %d msgs, %d prod, %d cons", *msgs, *prod, *cons)

	var enqOK, deqOK uint64
	csvCh := make(chan []string, 1<<16)
	go func() {
		f, _ := os.Create(*out)
		w := csv.NewWriter(f)
		_ = w.Write(hdr)
		for rec := range csvCh {
			_ = w.Write(rec)
		}
		w.Flush()
		f.Close()
	}()

	payload := bytes.Repeat([]byte{0xaa}, *size)
	wg := sync.WaitGroup{}

	// producers
	wg.Add(*prod)
	for p := 0; p < *prod; p++ {
		go func(id int) {
			defer wg.Done()
			client := &http.Client{}
			buf := bytes.NewReader(payload)
			for sent := 0; sent < perProducer; {
				buf.Reset(payload)
				t0 := time.Now()
				resp, err := client.Post(*addr+"/enqueue", "application/octet-stream", buf)
				lat := time.Since(t0).Nanoseconds()
				if err == nil && resp.StatusCode == 202 {
					atomic.AddUint64(&enqOK, 1)
					csvCh <- []string{"enqueue", strconv.FormatInt(lat, 10)}
					sent++
				}
				if resp != nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				if err != nil || resp.StatusCode == 429 {
					time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)
				}
			}
		}(p)
	}

	// consumers
	wg.Add(*cons)
	for c := 0; c < *cons; c++ {
		go func() {
			defer wg.Done()
			client := &http.Client{}
			for atomic.LoadUint64(&deqOK) < uint64(*msgs) {
				t0 := time.Now()
				resp, err := client.Get(*addr + "/dequeue")
				lat := time.Since(t0).Nanoseconds()
				if err == nil && resp.StatusCode == 200 {
					atomic.AddUint64(&deqOK, 1)
					csvCh <- []string{"dequeue", strconv.FormatInt(lat, 10)}
				}
				if resp != nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}()
	}

	wg.Wait()
	close(csvCh)

	log.Printf("enqueued %d, dequeued %d – csv → %s",
		enqOK, deqOK, *out)
	if enqOK != deqOK {
		log.Fatalf("DATA LOSS! enq=%d deq=%d", enqOK, deqOK)
	}
}
