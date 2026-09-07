package virtio

import (
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func FuzzPersistentImageMetadataParser(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("not persistent metadata"))
	var oversized [persistentImageHeaderSize]byte
	copy(oversized[:8], persistentImageMagic[:])
	binary.LittleEndian.PutUint32(oversized[8:12], persistentImageFormatVersion)
	binary.LittleEndian.PutUint64(oversized[24:32], persistentImageMaxMetadata)
	f.Add(oversized[:])
	f.Add(framedPersistentFuzzValue(persistentImageMagic, &persistentImageState{
		Version:    persistentImageFormatVersion,
		Sequence:   1,
		NextNodeID: 2,
	}))

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "metadata")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = readPersistentImageState(path)
	})
}

func FuzzPersistentImageWALParser(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("not a WAL"))
	f.Add(framedPersistentFuzzValue(persistentImageWALMagic, &persistentImageDelta{
		Version:    persistentImageFormatVersion,
		Sequence:   1,
		NextNodeID: 2,
		Committed:  true,
	}))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "staging"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.wal"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		store := &persistentImageStore{dir: dir}
		state := &persistentImageState{
			Version:    persistentImageFormatVersion,
			NextNodeID: 2,
		}
		_ = store.replayWAL(state)
	})
}

func framedPersistentFuzzValue(magic [8]byte, value any) []byte {
	payload, _ := json.Marshal(value)
	frame := make([]byte, persistentImageHeaderSize+len(payload))
	copy(frame[:8], magic[:])
	binary.LittleEndian.PutUint32(frame[8:12], persistentImageFormatVersion)
	var sequence uint64
	switch typed := value.(type) {
	case *persistentImageState:
		sequence = typed.Sequence
	case *persistentImageDelta:
		sequence = typed.Sequence
	}
	binary.LittleEndian.PutUint64(frame[16:24], sequence)
	binary.LittleEndian.PutUint64(frame[24:32], uint64(len(payload)))
	binary.LittleEndian.PutUint32(frame[32:36], crc32.ChecksumIEEE(payload))
	copy(frame[persistentImageHeaderSize:], payload)
	return frame
}
