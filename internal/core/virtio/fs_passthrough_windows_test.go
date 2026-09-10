//go:build windows

package virtio

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsSharedUnixMetadataSurvivesAliasesAndRestart(t *testing.T) {
	root := t.TempDir()
	first := NewPassthroughFS(root, nil).(*passthroughFS)
	node, fh, attr, errno := first.Create(1, "script", linuxORDWR, 0600, 1000, 100)
	if errno != 0 {
		t.Fatalf("create errno %d", errno)
	}
	defer first.Release(node, fh)
	if attr.Mode&linuxPermMask != 0600 || attr.UID != 1000 || attr.GID != 100 {
		t.Fatalf("create attr %+v", attr)
	}
	if _, errno := first.Write(node, fh, 0, []byte("#!/bin/sh\nexit 0\n"), 0); errno != 0 {
		t.Fatal(errno)
	}
	second := NewPassthroughFS(root, nil).(*passthroughFS)
	alias, _, errno := second.Lookup(1, "script")
	if errno != 0 {
		t.Fatal(errno)
	}
	if _, errno := first.SetAttr(node, fattrMode, 0, 0, 0755, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatal(errno)
	}
	if _, errno := second.SetAttr(alias, fattrGID, 0, 0, 0, 0, 200, time.Time{}, time.Time{}); errno != 0 {
		t.Fatal(errno)
	}
	if err := os.Rename(filepath.Join(root, "script"), filepath.Join(root, "renamed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "renamed"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	reopened := NewPassthroughFS(root, nil).(*passthroughFS)
	for _, name := range []string{"renamed", "alias"} {
		_, attr, errno := reopened.Lookup(1, name)
		if errno != 0 || attr.Mode&linuxPermMask != 0755 || attr.UID != 1000 || attr.GID != 200 {
			t.Fatalf("%s: attr %+v errno %d", name, attr, errno)
		}
	}
	if err := os.Remove(filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alias"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	_, attr, errno = reopened.Lookup(1, "alias")
	if errno != 0 || attr.Mode&0111 != 0 {
		t.Fatalf("replacement inherited executable metadata: %+v errno %d", attr, errno)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected visible metadata files: %v %v", entries, err)
	}
}

func TestWindowsSharedDirectoryPermissionsPersist(t *testing.T) {
	root := t.TempDir()
	fsys := NewPassthroughFS(root, nil).(*passthroughFS)
	id, _, errno := fsys.Mkdir(1, "private", 0700, 1000, 100)
	if errno != 0 {
		t.Fatal(errno)
	}
	if _, errno := fsys.SetAttr(id, fattrMode, 0, 0, 0750, 0, 0, time.Time{}, time.Time{}); errno != 0 {
		t.Fatal(errno)
	}
	_, attr, errno := NewPassthroughFS(root, nil).Lookup(1, "private")
	if errno != 0 || attr.Mode&linuxPermMask != 0750 {
		t.Fatalf("directory attr %+v errno %d", attr, errno)
	}
	if errno := fsys.RmDir(1, "private"); errno != 0 {
		t.Fatalf("directory metadata prevented rmdir: %d", errno)
	}
}
