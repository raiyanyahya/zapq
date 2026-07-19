package main

import (
	"testing"
	"time"
)

func BenchmarkEnqueue(b *testing.B) {
	q := newQueue(1<<30, 128<<10, b.N+1)
	payload := make([]byte, 128)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.enqueue(payload)
	}
}

func BenchmarkDequeue(b *testing.B) {
	q := newQueue(1<<30, 128<<10, b.N+1)
	payload := make([]byte, 128)
	for i := 0; i < b.N; i++ {
		q.enqueue(payload)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.dequeue()
	}
}

func BenchmarkEnqueueParallel(b *testing.B) {
	q := newQueue(1<<30, 128<<10, b.N+1)
	payload := make([]byte, 128)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.enqueue(payload)
		}
	})
}

func BenchmarkDequeueParallel(b *testing.B) {
	q := newQueue(1<<30, 128<<10, b.N+1)
	payload := make([]byte, 128)
	for i := 0; i < b.N; i++ {
		q.enqueue(payload)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.dequeue()
		}
	})
}

func BenchmarkEnqueueDequeue(b *testing.B) {
	q := newQueue(1<<30, 128<<10, 50000)
	payload := make([]byte, 128)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.enqueue(payload)
		q.dequeue()
	}
}

func BenchmarkEnqueueDequeueParallel(b *testing.B) {
	q := newQueue(1<<30, 128<<10, b.N+1)
	payload := make([]byte, 128)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.enqueue(payload)
			q.dequeue()
		}
	})
}

func BenchmarkEnqueueBatch100(b *testing.B) {
	q := newQueue(1<<40, 128<<10, b.N*100+100)
	batch := make([][]byte, 100)
	for i := range batch {
		batch[i] = make([]byte, 128)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.enqueueBatch(batch)
	}
}

func BenchmarkTakeLeaseAck(b *testing.B) {
	q := newQueue(1<<30, 128<<10, b.N+1)
	payload := make([]byte, 128)
	for i := 0; i < b.N; i++ {
		q.enqueue(payload)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, g, _ := q.take(time.Minute)
		q.ack(g.id)
	}
}

func BenchmarkMetricsRead(b *testing.B) {
	q := newQueue(1<<30, 128<<10, 50000)
	payload := make([]byte, 128)
	for i := 0; i < 10000; i++ {
		q.enqueue(payload)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.length()
			q.size()
		}
	})
}
