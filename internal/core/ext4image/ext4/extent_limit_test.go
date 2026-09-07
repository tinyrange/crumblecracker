package ext4

import (
	"errors"
	"testing"
)

func TestNewExtentTreeRejectsUnsupportedDepthBeforeAllocation(t *testing.T) {
	var sb Superblock
	if !sb.SetBlocksPerGroup(8) {
		t.Fatal("set blocks per group")
	}
	filesystem := &Ext4Filesystem{sb: &sb}
	_, err := newExtentTree(filesystem, &InodeWrapper{fs: filesystem}, 33)
	var limitErr *ExtentLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("newExtentTree error = %v, want ExtentLimitError", err)
	}
	if limitErr.RequiredExtents != 5 || limitErr.SupportedExtents != MaxInlineExtents {
		t.Fatalf("extent limit error = %#v", limitErr)
	}
}
