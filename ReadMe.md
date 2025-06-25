# ⚡ ZapQ In‑Memory FIFO Queue Server

A single‑binary Go microservice that exposes a **First‑In / First‑Out message queue** completely in RAM. This README explains **what every endpoint does, why it works internally, and the key computer‑science principles** behind each subsystem.

> **TL;DR**
> * Ultra‑low‑latency queue for transient messages (≤ 128 KB each).
> * Hard caps (`maxBytes`, `maxMsgs`) guard RAM.
> * Concurrency‑safe via a mutex + atomic counters.
> * JSON metrics, TLS option, graceful shutdown.

---

## 1. Architecture Overview

```
 ┌────────────┐   HTTP/HTTPS    ┌────────────────────────┐   Goroutines   ┌─────────────┐
 │  Producers │ ───────────────▶│  queue ([]byte slices) │◀──────────────▶│  Consumers  │
 └────────────┘                 └────────────────────────┘                └─────────────┘
```

* **queue.data** – a slice of byte‑slices (`[][]byte`). Index **0** is always the next item to pop, satisfying FIFO.
* **queue.mu** – a single `sync.Mutex` serialises structural modifications while permitting **parallel HTTP clients**.
* **queue.bytes** – running total for O(1) memory‑cap checks.
* **Atomic counters** record events without taking the mutex (cache‑friendly, lock‑free).

### Time & Space Complexity
| Operation  | Time | Space Comments |
|-----------|------|----------------|
| `enqueue` | **O(1)** append; amortised as Go grows slice capacity exponentially.
| `dequeue` | **O(1)** slice header moves, old entry set to `nil` for GC.
| `clear`   | **O(n)** nils everything so GC can reclaim.

Mutex ensures **linearizability** (each op appears instantaneous). Because we hold the lock only long enough to modify the slice, throughput scales linearly until contention on `enqueue`/`dequeue` dominates.

---

## 2. Endpoint Walk‑Through & CS Background

| Path / Verb | Semantics | Response | Internals & Rationale |
|-------------|-----------|----------|----------------------|
| **`POST /enqueue`** | Push a message (≤ 128 KB). | `202 Accepted` or `429 Queue Full` or `413 Payload Too Large`. | *Reads the body into a byte‑slice*. Calls `enqueue`. Checks both **count‑cap** (`maxMsgs`) and **byte‑cap** (`maxBytes`) in **O(1)**. On success increments `enqueueCnt` atomically. |
| **`GET /dequeue`** | Pop next message. | `200` with raw bytes or `204 No Content` if empty. | `dequeue` grabs mutex, returns first item, shifts slice head. Failure path increments `dequeueMiss` without lock. |
| **`POST /clear`** | Purge all data. | `200 OK`. | Nils every slice entry so Go’s tri‑colour GC can reclaim memory quickly; resets counters. |
| **`POST /persist?file=path.json`** | Snapshot queue to disk (blocking). | `200` or `500`. | **Snapshot copy** removes lock early to minimise stall, then JSON‑encodes for human‑readability. O(n) IO. |
| **`POST /load?file=path.json`** | Replace in‑memory queue with file contents. | `200` or `500`. | Validates file, recomputes `bytes` in one pass. |
| **`GET /metrics`** | Human‑friendly JSON telemetry. | `{…}` | Shows uptime, counters, queue size, Go goroutines, and FD usage (reads `/proc/self/fd`). Ideal for dashboards or Prometheus “textfile” collector. |
| **`GET /health`** | Liveness probe. | `ok` | Constant‑time, lock‑free. |

### Why JSON not Protobuf?
* Zero client codegen, trivial `curl` debugging, small payload (few hundred bytes).

---

## 3. Compile & Run

### Prerequisites
* Go ≥ 1.22
* (Linux) raise FD limit for heavy load tests: `ulimit -n 65535`

### Quick start (HTTP)
```bash
# Build & run with 2 GiB RAM cap and 100 k messages cap
QUEUE_MAX_BYTES=2G go run fifo_queue_server.go \
   --addr :8080 --max-msgs 100000
```

### Enable HTTPS
```bash
openssl req -x509 -newkey rsa:2048 -days 365 -nodes \
        -keyout key.pem -out cert.pem -subj "/CN=localhost"

go run fifo_queue_server.go --addr :8443 \
        --tls-cert cert.pem --tls-key key.pem
```
Now `curl -k https://localhost:8443/health` ➜ `ok`.

### Flags & Env
| Flag / Env          | Default    | Description |
|---------------------|-----------|-------------|
| `--addr`            | `:8080`   | Where to listen (`host:port`). |
| `--tls-cert/key`    | *empty*   | Enable TLS. |
| `--max-bytes` / `QUEUE_MAX_BYTES` | `256M` | Memory cap (bytes, M, G). |
| `--max-msgs` / `QUEUE_MAX_MSGS`   | `50000` | Count cap. |

*Flags beat env vars.*

---

## 4. Graceful Shutdown Explained

The server goroutine listens for **`SIGINT`/`SIGTERM`**:
```go
sigCh := make(chan os.Signal,1)
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
<-sigCh // blocking
```
Then runs `srv.Shutdown(ctx)` which:
1. Stops `Listen` accepting new sockets.  
2. Waits for in‑flight handlers to return (10 s timeout).  
3. Active goroutines finish `enqueue/ dequeue`, so no message corruption.

This follows Go’s **graceful‑shutdown contract**, ensuring linearizability even during redeploys.

---

## 5. Computer‑Science Nuggets

* **Critical Section** – guarded by a single mutex. Simpler than read‑write locks because writes dominate.
* **Copy‑on‑enqueue** – `cp := append([]byte(nil), b...)` avoids aliasing if caller reuses slice; preserves queue immutability.
* **Slice‑shifting** – Cheap header move; memory of popped item reclaimed after next GC cycle.
* **Atomic counters** – Use `sync/atomic` so metrics don’t need mutex (lock‑free, wait‑free).
* **Memory Cap Enforcement** – Prevents unbounded growth, a common cause of **OOM‑killer** crashes.
* **File‑descriptor telemetry** – Reading `/proc/self/fd` counts live FDs, helping detect `EMFILE` risk.
* **Back‑pressure via 429** – Follows HTTP semantics; clients must retry or throttle. This protects server latency (queue spill‑over ≈ head‑of‑line blocking).
* **Payload limit 128 KB** – Small enough to avoid pathological GC pauses; tweak `maxBody` constant if needed.

---

## 6. Load‑Testing Examples

```bash
# 100 parallel enqueues, 50k messages total
hey -m POST -d @<(head -c 16 /dev/urandom | base64) \
    -c 100 -n 50000 http://localhost:8080/enqueue

# In another shell
watch -n1 'curl -s http://localhost:8080/metrics | jq "{len:.queue_length, bytes:.queue_bytes}"'
```
Observe `queue_length` grow, then drain as consumers dequeue.

---

## 7. Production Checklist (Why It Matters)

| Item | Reason |
|------|--------|
| **`ulimit -n` ≥ 10 k** | Prevent `EMFILE` under heavy concurrency; FD count exposed via `/metrics`. |
| **Caps tuned** | Protect node‑RAM; shrink caps in staging. |
| **Process supervisor** | Auto‑restart on crash; passes SIGTERM for graceful shutdown. |
| **TLS or upstream termination** | Protect messages on the wire (privacy, integrity). |
| **Alerting** (`queue_length`, `uptime_sec`) | Spot backlog or unexpected restarts. |
| **Regular `/persist` cron** (optional) | Hot‑backup if message loss is costly. |
| **Client retry w/ jitter** | Avoid thundering herd when server returns 429. |

---

## 8. Extending the Server (Ideas)

* Replace global mutex with **segment locks** or a **lock‑free ring buffer** for higher parallelism.
* Add **priority queues** using a heap instead of FIFO slice.
* Swap JSON snapshot with **binary protobuf** for faster I/O.
* Integrate with **systemd‑sdnotify** for readiness signals.

Pull requests welcome! 🎉

---

## License

MIT – Hack, learn, enjoy.
