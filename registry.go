package main

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

const defaultQueueName = "default"

var (
	queueNameRe      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	errBadQueueName  = errors.New("invalid queue name (allowed: A-Z a-z 0-9 . _ -, max 64 chars)")
	errTooManyQueues = errors.New("queue limit reached")
)

// registry holds all named queues. Queues are created lazily on first use
// with the globally configured caps; the default queue always exists so the
// unprefixed endpoints keep working.
type registry struct {
	mu          sync.RWMutex
	queues      map[string]*queue
	maxQueues   int
	maxBytes    int64
	maxMsgBytes int64
	capacity    int
}

func newRegistry(maxBytes, maxMsgBytes int64, capacity, maxQueues int) *registry {
	r := &registry{
		queues:      make(map[string]*queue),
		maxQueues:   maxQueues,
		maxBytes:    maxBytes,
		maxMsgBytes: maxMsgBytes,
		capacity:    capacity,
	}
	r.queues[defaultQueueName] = newQueue(maxBytes, maxMsgBytes, capacity)
	return r
}

func (r *registry) newQueue() *queue {
	return newQueue(r.maxBytes, r.maxMsgBytes, r.capacity)
}

// get returns the named queue, creating it if needed. An empty name means
// the default queue.
func (r *registry) get(name string) (*queue, error) {
	if name == "" {
		name = defaultQueueName
	}
	if !queueNameRe.MatchString(name) {
		return nil, fmt.Errorf("%w: %q", errBadQueueName, name)
	}
	r.mu.RLock()
	q := r.queues[name]
	r.mu.RUnlock()
	if q != nil {
		return q, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if q := r.queues[name]; q != nil {
		return q, nil
	}
	if len(r.queues) >= r.maxQueues {
		return nil, errTooManyQueues
	}
	q = r.newQueue()
	r.queues[name] = q
	return q, nil
}

// all returns a name-sorted copy of the queue map.
func (r *registry) all() []struct {
	name string
	q    *queue
} {
	r.mu.RLock()
	out := make([]struct {
		name string
		q    *queue
	}, 0, len(r.queues))
	for name, q := range r.queues {
		out = append(out, struct {
			name string
			q    *queue
		}{name, q})
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// replace swaps in a whole new set of queues (used by snapshot load).
// Existing queues, including their leases, are dropped.
func (r *registry) replace(queues map[string]*queue) {
	if queues[defaultQueueName] == nil {
		queues[defaultQueueName] = r.newQueue()
	}
	r.mu.Lock()
	r.queues = queues
	r.mu.Unlock()
}

// expireAll requeues timed-out leases on every queue.
func (r *registry) expireAll(now time.Time) int {
	total := 0
	for _, e := range r.all() {
		total += e.q.expire(now)
	}
	return total
}
