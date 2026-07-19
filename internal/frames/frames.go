// Package frames implements the zapq batch wire format: each message is a
// 4-byte big-endian length prefix followed by the raw payload, repeated
// until EOF. It is shared by the server and the Go client.
package frames

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
)

// ContentType identifies a request or response body in frame format.
const ContentType = "application/vnd.zapq.frames"

const (
	// MaxMessages caps how many messages one batch may carry.
	MaxMessages = 1000
	// MaxTotalBytes caps the total payload bytes in one batch.
	MaxTotalBytes = 16 << 20
)

var (
	ErrMalformed       = errors.New("malformed frame data")
	ErrEmpty           = errors.New("batch contains no messages")
	ErrTooManyMessages = errors.New("batch exceeds message count limit")
	ErrTotalTooLarge   = errors.New("batch exceeds total byte limit")
	ErrMessageTooLarge = errors.New("frame exceeds per-message size limit")
)

// Read decodes all frames from r, enforcing the batch limits and the given
// per-message size cap.
func Read(r io.Reader, maxMsgBytes int64) ([][]byte, error) {
	br := bufio.NewReader(r)
	var out [][]byte
	var total int64
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				break
			}
			return nil, ErrMalformed
		}
		n := int64(binary.BigEndian.Uint32(hdr[:]))
		if n > maxMsgBytes {
			return nil, ErrMessageTooLarge
		}
		if len(out) >= MaxMessages {
			return nil, ErrTooManyMessages
		}
		total += n
		if total > MaxTotalBytes {
			return nil, ErrTotalTooLarge
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(br, b); err != nil {
			return nil, ErrMalformed
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	return out, nil
}

// Write encodes msgs as frames onto w.
func Write(w io.Writer, msgs [][]byte) error {
	bw := bufio.NewWriter(w)
	var hdr [4]byte
	for _, m := range msgs {
		binary.BigEndian.PutUint32(hdr[:], uint32(len(m)))
		if _, err := bw.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := bw.Write(m); err != nil {
			return err
		}
	}
	return bw.Flush()
}
