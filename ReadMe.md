# ⚡ ZapQ In‑Memory FIFO Queue Server

[![build](https://github.com/raiyanyahya/zapq/actions/workflows/release.yml/badge.svg?branch=master)](https://github.com/raiyanyahya/zapq/actions/workflows/release.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/raiyanyahya/zapq)](https://goreportcard.com/report/github.com/raiyanyahya/zapq)
[![PkgGoDev](https://pkg.go.dev/badge/github.com/raiyanyahya/zapq)](https://pkg.go.dev/github.com/raiyanyahya/zapq)

ZapQ is a tiny Go server that gives you a fast in memory FIFO message queue over HTTP. You push bytes in and pull them out in the same order. There is no config file, no external database and no clustering. It runs as a single binary and uses only RAM.

The queue uses a pre allocated ring buffer protected by a read write mutex. Enqueue and dequeue are both O(1) at every scale because there are no slice shifts and no growing arrays. Atomic counters track throughput without touching the lock so your metrics never slow down the hot path.

ZapQ is built for transient messages up to 128 KB each with hard memory and count caps to protect your node. If you need persistence, fan out or streams you want NATS, Kafka or Redis Streams. ZapQ is the tool you reach for when you just need a quick buffer next to your container.

## Benchmarks

These are measured on a Ryzen AI 7 350 with 16 threads. The queue itself runs in nanoseconds. The HTTP round trip dominates real world latency at about 50ms per call.

| Operation | Time | Allocations |
|-----------|------|-------------|
| Enqueue (128 byte payload) | 28 ns | 1 (payload copy) |
| Dequeue | 21 ns | 0 |
| Enqueue (parallel x16) | 95 ns | 1 |
| Dequeue (parallel x16) | 38 ns | 0 |
| Enqueue + dequeue round trip | 187 ns | 1 |
| Read length and size (parallel) | 64 ns | 0 |

## Endpoints

**`POST /enqueue`** Push a message in the request body. Returns `202` on success, `413` if the body is over 128 KB or `429` if the queue is full.

**`GET /dequeue`** Pop the next message off the front. Returns `200` with the raw bytes or `204` when the queue is empty.

**`POST /clear`** Empty the queue immediately. All data is discarded.

**`POST /persist?file=name`** Write a JSON snapshot of the current queue to a file inside the data directory. Returns `403` if the path tries to escape the data directory.

**`POST /load?file=name`** Load a previously persisted snapshot from the data directory. Replaces the entire in memory queue. Same path restrictions apply.

**`GET /metrics`** Returns JSON with uptime, enqueue and dequeue counters, queue length, bytes used, open file descriptors, goroutine count and the server version. Good for dashboards.

**`GET /health`** Always returns `ok`. Use this as a liveness probe.

**`GET /version`** Returns the version string injected at build time.

## Configuring the Server

You can set everything with flags or environment variables. Flags take priority.

| Flag | Env | Default | What it does |
|------|-----|---------|--------------|
| `--addr` | | `:8080` | Listen address |
| `--max-bytes` | `QUEUE_MAX_BYTES` | `256M` | Memory cap. Supports `M` and `G` suffixes |
| `--max-msgs` | `QUEUE_MAX_MSGS` | `50000` | Maximum number of messages |
| `--data-dir` | | `.` | Directory for persist and load operations |
| `--tls-cert` | | | Path to TLS certificate for HTTPS |
| `--tls-key` | | | Path to TLS key for HTTPS |

The server also respects `GOMEMLIMIT`. If you do not set it ZapQ prints a suggestion on startup.

## Running

You need Go 1.22 or later. On Linux raise your file descriptor limit for heavy loads:

```bash
ulimit -n 65535
```

Start with HTTP:

```bash
go run fifo_queue_server.go --addr :8080 --max-bytes 2G --max-msgs 100000
```

With TLS:

```bash
openssl req -x509 -newkey rsa:2048 -days 365 -nodes \
    -keyout key.pem -out cert.pem -subj "/CN=localhost"

go run fifo_queue_server.go --addr :8443 --tls-cert cert.pem --tls-key key.pem
```

With Docker:

```bash
docker build --build-arg VERSION=latest -t zapq .
docker run --rm -p 8080:8080 zapq
```

Or use the Makefile:

```bash
make build    # build the binary
make test     # run tests with race detector
make docker-build VERSION=v1.2.0
```

## How It Works

The queue is a fixed size ring buffer of byte slices. A read write mutex protects structural changes so multiple readers can check length and size concurrently while only writers block each other. Atomic counters sit outside the lock entirely.

When you enqueue the server copies your payload and places it at the tail position. When you dequeue it reads from the head position, clears that slot for the garbage collector and advances. Both operations touch exactly two integers (head or tail and count) plus one slice slot. No shifting. No growing. No hidden work that gets slower over time.

The persist endpoint takes a consistent snapshot under the write lock then does the JSON encoding and file I/O outside the lock so the queue keeps serving traffic during the write.

Shutdown is graceful. The server stops accepting new connections, drains in flight handlers with a 10 second timeout and then exits cleanly.

## Production Tips

Set `GOMEMLIMIT` to about double your max bytes so the Go garbage collector behaves predictably. Tune the message and byte caps based on your available RAM. Use a process supervisor like systemd to restart on crash it will pass SIGTERM and the server will shut down gracefully. Alert on `queue_length` if it keeps growing you have a consumer problem. Run a periodic persist cron if message loss matters to you. Have your clients retry with jitter when they see a 429.

## License

MIT
