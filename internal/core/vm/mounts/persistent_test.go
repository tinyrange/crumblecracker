package mounts

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestBuildPersistentImageMountsUsesImageHomeAndReleasesFailedPreparation(t *testing.T) {
	image := persistentMountTestImage(t, "scientist")
	storeRoot := filepath.Join(t.TempDir(), "homes")

	prepared, err := BuildPersistentImageMounts(storeRoot, image, []client.PersistentMount{
		{Name: "research"},
	})
	if err != nil {
		t.Fatalf("build persistent image mount: %v", err)
	}
	if len(prepared) != 1 || prepared[0].GuestPath != "/home/scientist" || !prepared[0].Writable || prepared[0].CacheMode != "aggressive" {
		t.Fatalf("persistent mount = %+v", prepared)
	}
	statusProvider, ok := prepared[0].Backend.(interface {
		PersistentFSStatus() []virtio.PersistentFSStatus
	})
	if !ok {
		t.Fatalf("persistent backend %T does not expose status", prepared[0].Backend)
	}
	statuses := statusProvider.PersistentFSStatus()
	if len(statuses) != 1 || statuses[0].Name != "research" || statuses[0].Mount != "/home/scientist" || statuses[0].FormatVersion != 1 {
		t.Fatalf("persistent status = %+v", statuses)
	}
	if err := closePersistentMounts(prepared); err != nil {
		t.Fatal(err)
	}

	_, err = BuildPersistentImageMounts(storeRoot, image, []client.PersistentMount{
		{Name: "research"},
		{Name: "invalid-second", Mount: "/does-not-exist"},
	})
	if err == nil {
		t.Fatal("preparation with an invalid second mount succeeded")
	}
	// The first mount from the failed group must not retain its writer lock.
	prepared, err = BuildPersistentImageMounts(storeRoot, image, []client.PersistentMount{{Name: "research"}})
	if err != nil {
		t.Fatalf("reopen mount after failed atomic preparation: %v", err)
	}
	if err := closePersistentMounts(prepared); err != nil {
		t.Fatal(err)
	}
}

func TestImageHomeDirectoryResolvesNameAndNumericUID(t *testing.T) {
	for _, user := range []string{"scientist", "1000", "scientist:users", "1000:1000"} {
		t.Run(user, func(t *testing.T) {
			image := persistentMountTestImage(t, user)
			home, err := imageHomeDirectory(image)
			if err != nil {
				t.Fatalf("resolve image home: %v", err)
			}
			if home != "/home/scientist" {
				t.Fatalf("home = %q", home)
			}
		})
	}
}

func TestImageHomeDirectoryUsesConfiguredDesktopHome(t *testing.T) {
	image := persistentMountTestImage(t, "root")
	image.Config.Env = []string{"HOME=/root", "HOME=/home/scientist"}
	home, err := imageHomeDirectory(image)
	if err != nil {
		t.Fatalf("resolve configured desktop home: %v", err)
	}
	if home != "/home/scientist" {
		t.Fatalf("home = %q", home)
	}
}

func TestBuildPersistentImageMountsRejectsStoreNameTraversal(t *testing.T) {
	image := persistentMountTestImage(t, "scientist")
	if _, err := BuildPersistentImageMounts(t.TempDir(), image, []client.PersistentMount{{Name: "../outside"}}); err == nil {
		t.Fatal("persistent mount name traversal succeeded")
	}
}

func TestPersistentImageMountScopesAnImageWideNamespaceToTheSelectedDirectory(t *testing.T) {
	overlay := imagefs.NewOverlay(nil)
	for _, dir := range []string{"/home", "/home/scientist"} {
		if err := overlay.AddDir(dir, fs.ModeDir|0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := overlay.AddFile("/home/scientist/inside", 0o644, []byte("inside")); err != nil {
		t.Fatal(err)
	}
	if err := overlay.AddFile("/outside", 0o644, []byte("outside")); err != nil {
		t.Fatal(err)
	}
	namespace, err := imagefs.BuildNamespace(overlay.Root())
	if err != nil {
		t.Fatal(err)
	}
	image := &oci.Image{
		Name:   "desktop",
		Source: "desktop@sha256:test",
		RootFS: namespaceLeakingDirectory{Directory: overlay.Root(), namespace: namespace},
	}
	prepared, err := BuildPersistentImageMounts(t.TempDir(), image, []client.PersistentMount{{
		Name: "scoped", Mount: "/home/scientist",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer closePersistentMounts(prepared)
	if _, _, errno := prepared[0].Backend.Lookup(1, "inside"); errno != 0 {
		t.Fatalf("lookup selected home file: errno %d", errno)
	}
	if _, _, errno := prepared[0].Backend.Lookup(1, "outside"); errno == 0 {
		t.Fatal("persistent home exposed a file outside the selected image directory")
	}
}

type namespaceLeakingDirectory struct {
	imagefs.Directory
	namespace *imagefs.Namespace
}

func (d namespaceLeakingDirectory) Namespace() *imagefs.Namespace {
	return d.namespace
}

func (d namespaceLeakingDirectory) Lookup(name string) (imagefs.Entry, error) {
	entry, err := d.Directory.Lookup(name)
	if err == nil && entry.Dir != nil {
		entry.Dir = namespaceLeakingDirectory{Directory: entry.Dir, namespace: d.namespace}
	}
	return entry, err
}

func persistentMountTestImage(t *testing.T, user string) *oci.Image {
	t.Helper()
	overlay := imagefs.NewOverlay(nil)
	for _, dir := range []string{"/etc", "/home", "/home/scientist"} {
		if err := overlay.AddDir(dir, fs.ModeDir|0o755); err != nil {
			t.Fatal(err)
		}
	}
	passwd := []byte("root:x:0:0:root:/root:/bin/sh\nscientist:x:1000:1000:Scientist:/home/scientist:/bin/sh\n")
	if err := overlay.AddFile("/etc/passwd", 0o644, passwd); err != nil {
		t.Fatal(err)
	}
	return &oci.Image{
		Name:   "desktop",
		Source: "desktop@sha256:test",
		RootFS: overlay.Root(),
		Config: oci.RuntimeConfig{User: user},
	}
}
