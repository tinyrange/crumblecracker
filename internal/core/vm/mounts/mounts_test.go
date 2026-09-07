package mounts

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

func TestMountAlternateImageWithShares(t *testing.T) {
	inst := &recordingRuntimeShareAdder{}
	mounter := &recordingImageMounter{}
	image := &oci.Image{Name: "alt"}

	err := MountAlternateImageWithShares(context.Background(), inst, mounter, "/run/images/alt", image, []client.ShareMount{
		{Source: "/host/data", Mount: "/data", Writable: true},
	})
	if err != nil {
		t.Fatalf("MountAlternateImageWithShares: %v", err)
	}
	if mounter.mountPath != "/run/images/alt" || mounter.image != image {
		t.Fatalf("image mount = (%q, %p), want (%q, %p)", mounter.mountPath, mounter.image, "/run/images/alt", image)
	}
	if len(inst.shares) != 1 {
		t.Fatalf("shares = %d, want 1", len(inst.shares))
	}
	if inst.shares[0].Mount != "/run/images/alt/data" {
		t.Fatalf("rebased mount = %q", inst.shares[0].Mount)
	}
	if !inst.shares[0].Writable {
		t.Fatalf("rebased share lost writable flag")
	}
}

func TestMountAlternateImageWithSharesRequiresMounter(t *testing.T) {
	err := MountAlternateImageWithShares(context.Background(), &recordingRuntimeShareAdder{}, nil, "/run/images/alt", &oci.Image{}, nil)
	if err == nil {
		t.Fatalf("nil mounter error = %v", err)
	}
}

func TestDelegatedRuntimeResources(t *testing.T) {
	ctx := context.Background()
	shareDelegate := &recordingShareDelegate{}
	share := client.ShareMount{Source: "/host", Mount: "/guest", Writable: true}
	if err := AddDelegatedRuntimeShare(ctx, shareDelegate, share, "runtime shares"); err != nil {
		t.Fatalf("AddDelegatedRuntimeShare: %v", err)
	}
	if len(shareDelegate.shares) != 1 || shareDelegate.shares[0] != share {
		t.Fatalf("delegated shares = %#v", shareDelegate.shares)
	}
	if err := AddDelegatedRuntimeShare(ctx, nil, share, "runtime shares"); err == nil {
		t.Fatalf("nil delegated share error = %v", err)
	}

	imageDelegate := &recordingImageMounter{}
	image := &oci.Image{Name: "alt"}
	if err := AddDelegatedRuntimeImage(ctx, imageDelegate, "/run/images/alt", image); err != nil {
		t.Fatalf("AddDelegatedRuntimeImage: %v", err)
	}
	if imageDelegate.mountPath != "/run/images/alt" || imageDelegate.image != image {
		t.Fatalf("delegated image = (%q, %p)", imageDelegate.mountPath, imageDelegate.image)
	}
	if err := AddDelegatedRuntimeImage(ctx, nil, "/run/images/alt", image); err == nil {
		t.Fatalf("nil delegated image error = %v", err)
	}
}

func TestBuildRuntimeDirectoryShare(t *testing.T) {
	var gotIndex int
	var gotShare vmruntime.DirectoryShare
	mount, err := BuildRuntimeDirectoryShare(client.ShareMount{
		Source:   "/host",
		Mount:    "/guest",
		Writable: true,
		MapOwner: true,
		OwnerUID: 1000,
		OwnerGID: 1001,
		Cache:    "auto",
	}, func(index int, share vmruntime.DirectoryShare) (virtio.ShareMount, error) {
		gotIndex = index
		gotShare = share
		return virtio.ShareMount{GuestPath: share.Mount, Writable: share.Writable, CacheMode: share.Cache}, nil
	})
	if err != nil {
		t.Fatalf("BuildRuntimeDirectoryShare: %v", err)
	}
	if gotIndex != 0 || gotShare.Source != "/host" || gotShare.Mount != "/guest" || !gotShare.Writable || !gotShare.MapOwner || gotShare.OwnerUID != 1000 || gotShare.OwnerGID != 1001 || gotShare.Cache != "auto" {
		t.Fatalf("builder received index=%d share=%+v", gotIndex, gotShare)
	}
	if mount.GuestPath != "/guest" || !mount.Writable || mount.CacheMode != "auto" {
		t.Fatalf("mount = %+v", mount)
	}

	if _, err := BuildRuntimeDirectoryShare(client.ShareMount{Mount: "/guest"}, nil); err == nil {
		t.Fatalf("nil builder error = %v", err)
	}
}

func TestAddImageMountTracksDuplicates(t *testing.T) {
	root := &recordingShareMounter{}
	var mu sync.Mutex
	imageMounts := map[string]string{}
	image := &oci.Image{
		Name:   "alt",
		RootFS: imagefs.NewHostFS(t.TempDir(), nil),
	}
	backend := ImageFSBackend(image)
	if err := AddImageMount(root, &mu, &imageMounts, "/run/images/alt", image, backend); err != nil {
		t.Fatalf("AddImageMount: %v", err)
	}
	if len(root.shares) != 1 {
		t.Fatalf("shares = %d, want 1", len(root.shares))
	}
	if root.shares[0].GuestPath != "/run/images/alt" || root.shares[0].Backend != backend || !root.shares[0].Writable || root.shares[0].CacheMode != "aggressive" {
		t.Fatalf("share mount = %+v", root.shares[0])
	}
	if imageMounts["/run/images/alt"] != "alt" {
		t.Fatalf("imageMounts = %#v", imageMounts)
	}

	if err := AddImageMount(root, &mu, &imageMounts, "/run/images/alt", image, backend); err != nil {
		t.Fatalf("duplicate same image: %v", err)
	}
	if len(root.shares) != 1 {
		t.Fatalf("duplicate added share, count = %d", len(root.shares))
	}
	other := &oci.Image{Name: "other", RootFS: image.RootFS}
	err := AddImageMount(root, &mu, &imageMounts, "/run/images/alt", other, ImageFSBackend(other))
	if err == nil {
		t.Fatalf("duplicate other image error = %v", err)
	}
}

func TestManagedMountStateAddImageTracksDuplicates(t *testing.T) {
	root := &recordingShareMounter{}
	state := State{}
	image := &oci.Image{
		Name:   "alt",
		RootFS: imagefs.NewHostFS(t.TempDir(), nil),
	}
	backend := ImageFSBackend(image)
	if err := state.AddImage(root, "/run/images/alt", image, backend); err != nil {
		t.Fatalf("AddImage: %v", err)
	}
	if len(root.shares) != 1 {
		t.Fatalf("shares = %d, want 1", len(root.shares))
	}
	if root.shares[0].GuestPath != "/run/images/alt" {
		t.Fatalf("guest path = %q", root.shares[0].GuestPath)
	}
	if err := state.AddImage(root, "/run/images/alt", image, backend); err != nil {
		t.Fatalf("duplicate same image: %v", err)
	}
	if len(root.shares) != 1 {
		t.Fatalf("duplicate added share, count = %d", len(root.shares))
	}
	other := &oci.Image{Name: "other", RootFS: image.RootFS}
	err := state.AddImage(root, "/run/images/alt", other, ImageFSBackend(other))
	if err == nil {
		t.Fatalf("duplicate other image error = %v", err)
	}
}

func TestAddImageMountValidatesInputs(t *testing.T) {
	var mu sync.Mutex
	imageMounts := map[string]string{}
	root := &recordingShareMounter{}
	image := &oci.Image{Name: "alt", RootFS: imagefs.NewHostFS(t.TempDir(), nil)}
	for _, tc := range []struct {
		name      string
		root      virtio.ShareMounter
		mountPath string
		image     *oci.Image
		backend   virtio.FSBackend
	}{
		{name: "missing root", root: nil, mountPath: "/run/images/alt", image: image, backend: ImageFSBackend(image)},
		{name: "relative mount", root: root, mountPath: "relative", image: image, backend: ImageFSBackend(image)},
		{name: "missing image", root: root, mountPath: "/run/images/alt", image: nil, backend: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := AddImageMount(tc.root, &mu, &imageMounts, tc.mountPath, tc.image, tc.backend)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestAddRuntimeShareMountTracksDuplicates(t *testing.T) {
	root := &recordingShareMounter{}
	var mu sync.Mutex
	shares := map[string]client.ShareMount{}
	share := client.ShareMount{Source: "/host/data", Mount: "/data", Writable: true, Cache: "auto"}
	builds := 0
	build := func(share client.ShareMount) (virtio.ShareMount, error) {
		builds++
		return virtio.ShareMount{GuestPath: share.Mount, Writable: share.Writable, CacheMode: share.Cache}, nil
	}
	if err := AddRuntimeShareMount(root, &mu, &shares, share, "shares", build); err != nil {
		t.Fatalf("AddRuntimeShareMount: %v", err)
	}
	if builds != 1 || len(root.shares) != 1 {
		t.Fatalf("builds/shares = %d/%d, want 1/1", builds, len(root.shares))
	}
	if root.shares[0].GuestPath != "/data" || !root.shares[0].Writable || root.shares[0].CacheMode != "auto" {
		t.Fatalf("share mount = %+v", root.shares[0])
	}
	if shares["/data"] != share {
		t.Fatalf("tracked shares = %#v", shares)
	}
	if err := AddRuntimeShareMount(root, &mu, &shares, share, "shares", build); err != nil {
		t.Fatalf("duplicate same share: %v", err)
	}
	if builds != 1 || len(root.shares) != 1 {
		t.Fatalf("duplicate builds/shares = %d/%d, want 1/1", builds, len(root.shares))
	}
	err := AddRuntimeShareMount(root, &mu, &shares, client.ShareMount{Source: "/other", Mount: "/data"}, "shares", build)
	if err == nil {
		t.Fatalf("duplicate conflicting share error = %v", err)
	}
}

func TestManagedMountStateAddShareTracksDuplicates(t *testing.T) {
	root := &recordingShareMounter{}
	state := State{}
	share := client.ShareMount{Source: "/host/data", Mount: "/data", Writable: true, Cache: "auto"}
	builds := 0
	build := func(share client.ShareMount) (virtio.ShareMount, error) {
		builds++
		return virtio.ShareMount{GuestPath: share.Mount, Writable: share.Writable, CacheMode: share.Cache}, nil
	}
	if err := state.AddShare(root, share, "shares", build); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	if builds != 1 || len(root.shares) != 1 {
		t.Fatalf("builds/shares = %d/%d, want 1/1", builds, len(root.shares))
	}
	if err := state.AddShare(root, share, "shares", build); err != nil {
		t.Fatalf("duplicate same share: %v", err)
	}
	if builds != 1 || len(root.shares) != 1 {
		t.Fatalf("duplicate builds/shares = %d/%d, want 1/1", builds, len(root.shares))
	}
	err := state.AddShare(root, client.ShareMount{Source: "/other", Mount: "/data"}, "shares", build)
	if err == nil {
		t.Fatalf("duplicate conflicting share error = %v", err)
	}
}

func TestManagedMountStateIncludesStartupShares(t *testing.T) {
	share := client.ShareMount{Source: "/host/data", Mount: "/data", Writable: true, Cache: "auto"}
	state, err := NewState([]client.ShareMount{share})
	if err != nil {
		t.Fatal(err)
	}
	root := &recordingShareMounter{}
	builds := 0
	build := func(share client.ShareMount) (virtio.ShareMount, error) {
		builds++
		return virtio.ShareMount{GuestPath: share.Mount}, nil
	}

	if err := state.AddShare(root, share, "shares", build); err != nil {
		t.Fatalf("repeat startup share: %v", err)
	}
	if builds != 0 || len(root.shares) != 0 {
		t.Fatalf("repeat startup share builds/mounts = %d/%d, want 0/0", builds, len(root.shares))
	}
	if err := state.AddShare(root, client.ShareMount{Source: "/other", Mount: "/data"}, "shares", build); err == nil {
		t.Fatal("conflicting startup share unexpectedly succeeded")
	}
}

func TestManagedMountStateBuildsWholeShareBatchBeforeMutation(t *testing.T) {
	state, err := NewState(nil)
	if err != nil {
		t.Fatal(err)
	}
	root := &recordingShareMounter{}
	builds := 0
	err = state.AddShares(root, []client.ShareMount{
		{Source: "/host/one", Mount: "/one"},
		{Source: "/host/two", Mount: "/two"},
	}, "shares", func(share client.ShareMount) (virtio.ShareMount, error) {
		builds++
		if builds == 2 {
			return virtio.ShareMount{}, errors.New("injected second build failure")
		}
		return virtio.ShareMount{GuestPath: share.Mount}, nil
	})
	if err == nil {
		t.Fatal("share batch unexpectedly succeeded")
	}
	if len(root.shares) != 0 || len(state.shares) != 0 {
		t.Fatalf("failed share batch mutated root/state: root=%+v state=%+v", root.shares, state.shares)
	}
}

func TestManagedMountStateClosesPreparedSharesAfterBatchFailure(t *testing.T) {
	state, err := NewState(nil)
	if err != nil {
		t.Fatal(err)
	}
	root := &recordingShareMounter{}
	prepared := &closingShareBackend{}
	builds := 0
	err = state.AddShares(root, []client.ShareMount{
		{Source: "/host/one", Mount: "/one/../one"},
		{Source: "/host/two", Mount: "/two"},
	}, "shares", func(share client.ShareMount) (virtio.ShareMount, error) {
		builds++
		if builds == 2 {
			return virtio.ShareMount{}, errors.New("injected second build failure")
		}
		return virtio.ShareMount{GuestPath: share.Mount, Backend: prepared}, nil
	})
	if err == nil {
		t.Fatal("share batch unexpectedly succeeded")
	}
	if prepared.closes != 1 {
		t.Fatalf("prepared backend closes = %d, want 1", prepared.closes)
	}
	if len(root.shares) != 0 {
		t.Fatalf("failed share batch mutated root: %+v", root.shares)
	}
}

func TestCanonicalRuntimeShareRejectsRootAndNormalizesAliases(t *testing.T) {
	if _, err := CanonicalRuntimeShare(client.ShareMount{Mount: "/"}); err == nil {
		t.Fatal("root runtime share unexpectedly accepted")
	}
	if _, err := NewState([]client.ShareMount{{Mount: "/"}}); err == nil {
		t.Fatal("invalid startup share was silently omitted from mount state")
	}
	share, err := CanonicalRuntimeShare(client.ShareMount{Source: "/host", Mount: " /data/../data/ "})
	if err != nil {
		t.Fatal(err)
	}
	if share.Mount != "/data" {
		t.Fatalf("canonical mount = %q", share.Mount)
	}
}

func TestAddRuntimeShareMountValidatesInputs(t *testing.T) {
	var mu sync.Mutex
	shares := map[string]client.ShareMount{}
	build := func(share client.ShareMount) (virtio.ShareMount, error) {
		return virtio.ShareMount{GuestPath: share.Mount}, nil
	}
	err := AddRuntimeShareMount(nil, &mu, &shares, client.ShareMount{Mount: "/data"}, "runtime shares", build)
	if err == nil {
		t.Fatalf("nil root error = %v", err)
	}
	err = AddRuntimeShareMount(&recordingShareMounter{}, &mu, &shares, client.ShareMount{}, "shares", build)
	if err == nil {
		t.Fatalf("missing mount error = %v", err)
	}
}

type recordingRuntimeShareAdder struct {
	shares []client.ShareMount
}

func (a *recordingRuntimeShareAdder) AddShare(_ context.Context, share client.ShareMount) error {
	a.shares = append(a.shares, share)
	return nil
}

type recordingImageMounter struct {
	mountPath string
	image     *oci.Image
}

func (m *recordingImageMounter) AddImage(_ context.Context, mountPath string, image *oci.Image) error {
	m.mountPath = mountPath
	m.image = image
	return nil
}

type recordingShareMounter struct {
	shares []virtio.ShareMount
}

type closingShareBackend struct {
	virtio.FSBackend
	closes int
}

func (b *closingShareBackend) Close() error {
	b.closes++
	return nil
}

func (m *recordingShareMounter) AddShare(share virtio.ShareMount) error {
	m.shares = append(m.shares, share)
	return nil
}

type recordingShareDelegate struct {
	shares []client.ShareMount
}

func (d *recordingShareDelegate) AddShare(_ context.Context, share client.ShareMount) error {
	d.shares = append(d.shares, share)
	return nil
}
