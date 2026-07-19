package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errTooLarge  = errors.New("payload exceeds per-message size limit")
	errQueueFull = errors.New("queue full")
)

// counters are per-queue statistics, updated with atomics outside the lock.
type counters struct {
	enqueues      atomic.Uint64
	dequeues      atomic.Uint64
	dequeuesEmpty atomic.Uint64
	acks          atomic.Uint64
	expirations   atomic.Uint64
}

// message is a queued payload plus the time it was accepted, which feeds
// the head-age metric and is preserved across snapshots.
type message struct {
	data []byte
	enq  time.Time
}

// lease tracks a message handed to a consumer that has not been acked yet.
// Leased messages still count against the queue's capacity and byte caps so
// an expiry can always requeue them.
type lease struct {
	msg     message
	expires time.Time
}

// leaseGrant is what a consumer gets back alongside a leased message.
type leaseGrant struct {
	id      string
	expires time.Time
}

// queue is a fixed-capacity FIFO ring buffer of messages, safe for
// concurrent use. Payloads are copied on enqueue and never mutated after,
// so snapshots only need a read lock.
type queue struct {
	mu          sync.RWMutex
	buf         []message
	head        int
	tail        int
	count       int
	bytes       int64
	leased      map[string]lease
	leasedBytes int64
	maxBytes    int64
	maxMsgBytes int64
	c           counters

	// notify wakes long-poll consumers. Buffered with capacity 1 so
	// producers never block; waiters re-check the queue on every wakeup.
	notify chan struct{}
}

func newQueue(maxBytes, maxMsgBytes int64, capacity int) *queue {
	return &queue{
		buf:         make([]message, capacity),
		leased:      make(map[string]lease),
		maxBytes:    maxBytes,
		maxMsgBytes: maxMsgBytes,
		notify:      make(chan struct{}, 1),
	}
}

func (q *queue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// roomLocked reports whether n more messages totalling extra bytes fit,
// counting leased messages against both caps.
func (q *queue) roomLocked(n int, extra int64) bool {
	return q.count+len(q.leased)+n <= len(q.buf) &&
		q.bytes+q.leasedBytes+extra <= q.maxBytes
}

func (q *queue) enqueue(b []byte) error {
	if int64(len(b)) > q.maxMsgBytes {
		return errTooLarge
	}
	q.mu.Lock()
	if !q.roomLocked(1, int64(len(b))) {
		q.mu.Unlock()
		return errQueueFull
	}
	cp := append([]byte(nil), b...)
	q.buf[q.tail] = message{data: cp, enq: time.Now()}
	q.tail = (q.tail + 1) % len(q.buf)
	q.count++
	q.bytes += int64(len(cp))
	q.mu.Unlock()
	q.signal()
	return nil
}

// enqueueBatch inserts all messages under one lock acquisition, or none of
// them: a batch that doesn't fit is rejected whole so producers never have
// to guess which half was accepted.
func (q *queue) enqueueBatch(bs [][]byte) error {
	var total int64
	for _, b := range bs {
		if int64(len(b)) > q.maxMsgBytes {
			return errTooLarge
		}
		total += int64(len(b))
	}
	now := time.Now()
	q.mu.Lock()
	if !q.roomLocked(len(bs), total) {
		q.mu.Unlock()
		return errQueueFull
	}
	for _, b := range bs {
		cp := append([]byte(nil), b...)
		q.buf[q.tail] = message{data: cp, enq: now}
		q.tail = (q.tail + 1) % len(q.buf)
		q.count++
		q.bytes += int64(len(cp))
	}
	q.mu.Unlock()
	q.signal()
	return nil
}

func (q *queue) popLocked() message {
	m := q.buf[q.head]
	q.buf[q.head] = message{}
	q.head = (q.head + 1) % len(q.buf)
	q.count--
	q.bytes -= int64(len(m.data))
	return m
}

func (q *queue) newLeaseIDLocked() string {
	for {
		id := randHex(8)
		if _, exists := q.leased[id]; !exists {
			return id
		}
	}
}

func (q *queue) grantLocked(m message, d time.Duration) *leaseGrant {
	g := &leaseGrant{id: q.newLeaseIDLocked(), expires: time.Now().Add(d)}
	q.leased[g.id] = lease{msg: m, expires: g.expires}
	q.leasedBytes += int64(len(m.data))
	return g
}

// take pops the next message. With leaseDur > 0 the message is held on a
// lease instead of being gone for good: the consumer must ack it before the
// lease expires or it is requeued at the front.
func (q *queue) take(leaseDur time.Duration) (message, *leaseGrant, bool) {
	q.mu.Lock()
	if q.count == 0 {
		q.mu.Unlock()
		return message{}, nil, false
	}
	m := q.popLocked()
	var g *leaseGrant
	if leaseDur > 0 {
		g = q.grantLocked(m, leaseDur)
	}
	q.mu.Unlock()
	return m, g, true
}

// takeBatch pops up to max messages under one lock acquisition.
func (q *queue) takeBatch(max int, leaseDur time.Duration) ([]message, []string) {
	q.mu.Lock()
	n := min(max, q.count)
	if n == 0 {
		q.mu.Unlock()
		return nil, nil
	}
	msgs := make([]message, 0, n)
	var ids []string
	for i := 0; i < n; i++ {
		m := q.popLocked()
		if leaseDur > 0 {
			ids = append(ids, q.grantLocked(m, leaseDur).id)
		}
		msgs = append(msgs, m)
	}
	q.mu.Unlock()
	return msgs, ids
}

// takeWait blocks up to wait for a message. It returns early if ctx is
// cancelled (e.g. the client disconnected).
func (q *queue) takeWait(ctx context.Context, wait, leaseDur time.Duration) (message, *leaseGrant, bool) {
	if m, g, ok := q.take(leaseDur); ok {
		return m, g, true
	}
	if wait <= 0 {
		return message{}, nil, false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-q.notify:
			if m, g, ok := q.take(leaseDur); ok {
				// The notify buffer holds one token, so pass the wakeup
				// along if messages remain and other waiters are parked.
				if q.length() > 0 {
					q.signal()
				}
				return m, g, true
			}
		case <-timer.C:
			return message{}, nil, false
		case <-ctx.Done():
			return message{}, nil, false
		}
	}
}

// takeBatchWait is takeBatch with long-poll semantics: it waits up to wait
// for the first message, then grabs whatever is available up to max.
func (q *queue) takeBatchWait(ctx context.Context, wait time.Duration, max int, leaseDur time.Duration) ([]message, []string) {
	if msgs, ids := q.takeBatch(max, leaseDur); len(msgs) > 0 {
		return msgs, ids
	}
	if wait <= 0 {
		return nil, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-q.notify:
			if msgs, ids := q.takeBatch(max, leaseDur); len(msgs) > 0 {
				if q.length() > 0 {
					q.signal()
				}
				return msgs, ids
			}
		case <-timer.C:
			return nil, nil
		case <-ctx.Done():
			return nil, nil
		}
	}
}

// dequeue is the plain no-lease pop, kept for tests and benchmarks.
func (q *queue) dequeue() ([]byte, bool) {
	m, _, ok := q.take(0)
	return m.data, ok
}

// ack settles a lease; the message is gone for good. Returns false if the
// lease is unknown, already acked, or already expired and requeued.
func (q *queue) ack(id string) bool {
	q.mu.Lock()
	l, ok := q.leased[id]
	if ok {
		delete(q.leased, id)
		q.leasedBytes -= int64(len(l.msg.data))
	}
	q.mu.Unlock()
	return ok
}

// expire requeues every lease past its deadline at the front of the queue
// and returns how many it requeued. Space is guaranteed because leased
// messages never stopped counting against the caps.
func (q *queue) expire(now time.Time) int {
	q.mu.Lock()
	n := 0
	for id, l := range q.leased {
		if now.After(l.expires) {
			delete(q.leased, id)
			q.leasedBytes -= int64(len(l.msg.data))
			q.head = (q.head - 1 + len(q.buf)) % len(q.buf)
			q.buf[q.head] = l.msg
			q.count++
			q.bytes += int64(len(l.msg.data))
			n++
		}
	}
	q.mu.Unlock()
	if n > 0 {
		q.c.expirations.Add(uint64(n))
		q.signal()
	}
	return n
}

func (q *queue) clear() int {
	q.mu.Lock()
	dropped := q.count + len(q.leased)
	for i := range q.buf {
		q.buf[i] = message{}
	}
	q.head, q.tail, q.count, q.bytes = 0, 0, 0, 0
	q.leased = make(map[string]lease)
	q.leasedBytes = 0
	q.mu.Unlock()
	return dropped
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

type queueStats struct {
	length      int
	bytes       int64
	leased      int
	leasedBytes int64
	headAgeSec  float64
}

func (q *queue) stats(now time.Time) queueStats {
	q.mu.RLock()
	st := queueStats{
		length:      q.count,
		bytes:       q.bytes,
		leased:      len(q.leased),
		leasedBytes: q.leasedBytes,
	}
	if q.count > 0 {
		st.headAgeSec = now.Sub(q.buf[q.head].enq).Seconds()
	}
	q.mu.RUnlock()
	return st
}

// exportMessages returns leased (unacked) messages first, then visible ones
// in order, so a snapshot restore re-delivers unfinished work.
func (q *queue) exportMessages() []message {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make([]message, 0, q.count+len(q.leased))
	for _, l := range q.leased {
		out = append(out, l.msg)
	}
	for i := 0; i < q.count; i++ {
		out = append(out, q.buf[(q.head+i)%len(q.buf)])
	}
	return out
}

// importMessages replaces the queue contents (and drops any leases) after
// validating the messages against the queue's caps.
func (q *queue) importMessages(msgs []message) error {
	var total int64
	for i, m := range msgs {
		if int64(len(m.data)) > q.maxMsgBytes {
			return fmt.Errorf("message %d is %d bytes, over the %d byte per-message limit", i, len(m.data), q.maxMsgBytes)
		}
		total += int64(len(m.data))
	}

	q.mu.Lock()
	if len(msgs) > len(q.buf) {
		q.mu.Unlock()
		return fmt.Errorf("snapshot has %d messages, queue capacity is %d", len(msgs), len(q.buf))
	}
	if total > q.maxBytes {
		q.mu.Unlock()
		return fmt.Errorf("snapshot is %d bytes, queue byte cap is %d", total, q.maxBytes)
	}
	for i := range q.buf {
		q.buf[i] = message{}
	}
	copy(q.buf, msgs)
	q.head = 0
	q.tail = len(msgs) % len(q.buf)
	q.count = len(msgs)
	q.bytes = total
	q.leased = make(map[string]lease)
	q.leasedBytes = 0
	q.mu.Unlock()

	if len(msgs) > 0 {
		q.signal()
	}
	return nil
}
