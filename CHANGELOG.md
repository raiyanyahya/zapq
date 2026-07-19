# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [SemVer](https://semver.org/).

## [Unreleased]

Nothing yet.

## [1.1.0] - 2026-07-19

Everything in this release is backward compatible with 1.0.0: the unprefixed endpoints, flags, env vars, metrics fields and old JSON snapshots all keep working.

### Added
- **Binary TCP protocol (ZQP1)** via `--tcp-addr` — a pipelined, length-prefixed wire format sharing the same queues, token scopes and limits as HTTP ([spec](docs/PROTOCOL.md)). Benchmarked at 13µs p50 round trips (vs 46µs HTTP) and 2M+ individual msg/s on a single pipelined connection (vs 183k over HTTP with 200 connections). Supports auth, enqueue, dequeue with `wait`/`lease`, ack and ping; TLS when the server has `--tls-cert`; `zapq_tcp_connections` gauge. The Go client gains `client.DialTCP` with `TCPAuth`/`TCPTLS`/`TCPTimeout` options.
- **At-least-once delivery**: `GET /dequeue?lease=30s` holds the message on a lease (`X-Lease-Id` header); `POST /ack?id=...` settles it. Unacked leases are requeued at the front when they expire (reaper runs every second, `--max-lease` caps duration, `zapq_lease_expirations_total` counts redeliveries). Leased messages keep counting against capacity so a full queue can never strand a retry.
- **Named queues**: `/q/{name}/...` variants of every data endpoint, created lazily with the configured caps, bounded by `--max-queues`. Unprefixed endpoints keep operating on the `default` queue. `GET /queues` lists them.
- **Batch operations**: `POST /enqueue/batch` (all-or-nothing) and `GET /dequeue/batch?max=N` move up to 1000 messages per request using a length-prefixed binary framing (`application/vnd.zapq.frames`); batch dequeue supports `wait` and `lease` (IDs in `X-Lease-Ids`).
- **Scoped tokens**: `--producer-token`, `--consumer-token`, `--admin-token` alongside the full-access `--api-token`; producers can't dequeue or clear, consumers can't enqueue.
- **Head-of-line message age**: enqueue timestamps ride along with messages (surviving snapshots) and feed `zapq_head_message_age_seconds{queue=...}` plus `head_age_sec` in JSON — the queue-latency metric to alert on.
- **Snapshot format v2**: binary, versioned magic, CRC32 trailer, registry-wide (all queues, unacked leases included). Corruption and truncation are detected at load; legacy JSON snapshots still load into the default queue.
- **Long-poll backpressure**: `--max-waiters` caps parked `wait=` requests (503 + `Retry-After` beyond it); `zapq_waiting_consumers` gauge.
- **Official Go client** (`github.com/raiyanyahya/zapq/client`): enqueue/dequeue/lease/ack/batch with jittered-backoff retries on 429/503 and transport errors.
- **OpenAPI 3.0 spec** (`openapi.yaml`) covering the full API.
- **Supply chain**: CodeQL and OpenSSF Scorecard workflows, Dependabot, cosign-signed release checksums and container images (keyless), SPDX SBOM on releases, native Go fuzz targets (parseSize, snapshot loader, frame codec) running in CI.
- **Container hardening**: image is now `FROM scratch` (no OS packages); `zapq --healthcheck` self-probe replaces wget for HEALTHCHECK.
- Long-poll dequeue: `GET /dequeue?wait=5s` holds the request until a message arrives (capped at 20s), so consumers no longer busy-poll.
- Optional bearer-token authentication (`--api-token` / `ZAPQ_API_TOKEN`) on all endpoints except `/health` and `/version`; constant-time comparison.
- Prometheus text exposition on `/metrics` via content negotiation (`Accept: text/plain` or `?format=prometheus`); JSON remains the default.
- Snapshot lifecycle: `--snapshot-file` restores on start and persists on graceful shutdown; `--snapshot-interval` adds periodic persistence.
- Per-message size cap is configurable: `--max-msg-bytes` / `ZAPQ_MAX_MSG_BYTES` (default unchanged at 128K).
- `--log-level` / `ZAPQ_LOG_LEVEL` with `log/slog` structured JSON logging; per-request debug logging with status and duration.
- Optional `/debug/pprof` endpoints behind `--pprof`.
- `Retry-After` header on 429 responses; `Content-Length` on dequeue responses; `404` when loading a missing snapshot.
- Docker image hardened: non-root user, `/data` volume, HEALTHCHECK; multi-arch image published to GHCR on tags.
- CI: gofmt check, staticcheck, govulncheck, darwin builds, release checksums.
- Deployment examples for systemd (hardened unit) and Kubernetes.
- SECURITY.md, CONTRIBUTING.md and this changelog.

### Fixed
- `/load` now validates snapshots against the queue's byte cap and per-message limit, not just message count.
- `/load` releases every stale buffer slot, so replaced messages no longer linger in memory until overwritten.
- Snapshots are fsynced before the atomic rename, so a crash right after persist can't leave a truncated file, and failed writes clean up their temp file.
- Invalid `QUEUE_MAX_BYTES`/`QUEUE_MAX_MSGS` environment values now fail startup instead of being silently ignored; zero/negative limits are rejected.
- Removed per-message logging from the dequeue hot path.

### Changed
- Source split from one file into `queue.go`, `server.go`, `metrics.go`, `main.go`. The module still builds a single zero-dependency binary.
- Logging switched from a hand-rolled JSON printer to `log/slog`.
- Routing uses Go 1.22 method patterns; wrong-method requests get `405` with an `Allow` header.
- HTTP server sets `ReadHeaderTimeout` and `MaxHeaderBytes`.
- `parseSize` accepts a `K` suffix and rejects negative sizes.

[Unreleased]: https://github.com/raiyanyahya/zapq/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/raiyanyahya/zapq/compare/v1.0.0...v1.1.0
