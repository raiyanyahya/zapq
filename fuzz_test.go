package main

import (
	"os"
	"path/filepath"
	"testing"
)

func FuzzParseSize(f *testing.F) {
	for _, seed := range []string{"100", "1M", "2G", "64K", "-1", "abc", " 4M ", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := parseSize(s)
		if err == nil && n < 0 {
			t.Fatalf("parseSize(%q) returned negative %d without error", s, n)
		}
	})
}

func FuzzSnapshotLoad(f *testing.F) {
	// seed with a valid v2 snapshot and a legacy JSON one
	r := newRegistry(1<<20, 1<<10, 64, 4)
	q, _ := r.get("")
	_ = q.enqueue([]byte("seed"))
	dir := f.TempDir()
	valid := filepath.Join(dir, "valid.bin")
	if err := r.persist(valid); err != nil {
		f.Fatal(err)
	}
	data, _ := os.ReadFile(valid)
	f.Add(data)
	f.Add([]byte(`["aGVsbG8="]`))
	f.Add([]byte("ZQSNAP2\x00garbage"))

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "fuzz.bin")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skip()
		}
		// must never panic; errors are fine
		_ = newRegistry(1<<20, 1<<10, 64, 4).load(path)
	})
}
