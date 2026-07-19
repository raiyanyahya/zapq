package client

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// TCP speaks the zapq binary protocol (ZQP1) — the low-latency alternative
// to HTTP when the queue sits on your hot path. One request/response per
// call; the connection is mutex-serialized so a *TCP is safe for concurrent
// use, though a connection per goroutine pipelines better.
//
//	t, _ := client.DialTCP("localhost:6789", client.TCPAuth(token))
//	_ = t.Enqueue("orders", []byte("job-1"))
//	msg, ok, _ := t.Dequeue("orders", 5*time.Second, 30*time.Second)
//	if ok { _ = t.Ack("orders", msg.LeaseID) }
type TCP struct {
	mu      sync.Mutex
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	timeout time.Duration
}

var (
	ErrNotFound = errors.New("lease unknown, already acked, or expired")
	ErrBusy     = errors.New("server busy: waiter cap reached")
)

const (
	tcpMagic  = "ZQP1"
	opAuth    = 0x01
	opEnqueue = 0x02
	opDequeue = 0x03
	opAck     = 0x04
	opPing    = 0x05

	stOK           = 0x00
	stEmpty        = 0x01
	stFull         = 0x02
	stTooLarge     = 0x03
	stUnauthorized = 0x04
	stBadRequest   = 0x05
	stNotFound     = 0x06
	stBusy         = 0x07
)

type TCPOption func(*tcpDialConfig)

type tcpDialConfig struct {
	token   string
	tlsConf *tls.Config
	timeout time.Duration
}

// TCPAuth authenticates the connection right after dialing.
func TCPAuth(token string) TCPOption { return func(c *tcpDialConfig) { c.token = token } }

// TCPTLS dials with TLS (use when the server runs --tls-cert/--tls-key).
func TCPTLS(conf *tls.Config) TCPOption { return func(c *tcpDialConfig) { c.tlsConf = conf } }

// TCPTimeout bounds each call's network I/O (default 30s; long-poll waits
// extend it automatically).
func TCPTimeout(d time.Duration) TCPOption { return func(c *tcpDialConfig) { c.timeout = d } }

func DialTCP(addr string, opts ...TCPOption) (*TCP, error) {
	cfg := tcpDialConfig{timeout: 30 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	var conn net.Conn
	var err error
	if cfg.tlsConf != nil {
		conn, err = tls.Dial("tcp", addr, cfg.tlsConf)
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	t := &TCP{
		conn:    conn,
		br:      bufio.NewReaderSize(conn, 64<<10),
		bw:      bufio.NewWriterSize(conn, 64<<10),
		timeout: cfg.timeout,
	}
	if _, err := conn.Write([]byte(tcpMagic)); err != nil {
		conn.Close()
		return nil, err
	}
	if cfg.token != "" {
		if err := t.Auth(cfg.token); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return t, nil
}

func (t *TCP) Close() error { return t.conn.Close() }

// roundTrip sends one request and reads one response under the lock.
func (t *TCP) roundTrip(op byte, queue string, waitMs, leaseMs uint32, payload []byte, extraWait time.Duration) (status byte, leaseID string, resp []byte, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.conn.SetDeadline(time.Now().Add(t.timeout + extraWait))
	var hdr [15]byte
	hdr[0] = op
	binary.BigEndian.PutUint16(hdr[1:3], uint16(len(queue)))
	binary.BigEndian.PutUint32(hdr[3:7], waitMs)
	binary.BigEndian.PutUint32(hdr[7:11], leaseMs)
	binary.BigEndian.PutUint32(hdr[11:15], uint32(len(payload)))
	t.bw.Write(hdr[:])
	t.bw.WriteString(queue)
	t.bw.Write(payload)
	if err := t.bw.Flush(); err != nil {
		return 0, "", nil, err
	}

	var rhdr [7]byte
	if _, err := io.ReadFull(t.br, rhdr[:]); err != nil {
		return 0, "", nil, err
	}
	status = rhdr[0]
	llen := int(binary.BigEndian.Uint16(rhdr[1:3]))
	plen := int(binary.BigEndian.Uint32(rhdr[3:7]))
	if llen > 0 {
		lb := make([]byte, llen)
		if _, err := io.ReadFull(t.br, lb); err != nil {
			return 0, "", nil, err
		}
		leaseID = string(lb)
	}
	if plen > 0 {
		resp = make([]byte, plen)
		if _, err := io.ReadFull(t.br, resp); err != nil {
			return 0, "", nil, err
		}
	}
	return status, leaseID, resp, nil
}

func statusErr(status byte, resp []byte) error {
	switch status {
	case stOK, stEmpty:
		return nil
	case stFull:
		return ErrQueueFull
	case stTooLarge:
		return errors.New("payload exceeds per-message size limit")
	case stUnauthorized:
		return ErrUnauthorized
	case stNotFound:
		return ErrNotFound
	case stBusy:
		return ErrBusy
	default:
		return fmt.Errorf("zapq: %s", string(resp))
	}
}

// Auth (re)authenticates the connection.
func (t *TCP) Auth(token string) error {
	status, _, resp, err := t.roundTrip(opAuth, "", 0, 0, []byte(token), 0)
	if err != nil {
		return err
	}
	return statusErr(status, resp)
}

// Ping checks the connection is alive.
func (t *TCP) Ping() error {
	status, _, resp, err := t.roundTrip(opPing, "", 0, 0, nil, 0)
	if err != nil {
		return err
	}
	return statusErr(status, resp)
}

// Enqueue pushes one message. Unlike the HTTP client it does not retry;
// wrap it yourself if you want that.
func (t *TCP) Enqueue(queue string, payload []byte) error {
	status, _, resp, err := t.roundTrip(opEnqueue, queue, 0, 0, payload, 0)
	if err != nil {
		return err
	}
	return statusErr(status, resp)
}

// Dequeue pops the next message, long-polling up to wait. lease > 0
// delivers under a lease (ack via Ack). ok is false when the queue stayed
// empty.
func (t *TCP) Dequeue(queue string, wait, lease time.Duration) (*Message, bool, error) {
	status, leaseID, resp, err := t.roundTrip(opDequeue, queue,
		uint32(wait.Milliseconds()), uint32(lease.Milliseconds()), nil, wait)
	if err != nil {
		return nil, false, err
	}
	switch status {
	case stEmpty:
		return nil, false, nil
	case stOK:
		return &Message{Data: resp, LeaseID: leaseID}, true, nil
	default:
		return nil, false, statusErr(status, resp)
	}
}

// Ack settles a leased message.
func (t *TCP) Ack(queue, leaseID string) error {
	status, _, resp, err := t.roundTrip(opAck, queue, 0, 0, []byte(leaseID), 0)
	if err != nil {
		return err
	}
	return statusErr(status, resp)
}
