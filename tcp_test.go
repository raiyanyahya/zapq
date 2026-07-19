package main

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/raiyanyahya/zapq/client"
)

func startTCPServer(t *testing.T, s *server) string {
	t.Helper()
	ts, err := newTCPServer(s, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ts.serve()
	t.Cleanup(func() { ts.shutdown(time.Second) })
	return ts.ln.Addr().String()
}

func newTCPTestServer(t *testing.T) (*server, string) {
	t.Helper()
	s := &server{
		reg:        newRegistry(256<<20, testMaxMsgBytes, 50000, 8),
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		startTime:  time.Now(),
		maxWait:    2 * time.Second,
		maxLease:   time.Minute,
		maxWaiters: 1024,
	}
	return s, startTCPServer(t, s)
}

func TestTCPEnqueueDequeue(t *testing.T) {
	_, addr := newTCPTestServer(t)
	c, err := client.DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := c.Enqueue("orders", []byte("job-1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	m, ok, err := c.Dequeue("orders", 0, 0)
	if err != nil || !ok || string(m.Data) != "job-1" {
		t.Fatalf("dequeue: %v ok=%v data=%q", err, ok, m)
	}
	if m.LeaseID != "" {
		t.Fatal("unexpected lease id without lease")
	}
	_, ok, err = c.Dequeue("orders", 0, 0)
	if err != nil || ok {
		t.Fatalf("empty dequeue: ok=%v err=%v", ok, err)
	}
}

func TestTCPLeaseAck(t *testing.T) {
	s, addr := newTCPTestServer(t)
	c, err := client.DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_ = c.Enqueue("", []byte("work"))
	m, ok, err := c.Dequeue("", 0, 30*time.Second)
	if err != nil || !ok || m.LeaseID == "" {
		t.Fatalf("leased dequeue: %v ok=%v lease=%q", err, ok, m.LeaseID)
	}
	q := defaultQ(t, s)
	if q.stats(time.Now()).leased != 1 {
		t.Fatal("lease not registered")
	}
	if err := c.Ack("", m.LeaseID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := c.Ack("", m.LeaseID); err != client.ErrNotFound {
		t.Fatalf("double ack: want ErrNotFound got %v", err)
	}
}

func TestTCPLongPoll(t *testing.T) {
	_, addr := newTCPTestServer(t)
	consumer, err := client.DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	producer, err := client.DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()

	done := make(chan string, 1)
	go func() {
		m, ok, err := consumer.Dequeue("", 2*time.Second, 0)
		if err != nil || !ok {
			done <- ""
			return
		}
		done <- string(m.Data)
	}()
	time.Sleep(50 * time.Millisecond)
	if err := producer.Enqueue("", []byte("late")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != "late" {
			t.Fatalf("long poll got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tcp long poll never returned")
	}
}

func TestTCPAuthScopes(t *testing.T) {
	s, addr := newTCPTestServer(t)
	s.tokens = tokens{producer: "prod", consumer: "cons"}

	// unauthenticated connection can do nothing
	c, err := client.DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Enqueue("", []byte("x")); err != client.ErrUnauthorized {
		t.Fatalf("unauthenticated enqueue: want ErrUnauthorized got %v", err)
	}

	// wrong token rejected at dial
	if _, err := client.DialTCP(addr, client.TCPAuth("nope")); err != client.ErrUnauthorized {
		t.Fatalf("bad token dial: want ErrUnauthorized got %v", err)
	}

	// producer can enqueue but not dequeue
	p, err := client.DialTCP(addr, client.TCPAuth("prod"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Enqueue("", []byte("x")); err != nil {
		t.Fatalf("producer enqueue: %v", err)
	}
	if _, _, err := p.Dequeue("", 0, 0); err != client.ErrUnauthorized {
		t.Fatalf("producer dequeue: want ErrUnauthorized got %v", err)
	}

	// consumer the other way round
	cons, err := client.DialTCP(addr, client.TCPAuth("cons"))
	if err != nil {
		t.Fatal(err)
	}
	defer cons.Close()
	if m, ok, err := cons.Dequeue("", 0, 0); err != nil || !ok || string(m.Data) != "x" {
		t.Fatalf("consumer dequeue: %v ok=%v", err, ok)
	}
	if err := cons.Enqueue("", []byte("y")); err != client.ErrUnauthorized {
		t.Fatalf("consumer enqueue: want ErrUnauthorized got %v", err)
	}
}

func TestTCPQueueLimitsApply(t *testing.T) {
	s, addr := newTCPTestServer(t)
	s.reg = newRegistry(256<<20, testMaxMsgBytes, 1, 8)
	c, err := client.DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Enqueue("", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := c.Enqueue("", []byte("b")); err != client.ErrQueueFull {
		t.Fatalf("want ErrQueueFull got %v", err)
	}
	if err := c.Enqueue("", make([]byte, testMaxMsgBytes+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
	// connection still usable after both rejections
	if err := c.Ping(); err != nil {
		t.Fatalf("ping after errors: %v", err)
	}
}

func TestTCPBadMagicRejected(t *testing.T) {
	_, addr := newTCPTestServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("HTTP")) // wrong magic
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("want EOF after bad magic, got %v", err)
	}
}

// TestTCPPipelining sends many requests before reading any responses and
// checks they all come back in order — the property the throughput numbers
// depend on.
func TestTCPPipelining(t *testing.T) {
	_, addr := newTCPTestServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte(tcpMagic))

	const n = 500
	req := make([]byte, 0, n*(15+1))
	for i := 0; i < n; i++ {
		var hdr [15]byte
		hdr[0] = opEnqueue
		binary.BigEndian.PutUint32(hdr[11:15], 1)
		req = append(req, hdr[:]...)
		req = append(req, byte(i))
	}
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var rhdr [7]byte
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(conn, rhdr[:]); err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		if rhdr[0] != stOK {
			t.Fatalf("response %d: status %d", i, rhdr[0])
		}
	}

	// now drain with pipelined dequeues and verify FIFO order survived
	req = req[:0]
	var hdr [15]byte
	hdr[0] = opDequeue
	for i := 0; i < n; i++ {
		req = append(req, hdr[:]...)
	}
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(conn, rhdr[:]); err != nil {
			t.Fatalf("dequeue response %d: %v", i, err)
		}
		plen := binary.BigEndian.Uint32(rhdr[3:7])
		body := make([]byte, plen)
		if _, err := io.ReadFull(conn, body); err != nil {
			t.Fatal(err)
		}
		if rhdr[0] != stOK || plen != 1 || body[0] != byte(i) {
			t.Fatalf("dequeue %d: status=%d len=%d body=%v", i, rhdr[0], plen, body)
		}
	}
}

func TestTCPConnGauge(t *testing.T) {
	s, addr := newTCPTestServer(t)
	c, err := client.DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Ping()
	if s.tcpConns.Load() != 1 {
		t.Fatalf("tcp conn gauge = %d, want 1", s.tcpConns.Load())
	}
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for s.tcpConns.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("tcp conn gauge stuck at %d", s.tcpConns.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
