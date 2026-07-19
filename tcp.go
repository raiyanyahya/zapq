package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// ZapQ binary TCP protocol (ZQP1) — the fast path when HTTP overhead is the
// bottleneck. Full wire spec in docs/PROTOCOL.md.
//
// Connection: client sends the 4-byte magic "ZQP1", then length-prefixed
// requests; responses come back in order, so requests can be pipelined.
//
//	request:  op(u8) qlen(u16) wait_ms(u32) lease_ms(u32) plen(u32) [name] [payload]
//	response: status(u8) llen(u16) plen(u32) [lease id] [payload]
const tcpMagic = "ZQP1"

const (
	opAuth    = 0x01 // payload = token
	opEnqueue = 0x02 // payload = message
	opDequeue = 0x03 // wait_ms/lease_ms honored; response may carry lease id
	opAck     = 0x04 // payload = lease id
	opPing    = 0x05
)

const (
	stOK           = 0x00
	stEmpty        = 0x01
	stFull         = 0x02
	stTooLarge     = 0x03
	stUnauthorized = 0x04
	stBadRequest   = 0x05
	stNotFound     = 0x06
	stBusy         = 0x07 // waiter cap reached; back off and retry
	stErr          = 0x08 // payload = error text
)

const (
	tcpMagicTimeout = 10 * time.Second
	tcpIdleTimeout  = 5 * time.Minute
	tcpWriteTimeout = 30 * time.Second
	maxQueueNameLen = 256
	reqHeaderLen    = 15
)

type connScope struct {
	produce, consume, admin bool
}

type tcpServer struct {
	s      *server
	ln     net.Listener
	conns  sync.Map // net.Conn -> struct{}
	wg     sync.WaitGroup
	closed chan struct{}
}

func newTCPServer(s *server, addr string, tlsConf *tls.Config) (*tcpServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if tlsConf != nil {
		ln = tls.NewListener(ln, tlsConf)
	}
	return &tcpServer{s: s, ln: ln, closed: make(chan struct{})}, nil
}

func (t *tcpServer) serve() {
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			select {
			case <-t.closed:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			t.s.log.Error("tcp accept failed", "error", err.Error())
			return
		}
		t.conns.Store(conn, struct{}{})
		t.s.tcpConns.Add(1)
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer t.s.tcpConns.Add(-1)
			defer t.conns.Delete(conn)
			defer conn.Close()
			t.handle(conn)
		}()
	}
}

// shutdown stops accepting, waits up to grace for in-flight connections,
// then force-closes stragglers.
func (t *tcpServer) shutdown(grace time.Duration) {
	close(t.closed)
	t.ln.Close()
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		t.conns.Range(func(k, _ any) bool {
			k.(net.Conn).Close()
			return true
		})
		<-done
	}
}

func (t *tcpServer) handle(conn net.Conn) {
	s := t.s
	conn.SetReadDeadline(time.Now().Add(tcpMagicTimeout))
	var magic [4]byte
	if _, err := io.ReadFull(conn, magic[:]); err != nil || string(magic[:]) != tcpMagic {
		return
	}

	// with no tokens configured the connection has full access
	scope := connScope{
		produce: !s.tokens.enabled(),
		consume: !s.tokens.enabled(),
		admin:   !s.tokens.enabled(),
	}

	br := bufio.NewReaderSize(conn, 64<<10)
	bw := bufio.NewWriterSize(conn, 64<<10)
	var hdr [reqHeaderLen]byte

	for {
		conn.SetReadDeadline(time.Now().Add(tcpIdleTimeout))
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return
		}
		op := hdr[0]
		qlen := int(binary.BigEndian.Uint16(hdr[1:3]))
		waitMs := binary.BigEndian.Uint32(hdr[3:7])
		leaseMs := binary.BigEndian.Uint32(hdr[7:11])
		plen := int64(binary.BigEndian.Uint32(hdr[11:15]))

		if qlen > maxQueueNameLen {
			return // protocol abuse; desync unavoidable
		}
		name := ""
		if qlen > 0 {
			nb := make([]byte, qlen)
			if _, err := io.ReadFull(br, nb); err != nil {
				return
			}
			name = string(nb)
		}
		// oversized payloads within reason are drained so the stream stays
		// in sync; absurd ones mean a broken or hostile client.
		maxAccept := s.reg.maxMsgBytes
		if plen > maxAccept {
			if plen > maxAccept+(64<<20) {
				return
			}
			if _, err := io.CopyN(io.Discard, br, plen); err != nil {
				return
			}
			writeResp(bw, stTooLarge, "", nil)
			if err := flushIfIdle(br, bw, conn); err != nil {
				return
			}
			continue
		}
		var payload []byte
		if plen > 0 {
			payload = make([]byte, plen)
			if _, err := io.ReadFull(br, payload); err != nil {
				return
			}
		}

		switch op {
		case opAuth:
			tok := string(payload)
			scope = connScope{
				produce: s.tokens.allows(clsProduce, tok),
				consume: s.tokens.allows(clsConsume, tok),
				admin:   s.tokens.allows(clsAdmin, tok),
			}
			if !scope.produce && !scope.consume && !scope.admin {
				writeResp(bw, stUnauthorized, "", nil)
			} else {
				writeResp(bw, stOK, "", nil)
			}

		case opPing:
			writeResp(bw, stOK, "", nil)

		case opEnqueue:
			if !scope.produce {
				writeResp(bw, stUnauthorized, "", nil)
				break
			}
			q, err := s.reg.get(name)
			if err != nil {
				writeResp(bw, stBadRequest, "", []byte(err.Error()))
				break
			}
			switch err := q.enqueue(payload); {
			case errors.Is(err, errQueueFull):
				writeResp(bw, stFull, "", nil)
			case errors.Is(err, errTooLarge):
				writeResp(bw, stTooLarge, "", nil)
			case err != nil:
				writeResp(bw, stErr, "", []byte(err.Error()))
			default:
				q.c.enqueues.Add(1)
				writeResp(bw, stOK, "", nil)
			}

		case opDequeue:
			if !scope.consume {
				writeResp(bw, stUnauthorized, "", nil)
				break
			}
			q, err := s.reg.get(name)
			if err != nil {
				writeResp(bw, stBadRequest, "", []byte(err.Error()))
				break
			}
			wait := min(time.Duration(waitMs)*time.Millisecond, s.maxWait)
			lease := min(time.Duration(leaseMs)*time.Millisecond, s.maxLease)
			if wait > 0 {
				if s.waiters.Add(1) > s.maxWaiters {
					s.waiters.Add(-1)
					writeResp(bw, stBusy, "", nil)
					break
				}
				// pipelined responses behind this wait would stall anyway,
				// so flushing first keeps earlier replies moving
				if err := flushIfIdle(br, bw, conn); err != nil {
					s.waiters.Add(-1)
					return
				}
			}
			m, g, ok := q.takeWait(context.Background(), wait, lease)
			if wait > 0 {
				s.waiters.Add(-1)
			}
			if !ok {
				q.c.dequeuesEmpty.Add(1)
				writeResp(bw, stEmpty, "", nil)
				break
			}
			q.c.dequeues.Add(1)
			leaseID := ""
			if g != nil {
				leaseID = g.id
			}
			writeResp(bw, stOK, leaseID, m.data)

		case opAck:
			if !scope.consume {
				writeResp(bw, stUnauthorized, "", nil)
				break
			}
			q, err := s.reg.get(name)
			if err != nil {
				writeResp(bw, stBadRequest, "", []byte(err.Error()))
				break
			}
			if !q.ack(string(payload)) {
				writeResp(bw, stNotFound, "", nil)
				break
			}
			q.c.acks.Add(1)
			writeResp(bw, stOK, "", nil)

		default:
			writeResp(bw, stBadRequest, "", []byte("unknown opcode"))
		}

		if err := flushIfIdle(br, bw, conn); err != nil {
			return
		}
	}
}

// flushIfIdle flushes buffered responses only when no pipelined request is
// already waiting — batching writes is where the TCP path earns its keep.
func flushIfIdle(br *bufio.Reader, bw *bufio.Writer, conn net.Conn) error {
	if br.Buffered() > 0 {
		return nil
	}
	conn.SetWriteDeadline(time.Now().Add(tcpWriteTimeout))
	return bw.Flush()
}

func writeResp(bw *bufio.Writer, status byte, leaseID string, payload []byte) {
	var hdr [7]byte
	hdr[0] = status
	binary.BigEndian.PutUint16(hdr[1:3], uint16(len(leaseID)))
	binary.BigEndian.PutUint32(hdr[3:7], uint32(len(payload)))
	bw.Write(hdr[:])
	bw.WriteString(leaseID)
	bw.Write(payload)
}
