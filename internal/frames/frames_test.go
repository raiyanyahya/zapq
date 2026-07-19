package frames

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := [][]byte{[]byte("one"), {}, []byte("three"), bytes.Repeat([]byte{0xff}, 1024)}
	var buf bytes.Buffer
	if err := Write(&buf, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := Read(&buf, 1<<20)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("count: got %d want %d", len(out), len(in))
	}
	for i := range in {
		if !bytes.Equal(in[i], out[i]) {
			t.Fatalf("frame %d mismatch", i)
		}
	}
}

func TestReadErrors(t *testing.T) {
	if _, err := Read(bytes.NewReader(nil), 1<<20); err != ErrEmpty {
		t.Fatalf("empty: got %v", err)
	}
	// truncated header
	if _, err := Read(bytes.NewReader([]byte{0, 0}), 1<<20); err != ErrMalformed {
		t.Fatalf("short header: got %v", err)
	}
	// header promises more bytes than present
	if _, err := Read(bytes.NewReader([]byte{0, 0, 0, 9, 'x'}), 1<<20); err != ErrMalformed {
		t.Fatalf("truncated body: got %v", err)
	}
	// over per-message cap
	var buf bytes.Buffer
	_ = Write(&buf, [][]byte{bytes.Repeat([]byte{'a'}, 100)})
	if _, err := Read(&buf, 10); err != ErrMessageTooLarge {
		t.Fatalf("oversize: got %v", err)
	}
}

func FuzzFrames(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 'x'})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{255, 255, 255, 255})
	f.Fuzz(func(t *testing.T, data []byte) {
		msgs, err := Read(bytes.NewReader(data), 1<<16)
		if err != nil {
			return
		}
		// anything that decodes must re-encode to the identical bytes
		var buf bytes.Buffer
		if err := Write(&buf, msgs); err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(buf.Bytes(), data) {
			t.Fatalf("round trip mismatch")
		}
	})
}
