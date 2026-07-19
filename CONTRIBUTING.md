# Contributing to ZapQ

Thanks for helping out. The bar for merging is simple: keep ZapQ small, fast and dependency-free.

## Ground rules

- **Zero dependencies.** The server and client build from the standard library only. PRs that add a module dependency need a very good reason.
- **The hot path stays hot.** `enqueue`/`take` are lock + copy + a few integer updates. Run `make bench` before and after; regressions need justification.
- **Everything is tested.** New behavior comes with tests, and `go test -race ./...` must pass. Parsers and decoders get fuzz targets.

## Workflow

```bash
git clone https://github.com/raiyanyahya/zapq && cd zapq
make test        # race detector + coverage
make lint        # staticcheck
make fuzz        # short fuzz run
make bench       # if you touched the queue
```

CI runs gofmt, vet, staticcheck, govulncheck, race-enabled tests and short fuzz sessions on every PR — running the same locally saves a round trip.

## What lives where

| Path | Contents |
|------|----------|
| `queue.go` | Ring buffer, leases/acks/expiry, batch ops, long-poll wait |
| `registry.go` | Named queue registry with lazy creation and limits |
| `snapshot.go` | Binary v2 snapshot format (CRC), legacy JSON loader |
| `server.go` | HTTP handlers, middleware, scoped-token auth |
| `tcp.go` | Binary TCP protocol listener (spec: `docs/PROTOCOL.md`) |
| `metrics.go` | JSON + Prometheus metrics, per-queue series |
| `main.go` | Flags/env config, lifecycle, lease reaper, healthcheck mode |
| `internal/frames/` | Batch wire format shared by server and client |
| `client/` | Official Go client (importable package) |
| `openapi.yaml` | The API contract — update it alongside handler changes |

## Scope

Good PRs: bug fixes, performance work, operational polish (metrics, logging, deployment examples), client improvements, documentation.

In scope by design: FIFO queues, leases/acks (at-least-once), batching, snapshots.

Out of scope (by design): clustering/replication, consumer groups, pluggable storage backends, exactly-once delivery. If you need those, you need a different tool — and that's fine.

## Releases

Maintainers tag `vX.Y.Z` on master; CI builds the binaries, checksums, SBOM and the signed multi-arch Docker image, and drafts the GitHub release. Please update `CHANGELOG.md` in your PR under *Unreleased*.
