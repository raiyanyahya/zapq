package client

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// fakeTCPServer implements just enough of the ZQP1 protocol to unit-test
// the client side: a single in-memory queue, one lease, token "tok".
func fakeTCPServer(t *testing.T, requireAuth bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var magic [4]byte
				if _, err := io.ReadFull(conn, magic[:]); err != nil || string(magic[:]) != tcpMagic {
					return
				}
				br := bufio.NewReader(conn)
				bw := bufio.NewWriter(conn)
				var queue [][]byte
				authed := !requireAuth
				leased := ""
				reply := func(status byte, leaseID string, payload []byte) {
					var h [7]byte
					h[0] = status
					binary.BigEndian.PutUint16(h[1:3], uint16(len(leaseID)))
					binary.BigEndian.PutUint32(h[3:7], uint32(len(payload)))
					bw.Write(h[:])
					bw.WriteString(leaseID)
					bw.Write(payload)
					bw.Flush()
				}
				for {
					var hdr [15]byte
					if _, err := io.ReadFull(br, hdr[:]); err != nil {
						return
					}
					qlen := int(binary.BigEndian.Uint16(hdr[1:3]))
					leaseMs := binary.BigEndian.Uint32(hdr[7:11])
					plen := int(binary.BigEndian.Uint32(hdr[11:15]))
					name := make([]byte, qlen)
					io.ReadFull(br, name)
					payload := make([]byte, plen)
					io.ReadFull(br, payload)
					switch hdr[0] {
					case opAuth:
						if string(payload) == "tok" {
							authed = true
							reply(stOK, "", nil)
						} else {
							reply(stUnauthorized, "", nil)
						}
					case opPing:
						reply(stOK, "", nil)
					case opEnqueue:
						if !authed {
							reply(stUnauthorized, "", nil)
							break
						}
						if len(payload) > 1024 {
							reply(stTooLarge, "", nil)
							break
						}
						queue = append(queue, payload)
						reply(stOK, "", nil)
					case opDequeue:
						if len(queue) == 0 {
							reply(stEmpty, "", nil)
							break
						}
						m := queue[0]
						queue = queue[1:]
						id := ""
						if leaseMs > 0 {
							id = "lease-1"
							leased = id
						}
						reply(stOK, id, m)
					case opAck:
						if string(payload) == leased && leased != "" {
							leased = ""
							reply(stOK, "", nil)
						} else {
							reply(stNotFound, "", nil)
						}
					default:
						reply(stBadRequest, "", []byte("unknown opcode"))
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestTCPClientRoundTrip(t *testing.T) {
	addr := fakeTCPServer(t, false)
	c, err := DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := c.Enqueue("jobs", []byte("payload")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	m, ok, err := c.Dequeue("jobs", 0, 0)
	if err != nil || !ok || !bytes.Equal(m.Data, []byte("payload")) || m.LeaseID != "" {
		t.Fatalf("dequeue: %v ok=%v %+v", err, ok, m)
	}
	_, ok, err = c.Dequeue("jobs", 0, 0)
	if err != nil || ok {
		t.Fatalf("empty dequeue: ok=%v err=%v", ok, err)
	}
}

func TestTCPClientLeaseAck(t *testing.T) {
	addr := fakeTCPServer(t, false)
	c, err := DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_ = c.Enqueue("", []byte("work"))
	m, ok, err := c.Dequeue("", 0, 30*time.Second)
	if err != nil || !ok || m.LeaseID != "lease-1" {
		t.Fatalf("leased dequeue: %v ok=%v lease=%q", err, ok, m.LeaseID)
	}
	if err := c.Ack("", m.LeaseID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := c.Ack("", m.LeaseID); err != ErrNotFound {
		t.Fatalf("double ack: want ErrNotFound got %v", err)
	}
}

func TestTCPClientAuth(t *testing.T) {
	addr := fakeTCPServer(t, true)

	if _, err := DialTCP(addr, TCPAuth("wrong")); err != ErrUnauthorized {
		t.Fatalf("bad token: want ErrUnauthorized got %v", err)
	}

	c, err := DialTCP(addr, TCPAuth("tok"), TCPTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("dial with auth: %v", err)
	}
	defer c.Close()
	if err := c.Enqueue("", []byte("x")); err != nil {
		t.Fatalf("authed enqueue: %v", err)
	}

	// unauthenticated connection gets rejected per-op
	c2, err := DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.Enqueue("", []byte("x")); err != ErrUnauthorized {
		t.Fatalf("unauthed enqueue: want ErrUnauthorized got %v", err)
	}
}

func TestTCPClientErrorMapping(t *testing.T) {
	addr := fakeTCPServer(t, false)
	c, err := DialTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Enqueue("", bytes.Repeat([]byte{'x'}, 2048)); err == nil {
		t.Fatal("oversized enqueue accepted")
	}
}

func TestTCPClientDialFailure(t *testing.T) {
	if _, err := DialTCP("127.0.0.1:1"); err == nil {
		t.Fatal("expected connection error")
	}
}
