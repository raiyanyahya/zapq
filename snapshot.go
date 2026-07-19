package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"time"
)

// Snapshot format v2: a binary registry-wide dump with a trailing CRC32.
//
//	magic   "ZQSNAP2\x00"                     8 bytes
//	u32     queue count
//	per queue:
//	  u16   name length, name bytes
//	  u32   message count
//	  per message:
//	    u64 enqueue time (unix nanos)
//	    u32 payload length, payload bytes
//	u32     CRC32 (IEEE) of everything above
//
// Legacy v1 snapshots (a JSON array of base64 payloads) are still readable
// and load into the default queue.
var snapMagic = []byte("ZQSNAP2\x00")

var errSnapCorrupt = errors.New("snapshot corrupt")

// --------------------------- encoding ----------------------------------------

type snapWriter struct {
	w   io.Writer
	err error
}

func (s *snapWriter) bytes(b []byte) {
	if s.err == nil {
		_, s.err = s.w.Write(b)
	}
}

func (s *snapWriter) u16(v uint16) { var b [2]byte; binary.BigEndian.PutUint16(b[:], v); s.bytes(b[:]) }
func (s *snapWriter) u32(v uint32) { var b [4]byte; binary.BigEndian.PutUint32(b[:], v); s.bytes(b[:]) }
func (s *snapWriter) u64(v uint64) { var b [8]byte; binary.BigEndian.PutUint64(b[:], v); s.bytes(b[:]) }

// persist writes every queue — unacked leased messages included — to path
// atomically: encode to a temp file, fsync, then rename. Queues keep
// serving during the write.
func (r *registry) persist(path string) error {
	type qsnap struct {
		name string
		msgs []message
	}
	entries := r.all()
	snaps := make([]qsnap, 0, len(entries))
	for _, e := range entries {
		snaps = append(snaps, qsnap{e.name, e.q.exportMessages()})
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}

	bw := bufio.NewWriter(f)
	h := crc32.NewIEEE()
	sw := &snapWriter{w: io.MultiWriter(bw, h)}
	sw.bytes(snapMagic)
	sw.u32(uint32(len(snaps)))
	for _, qs := range snaps {
		sw.u16(uint16(len(qs.name)))
		sw.bytes([]byte(qs.name))
		sw.u32(uint32(len(qs.msgs)))
		for _, m := range qs.msgs {
			sw.u64(uint64(m.enq.UnixNano()))
			sw.u32(uint32(len(m.data)))
			sw.bytes(m.data)
		}
	}
	if sw.err != nil {
		return fail(sw.err)
	}
	// CRC goes to the file only, not into its own hash.
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], h.Sum32())
	if _, err := bw.Write(crc[:]); err != nil {
		return fail(err)
	}
	if err := bw.Flush(); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// --------------------------- decoding ----------------------------------------

type snapReader struct {
	b   []byte
	off int
}

func (s *snapReader) take(n int) ([]byte, error) {
	if n < 0 || s.off+n > len(s.b) {
		return nil, errSnapCorrupt
	}
	b := s.b[s.off : s.off+n]
	s.off += n
	return b, nil
}

func (s *snapReader) u16() (uint16, error) {
	b, err := s.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (s *snapReader) u32() (uint32, error) {
	b, err := s.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (s *snapReader) u64() (uint64, error) {
	b, err := s.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

func decodeSnapshotV2(data []byte) (map[string][]message, error) {
	if len(data) < len(snapMagic)+8 {
		return nil, errSnapCorrupt
	}
	body, tail := data[:len(data)-4], data[len(data)-4:]
	if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(tail) {
		return nil, fmt.Errorf("%w: checksum mismatch", errSnapCorrupt)
	}
	s := &snapReader{b: body, off: len(snapMagic)}
	now := time.Now()

	nq, err := s.u32()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]message)
	for i := uint32(0); i < nq; i++ {
		nameLen, err := s.u16()
		if err != nil {
			return nil, err
		}
		nameB, err := s.take(int(nameLen))
		if err != nil {
			return nil, err
		}
		name := string(nameB)
		nm, err := s.u32()
		if err != nil {
			return nil, err
		}
		// each message needs at least its 12 header bytes
		if int64(nm)*12 > int64(len(body)-s.off) {
			return nil, errSnapCorrupt
		}
		msgs := make([]message, 0, nm)
		for j := uint32(0); j < nm; j++ {
			ts, err := s.u64()
			if err != nil {
				return nil, err
			}
			dl, err := s.u32()
			if err != nil {
				return nil, err
			}
			d, err := s.take(int(dl))
			if err != nil {
				return nil, err
			}
			enq := time.Unix(0, int64(ts))
			if enq.After(now) || enq.IsZero() {
				enq = now
			}
			msgs = append(msgs, message{data: append([]byte(nil), d...), enq: enq})
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%w: duplicate queue %q", errSnapCorrupt, name)
		}
		out[name] = msgs
	}
	if s.off != len(body) {
		return nil, fmt.Errorf("%w: trailing bytes", errSnapCorrupt)
	}
	return out, nil
}

func decodeLegacyJSON(data []byte) (map[string][]message, error) {
	var d [][]byte
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	now := time.Now()
	msgs := make([]message, 0, len(d))
	for _, b := range d {
		msgs = append(msgs, message{data: b, enq: now})
	}
	return map[string][]message{defaultQueueName: msgs}, nil
}

// load replaces the entire registry with the snapshot's contents after
// validating every queue against the configured caps. Queues not present
// in the snapshot are dropped, as are all outstanding leases.
func (r *registry) load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var imported map[string][]message
	if bytes.HasPrefix(data, snapMagic) {
		imported, err = decodeSnapshotV2(data)
	} else {
		imported, err = decodeLegacyJSON(data)
	}
	if err != nil {
		return err
	}
	if len(imported) > r.maxQueues {
		return fmt.Errorf("snapshot has %d queues, limit is %d", len(imported), r.maxQueues)
	}

	fresh := make(map[string]*queue, len(imported))
	for name, msgs := range imported {
		if !queueNameRe.MatchString(name) {
			return fmt.Errorf("%w in snapshot: %q", errBadQueueName, name)
		}
		q := r.newQueue()
		if err := q.importMessages(msgs); err != nil {
			return fmt.Errorf("queue %q: %w", name, err)
		}
		fresh[name] = q
	}
	r.replace(fresh)
	return nil
}
