package vm

import (
	"bytes"
	"io"
	"math/rand"
	"os"
	"strings"
	"testing"
)

type discardAt struct{}

func (d discardAt) WriteAt(p []byte, off int64) (n int, err error) {
	return len(p), nil
}

var (
	_ io.WriterAt = (*discardAt)(nil)
)

type countingRegion struct {
	data  RawRegion
	reads int
}

func (r *countingRegion) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	return r.data.ReadAt(p, off)
}

func (r *countingRegion) WriteAt([]byte, int64) (int, error) {
	return 0, io.ErrClosedPipe
}

func (r *countingRegion) Size() int64 {
	return r.data.Size()
}

func TestVirtualMemoryWritesOverlayMappedRegion(t *testing.T) {
	source := &countingRegion{data: RawRegion([]byte("abcdefghijklmnop"))}
	vm := NewVirtualMemory(16, 8)
	if err := vm.Map(source, 0); err != nil {
		t.Fatalf("map source: %v", err)
	}

	if _, err := vm.WriteAt([]byte("XY"), 2); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	buf := make([]byte, 16)
	if _, err := vm.ReadAt(buf, 0); err != nil {
		t.Fatalf("read merged data: %v", err)
	}
	if got, want := string(buf), "abXYefghijklmnop"; got != want {
		t.Fatalf("merged data = %q, want %q", got, want)
	}

	before := source.reads
	if _, err := vm.WriteAt([]byte("Z"), 3); err != nil {
		t.Fatalf("rewrite overlay: %v", err)
	}
	if source.reads != before {
		t.Fatalf("rewrite read source %d times, want %d", source.reads-before, 0)
	}

	out := &recordingWriterAt{}
	if _, err := vm.WriteSparseTo(out); err != nil {
		t.Fatalf("write sparse: %v", err)
	}
	if got, want := out.String(), "abXZefghijklmnop"; got != want {
		t.Fatalf("sparse output = %q, want %q", got, want)
	}
}

func TestVirtualMemoryReadAtSplitsMappedPageBoundaries(t *testing.T) {
	vm := NewVirtualMemory(16, 8)
	if err := vm.Map(RawRegion([]byte("abcdefgh")), 0); err != nil {
		t.Fatalf("map first page: %v", err)
	}
	if err := vm.Map(RawRegion([]byte("ijklmnop")), 8); err != nil {
		t.Fatalf("map second page: %v", err)
	}

	buf := make([]byte, 12)
	if _, err := vm.ReadAt(buf, 2); err != nil {
		t.Fatalf("read across page boundary: %v", err)
	}
	if got, want := string(buf), "cdefghijklmn"; got != want {
		t.Fatalf("read data = %q, want %q", got, want)
	}
}

func TestVirtualMemoryWriteAtSplitsMappedPageBoundaries(t *testing.T) {
	vm := NewVirtualMemory(16, 8)
	if err := vm.Map(RawRegion(make([]byte, 8)), 0); err != nil {
		t.Fatalf("map first page: %v", err)
	}
	if err := vm.Map(RawRegion(make([]byte, 8)), 8); err != nil {
		t.Fatalf("map second page: %v", err)
	}

	if _, err := vm.WriteAt([]byte("cdefghijklmn"), 2); err != nil {
		t.Fatalf("write across page boundary: %v", err)
	}
	buf := make([]byte, 16)
	if _, err := vm.ReadAt(buf, 0); err != nil {
		t.Fatalf("read merged data: %v", err)
	}
	if got, want := string(buf), "\x00\x00cdefghijklmn\x00\x00"; got != want {
		t.Fatalf("written data = %q, want %q", got, want)
	}
}

func TestVirtualMemoryRandomReadWriteConsistency(t *testing.T) {
	const (
		seed     = int64(0x766d7368)
		size     = 64 * 1024
		pageSize = 4096
	)

	rng := rand.New(rand.NewSource(seed))
	oracle := make([]byte, size)
	for i := range oracle {
		oracle[i] = byte(rng.Intn(256))
	}

	backing, err := os.CreateTemp(t.TempDir(), "vm-backing-*")
	if err != nil {
		t.Fatalf("create backing file: %v", err)
	}
	defer backing.Close()
	if _, err := backing.WriteAt(oracle, 0); err != nil {
		t.Fatalf("write backing file: %v", err)
	}
	if _, err := backing.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek backing file: %v", err)
	}

	vm := NewVirtualMemory(size, pageSize)
	if _, err := vm.MapFile(backing, 0, size); err != nil {
		t.Fatalf("map backing file: %v", err)
	}

	for i := 0; i < 2_000; i++ {
		switch rng.Intn(3) {
		case 0:
			off, data := randomVMSpan(rng, size, pageSize*3)
			if _, err := vm.WriteAt(data, int64(off)); err != nil {
				t.Fatalf("iteration %d write at %d len %d: %v", i, off, len(data), err)
			}
			copy(oracle[off:], data)
		case 1:
			off, data := randomVMSpan(rng, size, pageSize*3)
			region := RawRegion(bytes.Clone(data))
			if err := vm.Map(region, int64(off)); err != nil {
				t.Fatalf("iteration %d map at %d len %d: %v", i, off, len(data), err)
			}
			copy(oracle[off:], data)
		case 2:
			off, data := randomVMSpan(rng, size, pageSize*2)
			buf := make([]byte, len(data))
			if _, err := vm.ReadAt(buf, int64(off)); err != nil {
				t.Fatalf("iteration %d read at %d len %d: %v", i, off, len(buf), err)
			}
			if want := oracle[off : off+len(buf)]; !bytes.Equal(buf, want) {
				t.Fatalf("iteration %d read mismatch at %d len %d", i, off, len(buf))
			}
		}
	}

	out, err := os.CreateTemp(t.TempDir(), "vm-sparse-*")
	if err != nil {
		t.Fatalf("create sparse output: %v", err)
	}
	defer out.Close()
	if err := out.Truncate(size); err != nil {
		t.Fatalf("truncate sparse output: %v", err)
	}
	if _, err := vm.WriteSparseTo(out); err != nil {
		t.Fatalf("write sparse output: %v", err)
	}
	if err := backing.Close(); err != nil {
		t.Fatalf("close backing file: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close sparse output: %v", err)
	}
	got, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatalf("read sparse output: %v", err)
	}

	oracleFile := backing.Name() + ".oracle"
	if err := os.WriteFile(oracleFile, oracle, 0o600); err != nil {
		t.Fatalf("write oracle file: %v", err)
	}
	want, err := os.ReadFile(oracleFile)
	if err != nil {
		t.Fatalf("read oracle file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("sparse output does not match oracle file")
	}
}

func randomVMSpan(rng *rand.Rand, diskSize int, maxLen int) (int, []byte) {
	off := rng.Intn(diskSize)
	remaining := diskSize - off
	length := 1 + rng.Intn(min(remaining, maxLen))
	data := make([]byte, length)
	if _, err := rng.Read(data); err != nil {
		panic(err)
	}
	return off, data
}

type recordingWriterAt struct {
	buf []byte
}

func (w *recordingWriterAt) WriteAt(p []byte, off int64) (int, error) {
	end := int(off) + len(p)
	if end > len(w.buf) {
		grown := make([]byte, end)
		copy(grown, w.buf)
		w.buf = grown
	}
	return copy(w.buf[off:], p), nil
}

func (w *recordingWriterAt) String() string {
	return strings.TrimRight(string(w.buf), "\x00")
}

func BenchmarkNewVM1GB(b *testing.B) {
	for b.Loop() {
		vm := NewVirtualMemory(1*1024*1024*1024, 4096)
		_ = vm
	}
}

func BenchmarkNewVM128GB(b *testing.B) {
	for b.Loop() {
		vm := NewVirtualMemory(128*1024*1024*1024, 4096)
		_ = vm
	}
}

func BenchmarkVMWriteSparseTo(b *testing.B) {
	for b.Loop() {
		vm := NewVirtualMemory(1*1024*1024*1024, 4096)
		if _, err := vm.WriteSparseTo(discardAt{}); err != nil {
			_ = err
		}
	}
}
