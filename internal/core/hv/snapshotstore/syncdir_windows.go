//go:build windows

package snapshotstore

// Windows does not expose portable directory fsync through os.File. Component
// files are flushed before MoveFileEx-backed os.Rename publishes the capture.
func syncDir(string) error { return nil }
