package mounts

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"strings"
	"sync"

	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type AlternateImageMounter interface {
	AddImage(context.Context, string, *oci.Image) error
}

type DelegatedShareMounter interface {
	AddShare(context.Context, client.ShareMount) error
}

type RuntimeShareAdder interface {
	AddShare(context.Context, client.ShareMount) error
}

type RuntimeShareBatchAdder interface {
	AddShares(context.Context, []client.ShareMount) error
}

type State struct {
	mu          sync.Mutex
	shares      map[string]client.ShareMount
	imageMounts map[string]string
}

func NewState(shares []client.ShareMount) (*State, error) {
	tracked := make(map[string]client.ShareMount, len(shares))
	for _, share := range shares {
		canonical, err := CanonicalRuntimeShare(share)
		if err != nil {
			return nil, err
		}
		tracked[canonical.Mount] = canonical
	}
	return &State{shares: tracked}, nil
}

func (s *State) AddShare(rootFS virtio.ShareMounter, share client.ShareMount, unsupportedFeature string, build func(client.ShareMount) (virtio.ShareMount, error)) error {
	return s.AddShares(rootFS, []client.ShareMount{share}, unsupportedFeature, build)
}

func (s *State) AddShares(rootFS virtio.ShareMounter, requested []client.ShareMount, unsupportedFeature string, build func(client.ShareMount) (virtio.ShareMount, error)) error {
	if s == nil {
		if len(requested) == 1 {
			return AddRuntimeShareMount(rootFS, nil, nil, requested[0], unsupportedFeature, build)
		}
		return fmt.Errorf("instance mount state is unavailable for atomic multi-share mutation")
	}
	if rootFS == nil {
		return fmt.Errorf("instance rootfs does not support %s", firstNonEmptyMountFeature(unsupportedFeature))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prospective := make(map[string]client.ShareMount, len(s.shares)+len(requested))
	for key, share := range s.shares {
		prospective[key] = share
	}
	var mountsToAdd []virtio.ShareMount
	var sharesToAdd []client.ShareMount
	releasePrepared := true
	defer func() {
		if releasePrepared {
			closePreparedShareMounts(mountsToAdd)
		}
	}()
	for _, rawShare := range requested {
		share, err := CanonicalRuntimeShare(rawShare)
		if err != nil {
			return err
		}
		key := share.Mount
		if existing, ok := prospective[key]; ok {
			if existing == share {
				continue
			}
			return fmt.Errorf("share mount %q already exists", key)
		}
		mount, err := build(share)
		if err != nil {
			return err
		}
		prospective[key] = share
		sharesToAdd = append(sharesToAdd, share)
		mountsToAdd = append(mountsToAdd, mount)
	}
	if len(mountsToAdd) == 0 {
		releasePrepared = false
		return nil
	}
	batch, ok := rootFS.(virtio.ShareBatchMounter)
	if !ok && len(mountsToAdd) > 1 {
		return fmt.Errorf("instance rootfs does not support atomic multi-share mutation")
	}
	if ok {
		if err := batch.AddShares(mountsToAdd); err != nil {
			return err
		}
	} else if err := rootFS.AddShare(mountsToAdd[0]); err != nil {
		return err
	}
	if s.shares == nil {
		s.shares = make(map[string]client.ShareMount)
	}
	for _, share := range sharesToAdd {
		s.shares[share.Mount] = share
	}
	releasePrepared = false
	return nil
}

func CanonicalRuntimeShare(share client.ShareMount) (client.ShareMount, error) {
	canonical, err := vmruntime.CanonicalDirectoryShare(vmruntime.DirectoryShare{
		Source: share.Source, Mount: share.Mount, Writable: share.Writable,
		MapOwner: share.MapOwner, OwnerUID: share.OwnerUID, OwnerGID: share.OwnerGID,
		Cache: share.Cache,
	})
	if err != nil {
		return client.ShareMount{}, err
	}
	share.Source = canonical.Source
	share.Mount = canonical.Mount
	share.Writable = canonical.Writable
	share.MapOwner = canonical.MapOwner
	share.OwnerUID = canonical.OwnerUID
	share.OwnerGID = canonical.OwnerGID
	share.Cache = canonical.Cache
	return share, nil
}

func CanonicalRuntimeShares(shares []client.ShareMount) ([]client.ShareMount, error) {
	if len(shares) == 0 {
		return shares, nil
	}
	out := make([]client.ShareMount, 0, len(shares))
	for _, share := range shares {
		canonical, err := CanonicalRuntimeShare(share)
		if err != nil {
			return nil, err
		}
		out = append(out, canonical)
	}
	return out, nil
}

func closePreparedShareMounts(prepared []virtio.ShareMount) {
	var closed []virtio.FSBackend
	for _, mount := range prepared {
		if mount.Backend == nil {
			continue
		}
		duplicate := false
		for _, backend := range closed {
			if samePreparedShareBackend(backend, mount.Backend) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		closed = append(closed, mount.Backend)
		if closer, ok := mount.Backend.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
}

func samePreparedShareBackend(a, b virtio.FSBackend) bool {
	if a == nil || b == nil || reflect.TypeOf(a) != reflect.TypeOf(b) {
		return a == nil && b == nil
	}
	typ := reflect.TypeOf(a)
	if typ.Comparable() {
		return reflect.ValueOf(a).Interface() == reflect.ValueOf(b).Interface()
	}
	switch typ.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
	default:
		return false
	}
}

func (s *State) AddImage(rootFS virtio.ShareMounter, mountPath string, image *oci.Image, backend virtio.FSBackend) error {
	if s == nil {
		return AddImageMount(rootFS, nil, nil, mountPath, image, backend)
	}
	return AddImageMount(rootFS, &s.mu, &s.imageMounts, mountPath, image, backend)
}

func AddRuntimeShares(ctx context.Context, inst RuntimeShareAdder, shares []client.ShareMount) error {
	if len(shares) > 1 {
		batch, ok := inst.(RuntimeShareBatchAdder)
		if !ok {
			return fmt.Errorf("instance does not support atomic multi-share mutation")
		}
		return batch.AddShares(ctx, shares)
	}
	for _, share := range shares {
		if err := inst.AddShare(ctx, share); err != nil {
			return err
		}
	}
	return nil
}

func firstNonEmptyMountFeature(feature string) string {
	if feature = strings.TrimSpace(feature); feature != "" {
		return feature
	}
	return "shares"
}

func AddDelegatedRuntimeShare(ctx context.Context, delegate DelegatedShareMounter, share client.ShareMount, unsupportedFeature string) error {
	if delegate == nil {
		return AddRuntimeShareMount(nil, nil, nil, share, unsupportedFeature, nil)
	}
	return delegate.AddShare(ctx, share)
}

func AddDelegatedRuntimeImage(ctx context.Context, delegate AlternateImageMounter, mountPath string, image *oci.Image) error {
	if delegate == nil {
		return AddImageMount(nil, nil, nil, mountPath, image, nil)
	}
	return delegate.AddImage(ctx, mountPath, image)
}

func RebaseRuntimeShares(rootDir string, shares []client.ShareMount) []client.ShareMount {
	if rootDir == "" || len(shares) == 0 {
		return append([]client.ShareMount(nil), shares...)
	}
	out := make([]client.ShareMount, 0, len(shares))
	for _, share := range shares {
		rebased := share
		rebased.Mount = path.Join(rootDir, share.Mount)
		out = append(out, rebased)
	}
	return out
}

func ConvertShareMounts(shares []client.ShareMount) []vmruntime.DirectoryShare {
	if len(shares) == 0 {
		return nil
	}
	out := make([]vmruntime.DirectoryShare, 0, len(shares))
	for _, share := range shares {
		out = append(out, ShareMountToDirectoryShare(share))
	}
	return out
}

func ShareMountToDirectoryShare(share client.ShareMount) vmruntime.DirectoryShare {
	return vmruntime.DirectoryShare{
		Source:   share.Source,
		Mount:    share.Mount,
		Writable: share.Writable,
		MapOwner: share.MapOwner,
		OwnerUID: share.OwnerUID,
		OwnerGID: share.OwnerGID,
		Cache:    share.Cache,
	}
}

func BuildRuntimeDirectoryShare(share client.ShareMount, build func(int, vmruntime.DirectoryShare) (virtio.ShareMount, error)) (virtio.ShareMount, error) {
	if build == nil {
		return virtio.ShareMount{}, fmt.Errorf("share mount builder is not configured")
	}
	return build(0, ShareMountToDirectoryShare(share))
}

func MountAlternateImageWithShares(ctx context.Context, inst RuntimeShareAdder, mounter AlternateImageMounter, mountPath string, image *oci.Image, shares []client.ShareMount) error {
	if mounter == nil {
		return fmt.Errorf("running instance does not support image mounts")
	}
	if err := mounter.AddImage(ctx, mountPath, image); err != nil {
		return err
	}
	return AddRuntimeShares(ctx, inst, RebaseRuntimeShares(mountPath, shares))
}

func ImageFSBackend(image *oci.Image) virtio.FSBackend {
	if image == nil {
		return nil
	}
	return virtio.NewImageFS(image.RootFS, image.RootFSDir)
}

func AddRuntimeShareMount(rootFS virtio.ShareMounter, mu *sync.Mutex, shares *map[string]client.ShareMount, share client.ShareMount, unsupportedFeature string, build func(client.ShareMount) (virtio.ShareMount, error)) error {
	if rootFS == nil {
		if strings.TrimSpace(unsupportedFeature) == "" {
			unsupportedFeature = "shares"
		}
		return fmt.Errorf("instance rootfs does not support %s", unsupportedFeature)
	}
	canonical, err := CanonicalRuntimeShare(share)
	if err != nil {
		return err
	}
	share = canonical
	key := share.Mount
	if mu == nil {
		mu = &sync.Mutex{}
	}
	if shares == nil {
		local := map[string]client.ShareMount{}
		shares = &local
	}
	mu.Lock()
	if existing, ok := (*shares)[key]; ok {
		mu.Unlock()
		if existing == share {
			return nil
		}
		return fmt.Errorf("share mount %q already exists", key)
	}
	mu.Unlock()
	mount, err := build(share)
	if err != nil {
		return err
	}
	if err := rootFS.AddShare(mount); err != nil {
		closePreparedShareMounts([]virtio.ShareMount{mount})
		return err
	}
	mu.Lock()
	if *shares == nil {
		*shares = make(map[string]client.ShareMount)
	}
	(*shares)[key] = share
	mu.Unlock()
	return nil
}

func AddImageMount(rootFS virtio.ShareMounter, mu *sync.Mutex, mounts *map[string]string, mountPath string, image *oci.Image, backend virtio.FSBackend) error {
	if rootFS == nil {
		return fmt.Errorf("instance rootfs does not support image mounts")
	}
	if strings.TrimSpace(mountPath) == "" || !strings.HasPrefix(mountPath, "/") {
		return fmt.Errorf("image mount path must be absolute")
	}
	if image == nil || image.RootFS == nil || backend == nil {
		return fmt.Errorf("image root filesystem is not available")
	}
	if mu == nil {
		mu = &sync.Mutex{}
	}
	if mounts == nil {
		local := map[string]string{}
		mounts = &local
	}
	mu.Lock()
	if existing, ok := (*mounts)[mountPath]; ok {
		mu.Unlock()
		if existing == image.Name {
			return nil
		}
		return fmt.Errorf("image mount %q already exists", mountPath)
	}
	mu.Unlock()
	if err := rootFS.AddShare(virtio.ShareMount{
		GuestPath: mountPath,
		Backend:   backend,
		Writable:  true,
		CacheMode: "aggressive",
	}); err != nil {
		return err
	}
	mu.Lock()
	if *mounts == nil {
		*mounts = make(map[string]string)
	}
	(*mounts)[mountPath] = image.Name
	mu.Unlock()
	return nil
}
