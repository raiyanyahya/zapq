package main

import (
	"testing"
)

func BenchmarkEnqueue(b *testing.B) {
	q := newQueue(1<<30, b.N+1)
	c := &counters{}
	payload := make([]byte, 128)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.enqueue(payload, c)
	}
}

func BenchmarkDequeue(b *testing.B) {
	q := newQueue(1<<30, b.N+1)
	c := &counters{}
	payload := make([]byte, 128)
	for i := 0; i < b.N; i++ {
		q.enqueue(payload, c)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.dequeue(c)
	}
}

func BenchmarkEnqueueParallel(b *testing.B) {
	q := newQueue(1<<30, b.N+1)
	c := &counters{}
	payload := make([]byte, 128)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.enqueue(payload, c)
		}
	})
}

func BenchmarkDequeueParallel(b *testing.B) {
	q := newQueue(1<<30, b.N+1)
	c := &counters{}
	payload := make([]byte, 128)
	for i := 0; i < b.N; i++ {
		q.enqueue(payload, c)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.dequeue(c)
		}
	})
}

func BenchmarkEnqueueDequeue(b *testing.B) {
	q := newQueue(1<<30, 50000)
	c := &counters{}
	payload := make([]byte, 128)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.enqueue(payload, c)
		q.dequeue(c)
	}
}

func BenchmarkEnqueueDequeueParallel(b *testing.B) {
	q := newQueue(1<<30, b.N+1)
	c := &counters{}
	payload := make([]byte, 128)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.enqueue(payload, c)
			q.dequeue(c)
		}
	})
}

func BenchmarkMetricsRead(b *testing.B) {
	q := newQueue(1<<30, 50000)
	c := &counters{}
	payload := make([]byte, 128)
	for i := 0; i < 10000; i++ {
		q.enqueue(payload, c)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.length()
			q.size()
		}
	})
}
