package virtio

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestImageDataStorePortableCompactionPreservesLiveLocations(t *testing.T) {
	store := newImageDataStore()
	defer store.close()
	locations := make([]uint64, 3)
	for i := range locations {
		page := bytes.Repeat([]byte{byte(i + 1)}, int(imageDataPageSize))
		location, err := store.allocatePage(page)
		if err != nil {
			t.Fatal(err)
		}
		locations[i] = location
	}
	store.releasePage(locations[0])
	store.releasePage(locations[1])
	store.mu.Lock()
	err := store.compactLocked()
	next := store.next
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if next != imageDataPageSize {
		t.Fatalf("compacted backing size = %d, want %d", next, imageDataPageSize)
	}
	page := make([]byte, imageDataPageSize)
	if err := store.readPage(locations[2], page); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(page, bytes.Repeat([]byte{3}, len(page))) {
		t.Fatal("live page changed during compaction")
	}
}

func TestImageDataStoreFailedCompactionPreservesOldLocations(t *testing.T) {
	store := newImageDataStore()
	defer store.close()
	locations := make([]uint64, 3)
	for i := range locations {
		location, err := store.allocatePage(bytes.Repeat([]byte{byte(i + 1)}, int(imageDataPageSize)))
		if err != nil {
			t.Fatal(err)
		}
		locations[i] = location
	}
	store.releasePage(locations[0])

	store.mu.Lock()
	// Leave the second live page readable, then force the later read to fail.
	// A non-transactional compaction has already redirected the first live
	// token to offset zero by the time it observes this failure.
	if err := store.file.Truncate(2 * int64(imageDataPageSize)); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	err := store.compactLocked()
	store.mu.Unlock()
	if err == nil {
		t.Fatal("compaction unexpectedly succeeded with a truncated source")
	}

	page := make([]byte, imageDataPageSize)
	if err := store.readPage(locations[1], page); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(page, bytes.Repeat([]byte{2}, len(page))) {
		t.Fatal("failed compaction changed a live page mapping")
	}
}

func TestImageDataStoreRetainsFailedReplacementCleanupForRetry(t *testing.T) {
	store := newImageDataStore()
	dir := t.TempDir()
	stalePath := filepath.Join(dir, "replaced")
	if err := os.Mkdir(stalePath, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(stalePath, "scanner-hold")
	if err := os.WriteFile(child, []byte("hold"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.stale = append(store.stale, imageDataStaleFile{path: stalePath})
	err := store.cleanupStaleLocked()
	retained := len(store.stale)
	store.mu.Unlock()
	if err == nil || retained != 1 {
		t.Fatalf("failed cleanup error=%v retained=%d", err, retained)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	err = store.cleanupStaleLocked()
	retained = len(store.stale)
	store.mu.Unlock()
	if err != nil || retained != 0 {
		t.Fatalf("cleanup retry error=%v retained=%d", err, retained)
	}
}

func TestImageDataStoreClearsRecoveredStaleCleanupCondition(t *testing.T) {
	store := newImageDataStore()
	dir := t.TempDir()
	stalePath := filepath.Join(dir, "replaced")
	if err := os.Mkdir(stalePath, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(stalePath, "scanner-hold")
	if err := os.WriteFile(child, []byte("hold"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.stale = append(store.stale, imageDataStaleFile{path: stalePath})
	store.mu.Unlock()
	if _, _, _, err := store.usage(); err == nil {
		t.Fatal("active stale cleanup failure was not reported")
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.usage(); err != nil {
		t.Fatalf("successful cleanup retry retained a stale current error: %v", err)
	}
}

func TestImageDataStoreBatchesReleasedPagesAndAccountsImmediately(t *testing.T) {
	store := newImageDataStore()
	defer store.close()
	locations := make([]uint64, 256)
	for i := range locations {
		location, err := store.allocatePage([]byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		locations[i] = location
	}
	for _, location := range locations {
		store.releasePage(location)
	}
	store.mu.Lock()
	locationChunks := len(store.locationChunks)
	store.mu.Unlock()
	if locationChunks != 0 {
		t.Fatalf("released location indexes retained %d chunks", locationChunks)
	}
	current, highWater, _, err := store.usage()
	if current != 0 || highWater != uint64(len(locations))*imageDataPageSize || err != nil {
		t.Fatalf("usage after logical release = %d, %d, %v", current, highWater, err)
	}
	if err := store.sync(); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	next := store.next
	freeCapacity := cap(store.free)
	retainedFreeSet := store.retainedFreeSet
	retainedPending := store.retainedPendingReclaim
	store.mu.Unlock()
	if next != 0 {
		t.Fatalf("backing length after batched reclaim = %d, want 0", next)
	}
	if freeCapacity != 0 || retainedFreeSet != 0 || retainedPending != 0 {
		t.Fatalf("released page metadata retained free=%d free-set=%d pending=%d", freeCapacity, retainedFreeSet, retainedPending)
	}
}

func TestImageDataStoreHighLiveTokenDoesNotPinReleasedIndexes(t *testing.T) {
	store := newImageDataStore()
	defer store.close()
	locations := make([]uint64, 2000)
	for i := range locations {
		location, err := store.allocatePage([]byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		locations[i] = location
	}
	for _, location := range locations[:len(locations)-1] {
		store.releasePage(location)
	}
	if err := store.sync(); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	chunks := len(store.locationChunks)
	metadata := store.metadataUsageLocked()
	store.mu.Unlock()
	if chunks != 1 {
		t.Fatalf("one high live token retained %d location chunks, want 1", chunks)
	}
	if metadata > 64<<10 {
		t.Fatalf("one high live token retained %d bytes of store metadata", metadata)
	}
	page := make([]byte, imageDataPageSize)
	if err := store.readPage(locations[len(locations)-1], page); err != nil {
		t.Fatalf("read high live token: %v", err)
	}
}

func TestPortableImageStoreCompactsBeforeTwofoldAmplification(t *testing.T) {
	page := uint64(imageDataPageSize)
	if !shouldCompactPortableImageStore(1000*page, 1999*page) {
		t.Fatal("nearly twofold physical amplification was retained")
	}
	if shouldCompactPortableImageStore(1000*page, 1499*page) {
		t.Fatal("sub-threshold dead space triggered compaction")
	}
	if shouldCompactPortableImageStore(1<<20, 1792<<10) {
		t.Fatal("small backing file triggered portable compaction")
	}
}
