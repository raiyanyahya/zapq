# ZapQ Binary TCP Protocol (ZQP1)

The binary protocol is the low-latency alternative to HTTP, enabled with
`--tcp-addr :6789`. Same queues, same auth scopes, same limits — a different
wire. Use it when the queue sits on your hot path: it removes HTTP parsing
and, because responses always return in request order, clients can pipeline
requests and amortize syscalls.

All integers are big-endian. Strings are raw bytes (no NUL termination).

## Handshake

Immediately after connecting (TLS first if the server runs `--tls-cert`),
the client sends the 4-byte magic:

```
"ZQP1"
```

Anything else and the server closes the connection. If the server has any
token configured, the connection starts with no permissions — send `AUTH`
first.

## Request

```
offset  size  field
0       1     opcode
1       2     queue name length (0 = default queue, max 256)
3       4     wait_ms   (DEQUEUE only, else 0)
7       4     lease_ms  (DEQUEUE only, else 0)
11      4     payload length
15      —     queue name bytes, then payload bytes
```

| Opcode | Name | Payload |
|--------|------|---------|
| `0x01` | AUTH | token |
| `0x02` | ENQUEUE | message |
| `0x03` | DEQUEUE | — |
| `0x04` | ACK | lease id |
| `0x05` | PING | — |

## Response

Exactly one response per request, in request order.

```
offset  size  field
0       1     status
1       2     lease id length
3       4     payload length
7       —     lease id bytes, then payload bytes
```

| Status | Meaning |
|--------|---------|
| `0x00` | OK — DEQUEUE carries the message (and lease id if leased) |
| `0x01` | queue empty (after any wait elapsed) |
| `0x02` | queue full — back off and retry |
| `0x03` | payload exceeds the per-message size limit |
| `0x04` | unauthorized for this opcode's scope |
| `0x05` | bad request — payload carries the error text |
| `0x06` | lease unknown, already acked, or expired |
| `0x07` | waiter cap reached — back off and retry |
| `0x08` | server error — payload carries the error text |

## Semantics

- **Scopes** mirror HTTP: producer tokens may ENQUEUE, consumer tokens may
  DEQUEUE/ACK, api/admin tokens may do both. AUTH can be re-sent to switch
  identity.
- **wait_ms** long-polls (server-capped at 20s). A waiting DEQUEUE blocks
  every pipelined request behind it on that connection — use a dedicated
  connection for long-poll consumers.
- **lease_ms** (server-capped by `--max-lease`) delivers under a lease;
  ACK with the returned lease id before it expires or the message is
  requeued at the front.
- **Pipelining** is the throughput lever: send many requests before reading
  responses. The server batches its writes automatically while requests are
  still buffered.
- **Timeouts**: 10s to send the magic, 5 minutes idle between requests,
  then the server closes the connection. Reconnect and re-AUTH.
- Requests with a payload declared larger than the per-message cap are
  drained and answered `0x03` (stream stays usable); absurdly larger
  declarations close the connection.

## Example session

```
client: 5A 51 50 31                                        "ZQP1"
client: 02 0006 00000000 00000000 00000005 "orders" "hello"   ENQUEUE
server: 00 0000 00000000                                      OK
client: 03 0006 00001388 00007530 00000000 "orders"           DEQUEUE wait=5s lease=30s
server: 00 0010 00000005 <16-byte lease id> "hello"           OK + lease
client: 04 0006 00000000 00000000 00000010 "orders" <lease id>  ACK
server: 00 0000 00000000                                      OK
```

The Go client implements this as `client.DialTCP` — see
[client/tcp.go](../client/tcp.go) for a reference implementation.
