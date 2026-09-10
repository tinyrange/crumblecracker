package oci

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	intcvmfs "github.com/tinyrange/crumblecracker/internal/core/cvmfs"
	"github.com/tinyrange/crumblecracker/internal/core/download"
	"github.com/tinyrange/crumblecracker/internal/core/fsmeta"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/simg"
	"github.com/tinyrange/crumblecracker/internal/protocol"
	"github.com/ulikunitz/xz"
)

const defaultRegistry = "https://registry-1.docker.io/v2"
const sharedCacheEnv = "CCX3_OCI_SHARED_CACHE_DIR"
const sharedCacheSchemaVersion = "3"

const (
	SourceKindOCI           = "oci"
	SourceKindSIMG          = "simg"
	SourceKindCVMFS         = "cvmfs"
	SourceKindDockerArchive = "docker-archive"
	SourceKindRootFSTar     = "rootfs-tar"
	SourceKindSaved         = "saved"
	SourceKindInternal      = "internal"
)

const internalScratchSource = "scratch"
const maxRegistryMetadataBytes int64 = 16 << 20
const defaultOCIBlobDownloadWorkers = 4
const defaultOCIBlobInactivityTimeout = 2 * time.Minute
const maximumOCIBlobRetryDelay = 30 * time.Second

var ociBlobRetryDelay = time.Second

type Store struct {
	root            string
	sharedCacheRoot string
	httpClient      *http.Client
	CVMFSActivity   func(int)

	mu          sync.Mutex
	downloading map[string]bool
	lastErr     map[string]error
	opened      map[string]*Image
	opening     map[string]*imageOpenCall
}

type imageOpenCall struct {
	done  chan struct{}
	image *Image
	err   error
}

type metadata struct {
	Name               string      `json:"name"`
	Source             string      `json:"source"`
	ResolvedSource     string      `json:"resolved_source,omitempty"`
	SourceKind         string      `json:"source_kind,omitempty"`
	CVMFSRootHash      string      `json:"cvmfs_root_hash,omitempty"`
	CVMFSMirrors       []string    `json:"cvmfs_mirrors,omitempty"`
	Architecture       string      `json:"architecture,omitempty"`
	RootFSDir          string      `json:"rootfs_dir"`
	MetadataPath       string      `json:"metadata_path,omitempty"`
	IndexPath          string      `json:"index_path,omitempty"`
	PackedContentsPath string      `json:"packed_contents_path,omitempty"`
	Env                []string    `json:"env,omitempty"`
	Entrypoint         []string    `json:"entrypoint,omitempty"`
	Cmd                []string    `json:"cmd,omitempty"`
	WorkingDir         string      `json:"working_dir,omitempty"`
	User               string      `json:"user,omitempty"`
	Labels             []labelPair `json:"labels,omitempty"`
}

type labelPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Image struct {
	Name         string
	Source       string
	SourceKind   string
	Architecture string
	RootFSDir    string
	FSMetadata   map[string]fsmeta.Entry
	RootFS       imagefs.Directory
	Config       RuntimeConfig
}

type SourceSpec struct {
	Kind string
	Raw  string
}

type PullOptions struct {
	Architecture         string
	Prefetch             bool
	PrefetchWorkers      int
	CVMFSMirrors         []string
	Report               func(client.ProgressEvent)
	Refresh              bool
	KeepCompressedLayers bool
}

type SaveOptions struct {
	Source       string
	Architecture string
	Config       RuntimeConfig
}

func reportPullProgress(report func(client.ProgressEvent), event client.ProgressEvent) {
	if report == nil {
		return
	}
	report(event)
}

func ratio(current, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return min(1, max(0, float64(current)/float64(total)))
}

func normalizedMirrors(mirrors []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(mirrors))
	for _, mirror := range mirrors {
		mirror = strings.TrimRight(strings.TrimSpace(mirror), "/")
		if mirror == "" || seen[mirror] {
			continue
		}
		seen[mirror] = true
		out = append(out, mirror)
	}
	return out
}

type RuntimeConfig struct {
	Env        []string
	Entrypoint []string
	Cmd        []string
	WorkingDir string
	User       string
	Labels     map[string]string
}

type manifestList struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []manifestEntry `json:"manifests"`
}

type manifestEntry struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Platform  platform `json:"platform"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size,omitempty"`
}

type imageConfig struct {
	Architecture string `json:"architecture"`
	Config       struct {
		Env        []string          `json:"Env"`
		Entrypoint stringSlice       `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		WorkingDir string            `json:"WorkingDir"`
		User       string            `json:"User"`
		Labels     map[string]string `json:"Labels"`
	} `json:"config"`
}

type stringSlice []string

func (s *stringSlice) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	switch trimmed {
	case "", "null":
		*s = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []string
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		*s = arr
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}
	*s = []string{single}
	return nil
}

type registryContext struct {
	client   *http.Client
	registry string
	tokenMu  sync.RWMutex
	token    string
}

type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

func NewStore(root string) *Store {
	return &Store{
		root:        root,
		httpClient:  http.DefaultClient,
		downloading: map[string]bool{},
		lastErr:     map[string]error{},
		opened:      map[string]*Image{},
		opening:     map[string]*imageOpenCall{},
	}
}

func NewStoreWithSharedCache(root, sharedRoot string) *Store {
	store := NewStore(root)
	store.sharedCacheRoot = strings.TrimSpace(sharedRoot)
	return store
}

func (s *Store) newSharedStore() *Store {
	root := s.sharedRoot()
	shared := NewStoreWithSharedCache(root, root)
	shared.httpClient = s.httpClient
	return shared
}

func (s *Store) Root() string {
	return s.root
}

func (s *Store) List() ([]client.ImageState, error) {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, fmt.Errorf("create image store: %w", err)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("read image store: %w", err)
	}
	ret := make([]client.ImageState, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := s.Get(entry.Name())
		if err != nil {
			continue
		}
		ret = append(ret, state)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].Name < ret[j].Name })
	return ret, nil
}

func (s *Store) Get(name string) (client.ImageState, error) {
	if isScratchImageName(name) {
		if err := s.EnsureInternalScratch(context.Background(), name, ""); err != nil {
			return client.ImageState{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(name)
}

func (s *Store) Delete(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("image name is required")
	}
	if err := validateImageStoreName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downloading[name] {
		return fmt.Errorf("image %q download already in progress", name)
	}
	if _, err := s.getLocked(name); err != nil {
		return err
	}
	if err := os.RemoveAll(s.imageDir(name)); err != nil {
		return fmt.Errorf("remove image %q: %w", name, err)
	}
	delete(s.lastErr, name)
	delete(s.opened, name)
	return nil
}

func (s *Store) Open(name string) (*Image, error) {
	if isScratchImageName(name) {
		if err := s.EnsureInternalScratch(context.Background(), name, ""); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if image := s.opened[name]; image != nil {
		s.mu.Unlock()
		return cloneImage(image), nil
	}
	if call := s.opening[name]; call != nil {
		s.mu.Unlock()
		<-call.done
		return cloneImage(call.image), call.err
	}
	call := &imageOpenCall{done: make(chan struct{})}
	s.opening[name] = call
	s.mu.Unlock()

	image, err := s.openUncached(name)
	s.mu.Lock()
	call.image, call.err = image, err
	if err == nil {
		s.opened[name] = image
	}
	delete(s.opening, name)
	close(call.done)
	s.mu.Unlock()
	return cloneImage(image), err
}

func (s *Store) openUncached(name string) (*Image, error) {
	meta, err := s.readMetadata(name)
	if err != nil {
		return nil, err
	}
	var entries map[string]fsmeta.Entry
	if meta.MetadataPath != "" {
		buf, err := os.ReadFile(meta.MetadataPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read fs metadata: %w", err)
		}
		if len(buf) > 0 {
			if err := json.Unmarshal(buf, &entries); err != nil {
				return nil, fmt.Errorf("decode fs metadata: %w", err)
			}
		}
	}
	rootFS := imagefs.NewHostFS(meta.RootFSDir, entries)
	if meta.IndexPath != "" {
		indexBuf, err := os.ReadFile(meta.IndexPath)
		if err != nil {
			return nil, fmt.Errorf("read fs index: %w", err)
		}
		index, err := decodeFSIndex(indexBuf)
		if err != nil {
			return nil, fmt.Errorf("decode fs index: %w", err)
		}
		if meta.SourceKind == SourceKindCVMFS {
			cvmfsClient := &intcvmfs.Client{
				HTTPClient: s.httpClient,
				CacheDir:   cvmfsCacheDir(s.sharedRoot()),
				OnActivity: s.cvmfsActivity,
				Mirrors:    meta.CVMFSMirrors,
			}
			rootFS, err = buildCVMFSIndexedRootFS(cvmfsClient, meta.PackedContentsPath, index)
			if err != nil {
				return nil, fmt.Errorf("build cvmfs rootfs: %w", err)
			}
		} else {
			rootFS, err = buildIndexedRootFS(meta.RootFSDir, index)
			if err != nil {
				return nil, fmt.Errorf("build indexed rootfs: %w", err)
			}
		}
		return &Image{
			Name:         meta.Name,
			Source:       meta.Source,
			SourceKind:   meta.SourceKind,
			Architecture: meta.Architecture,
			RootFSDir:    meta.RootFSDir,
			FSMetadata:   entries,
			RootFS:       rootFS,
			Config: RuntimeConfig{
				Env:        append([]string(nil), meta.Env...),
				Entrypoint: append([]string(nil), meta.Entrypoint...),
				Cmd:        append([]string(nil), meta.Cmd...),
				WorkingDir: meta.WorkingDir,
				User:       meta.User,
				Labels:     labelsFromPairs(meta.Labels),
			},
		}, nil
	}
	if meta.SourceKind == SourceKindSIMG {
		rootFS, entries, arch, err := simg.BuildImageFS(filepath.Join(meta.RootFSDir, "rootfs.simg"))
		if err != nil {
			return nil, fmt.Errorf("build simg rootfs: %w", err)
		}
		return &Image{
			Name:         meta.Name,
			Source:       meta.Source,
			SourceKind:   meta.SourceKind,
			Architecture: firstNonEmpty(meta.Architecture, arch),
			RootFSDir:    meta.RootFSDir,
			FSMetadata:   entries,
			RootFS:       rootFS,
			Config: RuntimeConfig{
				Env:        append([]string(nil), meta.Env...),
				Entrypoint: append([]string(nil), meta.Entrypoint...),
				Cmd:        append([]string(nil), meta.Cmd...),
				WorkingDir: meta.WorkingDir,
				User:       meta.User,
				Labels:     labelsFromPairs(meta.Labels),
			},
		}, nil
	}
	return &Image{
		Name:         meta.Name,
		Source:       meta.Source,
		SourceKind:   meta.SourceKind,
		Architecture: meta.Architecture,
		RootFSDir:    meta.RootFSDir,
		FSMetadata:   entries,
		RootFS:       rootFS,
		Config: RuntimeConfig{
			Env:        append([]string(nil), meta.Env...),
			Entrypoint: append([]string(nil), meta.Entrypoint...),
			Cmd:        append([]string(nil), meta.Cmd...),
			WorkingDir: meta.WorkingDir,
			User:       meta.User,
			Labels:     labelsFromPairs(meta.Labels),
		},
	}, nil
}

func cloneImage(image *Image) *Image {
	if image == nil {
		return nil
	}
	clone := *image
	clone.Config.Env = append([]string(nil), image.Config.Env...)
	clone.Config.Entrypoint = append([]string(nil), image.Config.Entrypoint...)
	clone.Config.Cmd = append([]string(nil), image.Config.Cmd...)
	if image.Config.Labels != nil {
		clone.Config.Labels = make(map[string]string, len(image.Config.Labels))
		for key, value := range image.Config.Labels {
			clone.Config.Labels[key] = value
		}
	}
	return &clone
}

func (s *Store) Pull(ctx context.Context, name, source string, options ...PullOptions) (client.ImageState, error) {
	if name == "" {
		return client.ImageState{}, fmt.Errorf("image name is required")
	}
	if source == "" {
		return client.ImageState{}, fmt.Errorf("image source is required")
	}
	spec, err := ParseSource(source)
	if err != nil {
		return client.ImageState{}, err
	}
	var opts PullOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if spec.Kind == SourceKindInternal && spec.Raw == internalScratchSource {
		if err := s.EnsureInternalScratch(ctx, name, opts.Architecture); err != nil {
			return client.ImageState{}, err
		}
		reportPullProgress(opts.Report, client.ProgressEvent{Status: "available", Artifact: name})
		reportPullProgress(opts.Report, client.ProgressEvent{Status: "downloaded", Artifact: name})
		return s.Get(name)
	}
	if !opts.Refresh {
		if state, ok, err := s.existingState(ctx, name, spec, opts.Architecture); err != nil {
			return client.ImageState{}, err
		} else if ok {
			reportPullProgress(opts.Report, client.ProgressEvent{Status: "available", Artifact: name})
			if err := s.maybePrefetchCVMFSImage(ctx, name, spec, opts); err != nil {
				return client.ImageState{}, err
			}
			reportPullProgress(opts.Report, client.ProgressEvent{Status: "downloaded", Artifact: name})
			return state, nil
		}
		if state, ok, err := s.restoreFromSharedCache(ctx, name, spec, opts.Architecture); err != nil {
			return client.ImageState{}, err
		} else if ok {
			reportPullProgress(opts.Report, client.ProgressEvent{Status: "restored", Artifact: name})
			if err := s.maybePrefetchCVMFSImage(ctx, name, spec, opts); err != nil {
				return client.ImageState{}, err
			}
			reportPullProgress(opts.Report, client.ProgressEvent{Status: "downloaded", Artifact: name})
			return state, nil
		}
	}

	s.mu.Lock()
	if s.downloading[name] {
		s.mu.Unlock()
		return client.ImageState{}, fmt.Errorf("image %q download already in progress", name)
	}
	s.downloading[name] = true
	delete(s.lastErr, name)
	delete(s.opened, name)
	s.mu.Unlock()

	err = s.pull(ctx, name, spec, opts)

	s.mu.Lock()
	delete(s.downloading, name)
	s.lastErr[name] = err
	state, stateErr := s.getLocked(name)
	s.mu.Unlock()

	if err != nil {
		return client.ImageState{}, err
	}
	if stateErr != nil {
		return state, stateErr
	}
	reportPullProgress(opts.Report, client.ProgressEvent{Status: "downloaded", Artifact: name})
	return state, nil
}

// ActivateStaged replaces a named image with an already prepared image in the
// same store. Store readers see either the old or new image because activation
// is serialized by the store lock. Callers must ensure no running VM still
// uses name.
func (s *Store) ActivateStaged(name, stagedName string) (client.ImageState, error) {
	if name == "" || stagedName == "" || name == stagedName {
		return client.ImageState{}, fmt.Errorf("distinct active and staged image names are required")
	}
	stagedMeta, err := s.readMetadata(stagedName)
	if err != nil {
		return client.ImageState{}, fmt.Errorf("read staged image: %w", err)
	}
	spec := SourceSpec{Kind: stagedMeta.SourceKind, Raw: stagedMeta.Source}
	if spec.Kind == "" || spec.Raw == "" {
		return client.ImageState{}, fmt.Errorf("staged image metadata is incomplete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downloading[name] || s.downloading[stagedName] {
		return client.ImageState{}, fmt.Errorf("image preparation is still in progress")
	}
	delete(s.opened, name)
	if err := s.cloneFromStore(s, stagedName, name, spec); err != nil {
		return client.ImageState{}, err
	}
	state, err := s.getLocked(name)
	if err != nil {
		return client.ImageState{}, err
	}
	// The stage is no longer addressable through a client while the store is
	// locked. Removing it directly avoids recursively taking s.mu.
	delete(s.opened, stagedName)
	delete(s.lastErr, stagedName)
	_ = os.RemoveAll(s.imageDir(stagedName))
	return state, nil
}

func (s *Store) PlanPull(ctx context.Context, name, source string, options ...PullOptions) (client.ImagePullPlan, error) {
	if name == "" {
		return client.ImagePullPlan{}, fmt.Errorf("image name is required")
	}
	if source == "" {
		return client.ImagePullPlan{}, fmt.Errorf("image source is required")
	}
	spec, err := ParseSource(source)
	if err != nil {
		return client.ImagePullPlan{}, err
	}
	var opts PullOptions
	if len(options) > 0 {
		opts = options[0]
	}
	ctx = context.WithValue(ctx, metadataProgressKey{}, opts.Report)
	architecture := firstNonEmpty(normalizeArchitecture(opts.Architecture), nativeArch())
	plan := client.ImagePullPlan{
		Name:         name,
		Source:       source,
		Architecture: architecture,
	}
	sharedName := sharedImageKey(spec, opts.Architecture)
	shared := s.newSharedStore()
	plan.Installed = pullPlanImageInstalled(s, name, spec, architecture)
	if spec.Kind != SourceKindOCI {
		if _, ok, err := s.existingState(ctx, name, spec, opts.Architecture); err != nil {
			return client.ImagePullPlan{}, err
		} else if ok {
			plan.Available = true
			return plan, nil
		}
		if _, ok, err := shared.existingState(ctx, sharedName, spec, opts.Architecture); err != nil {
			return client.ImagePullPlan{}, err
		} else if ok {
			plan.Available = true
		}
		return plan, nil
	}

	registry, imageName, tag, err := ParseImageRef(spec.Raw)
	if err != nil {
		return client.ImagePullPlan{}, err
	}
	reg := &registryContext{client: shared.httpClient, registry: registry}
	mani, manifestDigest, err := shared.fetchManifest(ctx, reg, imageName, tag, preferredManifestArchitectures(opts.Architecture)...)
	if err != nil {
		return client.ImagePullPlan{}, err
	}
	if manifestDigest != "" {
		plan.ResolvedSource = resolvedOCISource(registry, imageName, manifestDigest)
	}
	if pullPlanMetadataMatches(s, name, spec, architecture, plan.ResolvedSource) ||
		pullPlanMetadataMatches(shared, sharedName, spec, architecture, plan.ResolvedSource) {
		plan.Available = true
		return plan, nil
	}
	seen := make(map[string]struct{}, len(mani.Layers))
	for _, layer := range mani.Layers {
		if layer.Size <= 0 {
			return client.ImagePullPlan{}, fmt.Errorf("layer %s has invalid size %d", layer.Digest, layer.Size)
		}
		if _, duplicate := seen[layer.Digest]; duplicate {
			continue
		}
		seen[layer.Digest] = struct{}{}
		plan.LayersTotal++
		plan.BytesTotal += layer.Size
		blobPath := filepath.Join(shared.root, "_blobs", digestToFileName(layer.Digest))
		if shared.cachedLayerArchiveAvailable(layer) || shared.cachedStargzLayerAvailable(layer) {
			plan.LayersCached++
			plan.BytesCached += layer.Size
		} else if info, statErr := os.Stat(blobPath); statErr == nil && info.Mode().IsRegular() && info.Size() == layer.Size {
			plan.LayersCached++
			plan.BytesCached += layer.Size
		} else if info, statErr := os.Stat(blobPath + ".partial"); statErr == nil && info.Mode().IsRegular() {
			// A normal partial blob is a verified prefix that cacheLayerBlob will
			// resume. Delta partials are intentionally excluded because they are
			// sparse, target-sized assembly files rather than downloaded prefixes.
			plan.BytesCached += min(layer.Size, max(int64(0), info.Size()))
		}
	}
	plan.BytesToDownload = max(0, plan.BytesTotal-plan.BytesCached)
	return plan, nil
}

func pullPlanImageInstalled(store *Store, name string, spec SourceSpec, architecture string) bool {
	meta, err := store.readMetadata(name)
	if err != nil || meta.Source != spec.Raw || meta.SourceKind != spec.Kind || !dirExists(meta.RootFSDir) {
		return false
	}
	return architecture == "" || normalizeArchitecture(meta.Architecture) == normalizeArchitecture(architecture)
}

func pullPlanMetadataMatches(store *Store, name string, spec SourceSpec, architecture, resolvedSource string) bool {
	if resolvedSource == "" {
		return false
	}
	meta, err := store.readMetadata(name)
	if err != nil || meta.ResolvedSource != resolvedSource {
		return false
	}
	return pullPlanImageInstalled(store, name, spec, architecture)
}

func (s *Store) pull(ctx context.Context, name string, spec SourceSpec, options PullOptions) error {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("create image store: %w", err)
	}
	if err := os.MkdirAll(s.sharedRoot(), 0o755); err != nil {
		return fmt.Errorf("create shared image store: %w", err)
	}

	sharedName := sharedImageKey(spec, options.Architecture)
	shared := s.newSharedStore()
	if options.Refresh {
		if err := shared.pullDirect(ctx, sharedName, spec, options); err != nil {
			return err
		}
	} else if _, ok, err := shared.existingState(ctx, sharedName, spec, options.Architecture); err != nil {
		return err
	} else if !ok {
		if err := shared.pullDirect(ctx, sharedName, spec, options); err != nil {
			return err
		}
	}
	return s.cloneFromStore(shared, sharedName, name, spec)
}

func (s *Store) pullDirect(ctx context.Context, name string, spec SourceSpec, options PullOptions) error {
	switch spec.Kind {
	case SourceKindOCI:
		return s.pullOCIDirect(ctx, name, spec, options)
	case SourceKindSIMG:
		return s.pullSIMGDirect(ctx, name, spec)
	case SourceKindRootFSTar:
		return s.pullRootFSTarDirect(ctx, name, spec, options)
	case SourceKindCVMFS:
		return s.pullCVMFSDirect(ctx, name, spec, options)
	case SourceKindDockerArchive:
		return s.pullDockerArchiveDirect(ctx, name, spec)
	case SourceKindInternal:
		if spec.Raw == internalScratchSource {
			return s.ensureInternalScratch(ctx, name, options.Architecture)
		}
		return fmt.Errorf("unsupported internal image source %q", spec.Raw)
	default:
		return fmt.Errorf("unsupported image source kind %q", spec.Kind)
	}
}

func (s *Store) pullRootFSTarDirect(ctx context.Context, name string, spec SourceSpec, options PullOptions) error {
	reportPullProgress(options.Report, client.ProgressEvent{Status: "resolving", Artifact: name, Blob: "rootfs"})
	imageDir := s.imageDir(name)
	tmpDir := imageDir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("remove temp image dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "layers"), 0o755); err != nil {
		return fmt.Errorf("create temp image dir: %w", err)
	}
	layerTarRel := filepath.Join("layers", "rootfs.tar")
	layerTarPath := filepath.Join(tmpDir, layerTarRel)
	reportPullProgress(options.Report, client.ProgressEvent{Status: "downloading", Artifact: name, Blob: "rootfs"})
	if err := s.fetchRootFSTar(ctx, name, strings.TrimPrefix(spec.Raw, "rootfs-tar:"), layerTarPath, options.Report); err != nil {
		return err
	}
	reportPullProgress(options.Report, client.ProgressEvent{Status: "indexing", Artifact: name, Blob: "rootfs"})
	build := newIndexedBuildState()
	if err := applyIndexedLayer(layerTarPath, layerTarRel, build.merged, build.fsEntries); err != nil {
		return fmt.Errorf("index rootfs tar: %w", err)
	}
	cfg := imageConfig{Architecture: firstNonEmpty(normalizeArchitecture(options.Architecture), nativeArch())}
	cfg.Config.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	cfg.Config.WorkingDir = "/"
	if err := s.finalizeIndexedImage(name, spec, "", imageDir, tmpDir, cfg, build); err != nil {
		return err
	}
	return nil
}

func (s *Store) pullSIMGDirect(ctx context.Context, name string, spec SourceSpec) error {
	imageDir := s.imageDir(name)
	tmpDir := imageDir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("remove temp image dir: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("create temp image dir: %w", err)
	}
	simgPath := filepath.Join(tmpDir, "rootfs.simg")
	if err := s.fetchSIMG(ctx, spec.Raw, simgPath); err != nil {
		return err
	}
	return s.finalizeSIMGImage(name, spec, imageDir, tmpDir, simgPath)
}

func (s *Store) pullCVMFSDirect(ctx context.Context, name string, spec SourceSpec, options PullOptions) error {
	reportPullProgress(options.Report, client.ProgressEvent{Status: "resolving", Artifact: name, Blob: "cvmfs"})
	imageDir := s.imageDir(name)
	tmpDir := imageDir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("remove temp image dir: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("create temp image dir: %w", err)
	}
	cvmfsClient := &intcvmfs.Client{
		HTTPClient: s.httpClient,
		Context:    ctx,
		CacheDir:   cvmfsCacheDir(s.sharedRoot()),
		OnActivity: s.cvmfsActivity,
		Mirrors:    options.CVMFSMirrors,
	}
	defer func() { cvmfsClient.Context = nil }()
	normalizedSource := normalizeCVMFSSource(spec.Raw)
	rootTarget, isDir, err := resolveCVMFSRootTarget(cvmfsClient, normalizedSource)
	if err != nil {
		return err
	}
	if !isDir {
		return fmt.Errorf("resolve cvmfs container root: %q is not a container directory", spec.Raw)
	}
	rootHash, err := cvmfsClient.ManifestRootHash(normalizedSource)
	if err != nil {
		return fmt.Errorf("read cvmfs manifest root hash: %w", err)
	}
	reportPullProgress(options.Report, client.ProgressEvent{Status: "indexing", Artifact: name, Blob: rootHash})
	nodes, entries, arch, ok, err := loadCVMFSDirectoryIndexCache(cvmfsClient.CacheDir, rootHash, rootTarget)
	if err != nil {
		return fmt.Errorf("load cached cvmfs rootfs index: %w", err)
	}
	if !ok {
		nodes, entries, arch, err = buildCVMFSDirectoryIndex(cvmfsClient, rootTarget)
		if err != nil {
			return fmt.Errorf("index cvmfs rootfs: %w", err)
		}
		if err := saveCVMFSDirectoryIndexCache(cvmfsClient.CacheDir, rootHash, rootTarget, nodes, entries, arch); err != nil {
			return fmt.Errorf("cache cvmfs rootfs index: %w", err)
		}
	}
	if options.Prefetch {
		cachedNodes, err := prefetchCVMFSFiles(ctx, cvmfsClient, nodes, options.PrefetchWorkers, name, options.Report)
		if err != nil {
			return fmt.Errorf("prefetch cvmfs rootfs: %w", err)
		}
		nodes = cachedNodes
	}
	fsMetaBuf, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal fs metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "rootfs.metadata.json"), fsMetaBuf, 0o644); err != nil {
		return fmt.Errorf("write fs metadata: %w", err)
	}
	indexBuf, err := encodeIndexedNodes(nodes)
	if err != nil {
		return fmt.Errorf("marshal fs index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "rootfs.index.json"), indexBuf, 0o644); err != nil {
		return fmt.Errorf("write fs index: %w", err)
	}
	deployMetadata, err := extractCVMFSDeployMetadata(cvmfsClient, "", nodes)
	if err != nil {
		return fmt.Errorf("extract cvmfs deploy metadata: %w", err)
	}
	meta := metadata{
		Name:          name,
		Source:        spec.Raw,
		SourceKind:    spec.Kind,
		CVMFSRootHash: rootHash,
		CVMFSMirrors:  normalizedMirrors(options.CVMFSMirrors),
		Architecture:  arch,
		RootFSDir:     imageDir,
		MetadataPath:  filepath.Join(imageDir, "rootfs.metadata.json"),
		IndexPath:     filepath.Join(imageDir, "rootfs.index.json"),
		Env:           deployMetadata.Env,
	}
	if err := os.RemoveAll(imageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old image dir: %w", err)
	}
	if err := os.Rename(tmpDir, imageDir); err != nil {
		return fmt.Errorf("activate image dir: %w", err)
	}
	if err := s.writeMetadata(name, meta); err != nil {
		return err
	}
	return nil
}

func extractCVMFSDeployMetadata(client *intcvmfs.Client, packedPath string, nodes []indexedNode) (simgDeployMetadata, error) {
	var envTexts []string
	var buildYAML string
	sorted := append([]indexedNode(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, node := range sorted {
		if node.Kind != indexedKindFile {
			continue
		}
		isEnvFile := strings.HasPrefix(node.Path, "/.singularity.d/env/") && strings.HasSuffix(node.Path, ".sh")
		if !isEnvFile && node.Path != "/build.yaml" {
			continue
		}
		text, err := readCVMFSIndexedNodeText(client, packedPath, node)
		if err != nil {
			return simgDeployMetadata{}, err
		}
		if isEnvFile {
			envTexts = append(envTexts, text)
			continue
		}
		buildYAML = text
	}
	return extractDeployMetadataTexts(envTexts, buildYAML), nil
}

func readCVMFSIndexedNodeText(client *intcvmfs.Client, packedPath string, node indexedNode) (string, error) {
	if node.Size > maxSIMGMetadataFileSize {
		node.Size = maxSIMGMetadataFileSize
	}
	if node.Packed {
		if packedPath == "" {
			return "", fmt.Errorf("packed path is required for %q", node.Path)
		}
		file, err := os.Open(packedPath)
		if err != nil {
			return "", err
		}
		defer file.Close()
		buf := make([]byte, node.Size)
		n, err := file.ReadAt(buf, int64(node.PackedOffset))
		if err != nil && err != io.EOF {
			return "", err
		}
		return string(buf[:n]), nil
	}
	if node.CVMFSTarget == "" {
		return "", nil
	}
	if client == nil {
		return "", fmt.Errorf("cvmfs client is required for %q", node.Path)
	}
	data, err := client.ReadFile(node.CVMFSTarget)
	if err != nil {
		return "", err
	}
	if len(data) > maxSIMGMetadataFileSize {
		data = data[:maxSIMGMetadataFileSize]
	}
	return string(data), nil
}

func (s *Store) finalizeSIMGImage(name string, spec SourceSpec, imageDir, tmpDir, simgPath string) error {
	rootFS, entries, arch, err := simg.BuildImageFS(simgPath)
	if err != nil {
		return fmt.Errorf("index simg: %w", err)
	}
	deployMetadata := extractSIMGDeployMetadata(rootFS)
	metadataPath := filepath.Join(imageDir, "rootfs.metadata.json")
	fsMetaBuf, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal fs metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "rootfs.metadata.json"), fsMetaBuf, 0o644); err != nil {
		return fmt.Errorf("write fs metadata: %w", err)
	}
	meta := metadata{
		Name:         name,
		Source:       spec.Raw,
		SourceKind:   spec.Kind,
		Architecture: arch,
		RootFSDir:    imageDir,
		MetadataPath: metadataPath,
		Env:          deployMetadata.Env,
	}
	if err := os.RemoveAll(imageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old image dir: %w", err)
	}
	if err := os.Rename(tmpDir, imageDir); err != nil {
		return fmt.Errorf("activate image dir: %w", err)
	}
	if err := s.writeMetadata(name, meta); err != nil {
		return err
	}
	return nil
}

func (s *Store) maybePrefetchCVMFSImage(ctx context.Context, name string, spec SourceSpec, options PullOptions) error {
	if spec.Kind != SourceKindCVMFS || !options.Prefetch {
		return nil
	}
	meta, err := s.readMetadata(name)
	if err != nil {
		return fmt.Errorf("read image metadata for prefetch: %w", err)
	}
	if meta.IndexPath == "" {
		return nil
	}
	indexBuf, err := os.ReadFile(meta.IndexPath)
	if err != nil {
		return fmt.Errorf("read fs index for prefetch: %w", err)
	}
	nodes, err := decodeFSIndex(indexBuf)
	if err != nil {
		return fmt.Errorf("decode fs index for prefetch: %w", err)
	}
	cvmfsClient := &intcvmfs.Client{
		HTTPClient: s.httpClient,
		Context:    ctx,
		CacheDir:   cvmfsCacheDir(s.sharedRoot()),
		OnActivity: s.cvmfsActivity,
		Mirrors:    options.CVMFSMirrors,
	}
	cachedNodes, err := prefetchCVMFSFiles(ctx, cvmfsClient, nodes, options.PrefetchWorkers, name, options.Report)
	if err != nil {
		return err
	}
	indexBuf, err = encodeIndexedNodes(cachedNodes)
	if err != nil {
		return fmt.Errorf("marshal fs index after prefetch: %w", err)
	}
	if err := os.WriteFile(meta.IndexPath, indexBuf, 0o644); err != nil {
		return fmt.Errorf("write fs index after prefetch: %w", err)
	}
	if meta.PackedContentsPath != "" {
		_ = os.Remove(meta.PackedContentsPath)
		meta.PackedContentsPath = ""
	}
	return s.writeMetadata(name, meta)
}

func normalizeCVMFSSource(source string) string {
	lower := strings.ToLower(source)
	if strings.HasPrefix(lower, "http+cvmfs://") {
		u, err := url.Parse("https://" + source[len("http+cvmfs://"):])
		if err == nil {
			queryPath := strings.TrimSpace(u.Query().Get("path"))
			if queryPath != "" {
				u.RawQuery = ""
				u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimPrefix(path.Clean("/"+queryPath), "/")
				return u.String()
			}
		}
		return "https://" + source[len("http+cvmfs://"):]
	}
	return source
}

func (s *Store) fetchSIMG(ctx context.Context, source, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create simg dir: %w", err)
	}
	tmpPath := destPath + ".tmp"
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return err
		}
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("download simg: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download simg: status %s", resp.Status)
		}
		if err := download.BoundResponse(resp, 0); err != nil {
			return fmt.Errorf("download simg: %w", err)
		}
		dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := download.Copy(ctx, dst, resp, download.Budget{MaxBytes: resp.ContentLength, ExpectedBytes: resp.ContentLength}); err != nil {
			_ = dst.Close()
			_ = os.Remove(tmpPath)
			return err
		}
		if err := dst.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
		return os.Rename(tmpPath, destPath)
	}
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("stat simg source: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("simg source must be a file")
	}
	if err := copyFile(source, tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy simg source: %w", err)
	}
	return os.Rename(tmpPath, destPath)
}

func (s *Store) fetchRootFSTar(ctx context.Context, name, source, destPath string, report func(client.ProgressEvent)) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create rootfs tar dir: %w", err)
	}
	tmpPath := destPath + ".tmp"
	var src io.ReadCloser
	totalBytes := int64(0)
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return err
		}
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("download rootfs tar: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return fmt.Errorf("download rootfs tar: status %s (%s)", resp.Status, strings.TrimSpace(string(body)))
		}
		if err := download.BoundResponse(resp, 0); err != nil {
			resp.Body.Close()
			return fmt.Errorf("download rootfs tar: %w", err)
		}
		src = resp.Body
		if resp.ContentLength > 0 {
			totalBytes = resp.ContentLength
		}
	} else {
		file, err := os.Open(source)
		if err != nil {
			return fmt.Errorf("open rootfs tar source: %w", err)
		}
		src = file
		if info, err := file.Stat(); err == nil && info.Size() > 0 {
			totalBytes = info.Size()
		}
	}
	defer src.Close()
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	progress := newDownloadProgressReader(src, name, "rootfs", totalBytes, report)
	expandedBudget, err := download.FilesystemBudget(destPath)
	if err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("determine rootfs expansion budget: %w", err)
	}
	budgetedDst, err := download.NewLimitWriter(dst, expandedBudget)
	if err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := writeUncompressedTar(budgetedDst, source, progress); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	progress.finish()
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, destPath)
}

func (s *Store) pullOCIDirect(ctx context.Context, name string, spec SourceSpec, options PullOptions) error {
	var reportMu sync.Mutex
	report := func(event client.ProgressEvent) {
		reportMu.Lock()
		defer reportMu.Unlock()
		reportPullProgress(options.Report, event)
	}
	ctx = context.WithValue(ctx, metadataProgressKey{}, report)
	report(client.ProgressEvent{Status: "resolving", Artifact: name, Blob: "manifest"})
	registry, imageName, tag, err := ParseImageRef(spec.Raw)
	if err != nil {
		return err
	}
	reg := &registryContext{client: s.httpClient, registry: registry}

	imageDir := s.imageDir(name)
	tmpDir := imageDir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("remove temp image dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "blobs"), 0o755); err != nil {
		return fmt.Errorf("create temp image dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "layers"), 0o755); err != nil {
		return fmt.Errorf("create layers dir: %w", err)
	}

	mani, manifestDigest, err := s.fetchManifest(ctx, reg, imageName, tag, preferredManifestArchitectures(options.Architecture)...)
	if err != nil {
		return err
	}

	cfgBlob, err := s.fetchBlob(ctx, reg, imageName, mani.Config)
	if err != nil {
		return fmt.Errorf("fetch config blob: %w", err)
	}
	var cfg imageConfig
	if err := json.Unmarshal(cfgBlob, &cfg); err != nil {
		return fmt.Errorf("decode image config: %w", err)
	}

	var totalLayerBytes int64
	for _, layer := range mani.Layers {
		totalLayerBytes += layer.Size
	}
	var pipelineMu sync.Mutex
	build := newIndexedBuildState()
	prepareStarted := time.Now()
	networkByLayer := make([]int64, len(mani.Layers))
	downloadedByLayer := make([]int64, len(mani.Layers))
	indexedByLayer := make([]int64, len(mani.Layers))
	downloadRateByLayer := make([]float64, len(mani.Layers))
	for layerIndex, layer := range mani.Layers {
		switch {
		case s.cachedLayerArchiveAvailable(layer) || s.cachedStargzLayerAvailable(layer):
			downloadedByLayer[layerIndex] = layer.Size
			indexedByLayer[layerIndex] = layer.Size
		case s.cachedLayerBlobAvailable(layer):
			downloadedByLayer[layerIndex] = layer.Size
		default:
			partialPath := filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest)) + ".partial"
			if info, statErr := os.Stat(partialPath); statErr == nil {
				downloadedByLayer[layerIndex] = min(layer.Size, max(0, info.Size()))
			}
		}
	}
	for i, layer := range mani.Layers {
		state := "queued"
		if downloadedByLayer[i] == layer.Size {
			state = "cached"
		}
		report(client.ProgressEvent{Artifact: name, Blob: layer.Digest, Transfer: &client.TransferProgress{ID: layer.Digest, Kind: "image_layer", State: state, Completed: downloadedByLayer[i], Total: layer.Size}})
	}
	report(client.ProgressEvent{Artifact: name, PlanningComplete: true})
	reportPipeline := func(layerIndex int, layer descriptor, downloaded, indexed *int64, downloadRate *float64) {
		pipelineMu.Lock()
		if downloaded != nil {
			if downloadRate != nil {
				networkByLayer[layerIndex] += max(0, *downloaded-downloadedByLayer[layerIndex])
			}
			downloadedByLayer[layerIndex] = min(layer.Size, max(0, *downloaded))
		}
		if indexed != nil {
			indexedByLayer[layerIndex] = min(layer.Size, max(0, *indexed))
		}
		if downloadRate != nil {
			downloadRateByLayer[layerIndex] = max(0, *downloadRate)
		}
		var downloadedBytes, indexedBytes, indexedLayers int64
		var rateBytesPerSecond float64
		for index := range mani.Layers {
			downloadedBytes += downloadedByLayer[index]
			indexedBytes += indexedByLayer[index]
			rateBytesPerSecond += downloadRateByLayer[index]
			if indexedByLayer[index] >= mani.Layers[index].Size {
				indexedLayers++
			}
		}
		downloadProgress := ratio(downloadedBytes, totalLayerBytes)
		indexProgress := ratio(indexedBytes, totalLayerBytes)
		progress := ratio(downloadedBytes+indexedBytes, totalLayerBytes*2)
		transferState := "downloading"
		if downloadedByLayer[layerIndex] >= layer.Size {
			transferState = "preparing"
		}
		if indexedByLayer[layerIndex] >= layer.Size {
			transferState = "ready"
		}
		transfer := &client.TransferProgress{ID: layer.Digest, Kind: "image_layer", State: transferState, Completed: downloadedByLayer[layerIndex], Total: layer.Size, NetworkBytes: networkByLayer[layerIndex]}
		if networkByLayer[layerIndex] == 0 && indexedByLayer[layerIndex] >= layer.Size {
			transfer.State = "cached"
		}
		pipelineMu.Unlock()
		elapsed := time.Since(prepareStarted).Seconds()
		var eta float64
		if progress > 0 && progress < 1 && elapsed > 0 {
			eta = elapsed * (1 - progress) / progress
		}
		status := "processing"
		if indexedBytes == 0 {
			status = "downloading"
		}
		report(client.ProgressEvent{
			Transfer:           transfer,
			Status:             status,
			Artifact:           name,
			Blob:               layer.Digest,
			Progress:           progress,
			DownloadProgress:   downloadProgress,
			IndexProgress:      indexProgress,
			BytesDownloaded:    downloadedBytes,
			BytesTotal:         totalLayerBytes,
			RateBytesPerSecond: rateBytesPerSecond,
			FilesDownloaded:    indexedLayers,
			FilesTotal:         int64(len(mani.Layers)),
			ETASeconds:         eta,
		})
	}
	prepareCtx, cancelPrepare := context.WithCancel(ctx)
	defer cancelPrepare()
	type layerPrepareResult struct {
		done chan struct{}
		err  error
	}
	prepareResults := make([]*layerPrepareResult, len(mani.Layers))
	resultsByDigest := make(map[string]*layerPrepareResult, len(mani.Layers))
	prepareJobs := make(chan int, len(mani.Layers))
	missingLayers := 0
	for layerIndex, layer := range mani.Layers {
		if s.cachedLayerArchiveAvailable(layer) || s.cachedStargzLayerAvailable(layer) {
			continue
		}
		if existing := resultsByDigest[layer.Digest]; existing != nil {
			prepareResults[layerIndex] = existing
			continue
		}
		result := &layerPrepareResult{done: make(chan struct{})}
		resultsByDigest[layer.Digest] = result
		prepareResults[layerIndex] = result
		prepareJobs <- layerIndex
		missingLayers++
	}
	close(prepareJobs)
	// Ordinary layers are decompressed and indexed while they download, so keep
	// that CPU and disk-heavy pipeline conservative. Enhanced layers remain
	// compressed and are predominantly independent registry I/O, where four
	// workers avoid idle gaps between layer and range requests.
	prepareWorkers := 2
	if options.KeepCompressedLayers {
		prepareWorkers = 4
	}
	for range min(prepareWorkers, missingLayers) {
		go func() {
			for layerIndex := range prepareJobs {
				layer := mani.Layers[layerIndex]
				err := s.ensureLayerArchive(
					prepareCtx,
					reg,
					imageName,
					name,
					layer,
					func(current int64, rate float64) {
						reportPipeline(layerIndex, layer, &current, nil, &rate)
					},
					func(current int64) {
						reportPipeline(layerIndex, layer, nil, &current, nil)
					},
					options.KeepCompressedLayers,
				)
				result := prepareResults[layerIndex]
				result.err = err
				close(result.done)
			}
		}()
	}

	for layerIndex, layer := range mani.Layers {
		if result := prepareResults[layerIndex]; result != nil {
			<-result.done
			if err := result.err; err != nil {
				cancelPrepare()
				return fmt.Errorf("prepare layer %s: %w", layer.Digest, err)
			}
		}
		layerSuffix := ".contents"
		if s.cachedStargzLayerAvailable(layer) {
			layerSuffix = ".stargz"
		}
		layerContentsRel := filepath.Join("layers", digestToFileName(layer.Digest)+layerSuffix)
		layerContentsPath := filepath.Join(tmpDir, layerContentsRel)
		if err := s.writeAndApplyCachedLayer(layer, layerContentsPath, layerContentsRel, build.merged, build.fsEntries, nil); err != nil {
			cancelPrepare()
			return fmt.Errorf("apply layer %s: %w", layer.Digest, err)
		}
		complete := layer.Size
		reportPipeline(layerIndex, layer, &complete, &complete, nil)
	}
	report(client.ProgressEvent{
		Status:           "processing",
		Artifact:         name,
		Blob:             "filesystem",
		Progress:         1,
		DownloadProgress: 1,
		IndexProgress:    1,
		BytesDownloaded:  totalLayerBytes,
		BytesTotal:       totalLayerBytes,
		FilesDownloaded:  int64(len(mani.Layers)),
		FilesTotal:       int64(len(mani.Layers)),
	})
	resolvedSource := ""
	if manifestDigest != "" {
		resolvedSource = resolvedOCISource(registry, imageName, manifestDigest)
	}
	return s.finalizeIndexedImage(name, spec, resolvedSource, imageDir, tmpDir, cfg, build)
}

type indexedBuildState struct {
	merged    map[string]*indexedNode
	fsEntries map[string]fsmeta.Entry
}

func newIndexedBuildState() indexedBuildState {
	rootMode := fsmeta.LinuxModeFromFileMode(os.ModeDir | 0o755)
	return indexedBuildState{
		merged: map[string]*indexedNode{
			"/": {
				Path: "/",
				Kind: indexedKindDir,
				Mode: rootMode,
				UID:  0,
				GID:  0,
			},
		},
	}
}

func (s *Store) finalizeIndexedImage(name string, spec SourceSpec, resolvedSource, imageDir, tmpDir string, cfg imageConfig, build indexedBuildState) error {
	ensureIndexedParents(build.merged, build.fsEntries)
	indexPath := filepath.Join(imageDir, "rootfs.index")
	indexBuf, err := encodeFSIndex(build.merged)
	if err != nil {
		return fmt.Errorf("marshal fs index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "rootfs.index"), indexBuf, 0o644); err != nil {
		return fmt.Errorf("write fs index: %w", err)
	}

	meta := metadata{
		Name:           name,
		Source:         spec.Raw,
		ResolvedSource: resolvedSource,
		SourceKind:     spec.Kind,
		Architecture:   cfg.Architecture,
		RootFSDir:      imageDir,
		IndexPath:      indexPath,
		Env:            append([]string(nil), cfg.Config.Env...),
		Entrypoint:     append([]string(nil), cfg.Config.Entrypoint...),
		Cmd:            append([]string(nil), cfg.Config.Cmd...),
		WorkingDir:     cfg.Config.WorkingDir,
		User:           cfg.Config.User,
		Labels:         labelPairsFromMap(cfg.Config.Labels),
	}

	if err := os.RemoveAll(imageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old image dir: %w", err)
	}
	if err := os.Rename(tmpDir, imageDir); err != nil {
		return fmt.Errorf("activate image dir: %w", err)
	}
	if err := s.writeMetadata(name, meta); err != nil {
		return err
	}
	return nil
}

func (s *Store) existingState(ctx context.Context, name string, spec SourceSpec, architecture string) (client.ImageState, bool, error) {
	meta, err := s.readMetadata(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return client.ImageState{}, false, nil
		}
		return client.ImageState{}, false, err
	}
	if meta.Source != spec.Raw || meta.SourceKind != spec.Kind {
		return client.ImageState{}, false, nil
	}
	if arch := normalizeArchitecture(architecture); arch != "" && meta.Architecture != arch {
		return client.ImageState{}, false, nil
	}
	if spec.Kind == SourceKindCVMFS {
		if meta.CVMFSRootHash == "" {
			return client.ImageState{}, false, nil
		}
		currentHash, err := s.currentCVMFSRootHash(ctx, spec, meta.CVMFSMirrors)
		if err != nil {
			return client.ImageState{}, false, err
		}
		if currentHash != meta.CVMFSRootHash {
			return client.ImageState{}, false, nil
		}
	}
	if !dirExists(meta.RootFSDir) {
		return client.ImageState{}, false, nil
	}
	return client.ImageState{Name: meta.Name, Source: meta.Source, ResolvedSource: meta.ResolvedSource, SourceKind: meta.SourceKind, Status: "downloaded"}, true, nil
}

func (s *Store) restoreFromSharedCache(ctx context.Context, name string, spec SourceSpec, architecture string) (client.ImageState, bool, error) {
	shared := s.newSharedStore()
	sharedName := sharedImageKey(spec, architecture)
	meta, err := shared.readMetadata(sharedName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return client.ImageState{}, false, nil
		}
		return client.ImageState{}, false, err
	}
	if meta.Source != spec.Raw || meta.SourceKind != spec.Kind || !dirExists(meta.RootFSDir) {
		return client.ImageState{}, false, nil
	}
	if arch := normalizeArchitecture(architecture); arch != "" && meta.Architecture != arch {
		return client.ImageState{}, false, nil
	}
	if spec.Kind == SourceKindCVMFS {
		if meta.CVMFSRootHash == "" {
			return client.ImageState{}, false, nil
		}
		currentHash, err := s.currentCVMFSRootHash(ctx, spec, meta.CVMFSMirrors)
		if err != nil {
			return client.ImageState{}, false, err
		}
		if currentHash != meta.CVMFSRootHash {
			return client.ImageState{}, false, nil
		}
	}
	if !dirExists(meta.RootFSDir) {
		return client.ImageState{}, false, nil
	}
	if err := s.cloneFromStore(shared, sharedName, name, spec); err != nil {
		return client.ImageState{}, false, err
	}
	return client.ImageState{Name: name, Source: spec.Raw, ResolvedSource: meta.ResolvedSource, SourceKind: spec.Kind, Status: "downloaded"}, true, nil
}

func (s *Store) currentCVMFSRootHash(ctx context.Context, spec SourceSpec, mirrors []string) (string, error) {
	cvmfsClient := &intcvmfs.Client{
		HTTPClient: s.httpClient,
		Context:    ctx,
		CacheDir:   cvmfsCacheDir(s.sharedRoot()),
		OnActivity: s.cvmfsActivity,
		Mirrors:    mirrors,
	}
	return cvmfsClient.ManifestRootHash(normalizeCVMFSSource(spec.Raw))
}

func (s *Store) cvmfsActivity(event intcvmfs.ActivityEvent) {
	if s == nil || s.CVMFSActivity == nil {
		return
	}
	s.CVMFSActivity(event.Bytes)
}

func (s *Store) cloneFromStore(src *Store, srcName, dstName string, spec SourceSpec) error {
	srcMeta, err := src.readMetadata(srcName)
	if err != nil {
		return err
	}
	srcDir := src.imageDir(srcName)
	dstDir := s.imageDir(dstName)
	tmpDir := dstDir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("remove temp image dir: %w", err)
	}
	if err := copyTree(srcDir, tmpDir); err != nil {
		return fmt.Errorf("copy cached image: %w", err)
	}
	meta := srcMeta
	meta.Name = dstName
	meta.Source = spec.Raw
	meta.SourceKind = spec.Kind
	if rel, err := filepath.Rel(srcDir, srcMeta.RootFSDir); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
		meta.RootFSDir = filepath.Join(dstDir, rel)
	} else {
		meta.RootFSDir = dstDir
	}
	if meta.MetadataPath != "" {
		meta.MetadataPath = filepath.Join(dstDir, filepath.Base(srcMeta.MetadataPath))
	}
	if meta.IndexPath != "" {
		meta.IndexPath = filepath.Join(dstDir, filepath.Base(srcMeta.IndexPath))
	}
	buf, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image metadata: %w", err)
	}
	// copyTree hard-links immutable cache artifacts when possible. image.json
	// belongs to the named image, so unlink it before writing the adjusted
	// metadata rather than truncating the shared cache's copy.
	if err := os.Remove(filepath.Join(tmpDir, "image.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace cached image metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "image.json"), buf, 0o644); err != nil {
		return fmt.Errorf("write image metadata: %w", err)
	}
	return replaceImageDir(tmpDir, dstDir)
}

// replaceImageDir keeps the previous complete directory available for
// rollback until the prepared directory has been installed.
func replaceImageDir(tmpDir, dstDir string) error {
	previousDir := dstDir + ".previous"
	if err := os.RemoveAll(previousDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove previous image backup: %w", err)
	}
	hadPrevious := false
	if err := os.Rename(dstDir, previousDir); err == nil {
		hadPrevious = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("preserve old image dir: %w", err)
	}
	if err := os.Rename(tmpDir, dstDir); err != nil {
		if hadPrevious {
			_ = os.Rename(previousDir, dstDir)
		}
		return fmt.Errorf("activate image dir: %w", err)
	}
	if hadPrevious {
		_ = os.RemoveAll(previousDir)
	}
	return nil
}

func (s *Store) fetchManifest(ctx context.Context, reg *registryContext, imageName, tag string, archs ...string) (manifest, string, error) {
	body, mediaType, digest, err := s.getJSONBlobWithDigest(ctx, reg, "/"+imageName+"/manifests/"+tag, []string{
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
	})
	if err != nil {
		return manifest{}, "", err
	}

	if strings.HasPrefix(tag, "sha256:") {
		actual := sha256.Sum256(body)
		if "sha256:"+hex.EncodeToString(actual[:]) != tag {
			return manifest{}, "", &download.DigestError{Expected: tag, Actual: "sha256:" + hex.EncodeToString(actual[:])}
		}
	}
	if isManifestMediaType(mediaType) {
		var mani manifest
		if err := json.Unmarshal(body, &mani); err != nil {
			return manifest{}, "", fmt.Errorf("decode manifest: %w", err)
		}
		return mani, digest, nil
	}

	var index manifestList
	if err := json.Unmarshal(body, &index); err != nil {
		return manifest{}, "", fmt.Errorf("decode manifest list: %w", err)
	}

	for _, arch := range archs {
		for _, entry := range index.Manifests {
			if entry.Platform.OS == "linux" && entry.Platform.Architecture == arch {
				body, _, err := s.getJSONBlob(ctx, reg, "/"+imageName+"/manifests/"+entry.Digest, []string{
					"application/vnd.docker.distribution.manifest.v2+json",
					"application/vnd.oci.image.manifest.v1+json",
				})
				if err != nil {
					return manifest{}, "", err
				}
				actual := sha256.Sum256(body)
				if "sha256:"+hex.EncodeToString(actual[:]) != entry.Digest {
					return manifest{}, "", &download.DigestError{Expected: entry.Digest, Actual: "sha256:" + hex.EncodeToString(actual[:])}
				}
				var mani manifest
				if err := json.Unmarshal(body, &mani); err != nil {
					return manifest{}, "", fmt.Errorf("decode manifest: %w", err)
				}
				return mani, entry.Digest, nil
			}
		}
	}

	return manifest{}, "", fmt.Errorf("manifest for %v not found", archs)
}

func resolvedOCISource(registry, imageName, digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return imageName
	}
	registry = strings.TrimSpace(registry)
	registry = strings.TrimPrefix(registry, "https://")
	registry = strings.TrimPrefix(registry, "http://")
	registry = strings.TrimSuffix(registry, "/")
	registry = strings.TrimSuffix(registry, "/v2")
	if registry == "" || registry == "registry-1.docker.io" {
		return imageName + "@" + digest
	}
	return registry + "/" + imageName + "@" + digest
}

func (s *Store) fetchBlob(ctx context.Context, reg *registryContext, imageName string, blob descriptor) ([]byte, error) {
	blobPath := filepath.Join(s.root, "_blobs", digestToFileName(blob.Digest))
	if data, err := os.ReadFile(blobPath); err == nil {
		metadataProgress(ctx, "config:"+blob.Digest, "image_config", "cached", 0, 0, 0)
		return data, nil
	}

	data, err := s.getRawBlob(ctx, reg, "/"+imageName+"/blobs/"+blob.Digest, blob)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err == nil {
		_ = os.WriteFile(blobPath, data, 0o644)
	}
	return data, nil
}

func (s *Store) cacheLayerBlobs(
	ctx context.Context,
	reg *registryContext,
	imageName string,
	artifact string,
	layers []descriptor,
	report func(client.ProgressEvent),
) error {
	pull, err := s.startLayerBlobPull(ctx, reg, imageName, artifact, layers, report)
	if err != nil {
		return err
	}
	defer pull.cancel()
	return pull.wait(ctx)
}

type layerBlobPull struct {
	cancel    context.CancelFunc
	mu        sync.Mutex
	ready     map[string]chan struct{}
	completed map[string]bool
	results   map[string]error
	done      chan struct{}
	err       error
}

func (p *layerBlobPull) complete(digest string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.completed[digest] {
		return
	}
	p.completed[digest] = true
	p.results[digest] = err
	close(p.ready[digest])
	if err != nil && p.err == nil {
		p.err = err
		p.cancel()
	}
}

func (p *layerBlobPull) waitLayer(ctx context.Context, digest string) error {
	p.mu.Lock()
	ready := p.ready[digest]
	p.mu.Unlock()
	if ready == nil {
		return fmt.Errorf("layer %s is not in the download plan", digest)
	}
	select {
	case <-ready:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.results[digest]
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *layerBlobPull) wait(ctx context.Context) error {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) startLayerBlobPull(
	ctx context.Context,
	reg *registryContext,
	imageName string,
	artifact string,
	layers []descriptor,
	report func(client.ProgressEvent),
) (*layerBlobPull, error) {
	unique := make([]descriptor, 0, len(layers))
	seen := make(map[string]bool, len(layers))
	var totalBytes int64
	for _, layer := range layers {
		if layer.Size <= 0 {
			return nil, fmt.Errorf("layer %s has invalid size %d", layer.Digest, layer.Size)
		}
		if seen[layer.Digest] {
			continue
		}
		seen[layer.Digest] = true
		unique = append(unique, layer)
		totalBytes += layer.Size
	}
	if len(unique) == 0 {
		pull := &layerBlobPull{
			cancel:    func() {},
			ready:     map[string]chan struct{}{},
			completed: map[string]bool{},
			results:   map[string]error{},
			done:      make(chan struct{}),
		}
		close(pull.done)
		return pull, nil
	}

	progress := newParallelBlobProgress(artifact, totalBytes, report)
	pullCtx, cancel := context.WithCancel(ctx)
	pull := &layerBlobPull{
		cancel:    cancel,
		ready:     make(map[string]chan struct{}, len(unique)),
		completed: make(map[string]bool, len(unique)),
		results:   make(map[string]error, len(unique)),
		done:      make(chan struct{}),
	}
	for _, layer := range unique {
		pull.ready[layer.Digest] = make(chan struct{})
	}

	go func() {
		jobs := make(chan descriptor)
		workerCount := min(defaultOCIBlobDownloadWorkers, len(unique))
		var workers sync.WaitGroup
		for range workerCount {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for layer := range jobs {
					err := s.cacheLayerBlob(pullCtx, reg, imageName, layer, progress)
					if err != nil {
						err = fmt.Errorf("cache layer %s: %w", layer.Digest, err)
					}
					pull.complete(layer.Digest, err)
					if err != nil {
						return
					}
				}
			}()
		}
	sendLayers:
		for _, layer := range unique {
			select {
			case jobs <- layer:
			case <-pullCtx.Done():
				break sendLayers
			}
		}
		close(jobs)
		workers.Wait()

		pull.mu.Lock()
		failure := pull.err
		if failure == nil && ctx.Err() != nil {
			failure = ctx.Err()
			pull.err = failure
		}
		pull.mu.Unlock()
		for _, layer := range unique {
			pull.complete(layer.Digest, failure)
		}
		close(pull.done)
	}()
	return pull, nil
}

func (s *Store) cacheLayerBlob(
	ctx context.Context,
	reg *registryContext,
	imageName string,
	layer descriptor,
	progress *parallelBlobProgress,
) error {
	blobPath := filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest))
	if info, err := os.Stat(blobPath); err == nil && info.Mode().IsRegular() {
		if info.Size() == layer.Size {
			progress.complete(layer.Digest, layer.Size)
			return nil
		}
		if err := os.Remove(blobPath); err != nil {
			return fmt.Errorf("remove invalid cached layer: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		return err
	}
	partialPath := blobPath + ".partial"
	out, err := os.OpenFile(partialPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	offset, digest, err := preparePartialBlob(out, partialPath, layer)
	if err != nil {
		return err
	}
	progress.set(layer.Digest, offset)

	delay := ociBlobRetryDelay
	integrityRestarts := 0
	var actualDigest string

downloadPartial:
	for offset < layer.Size {
		attemptCtx, attemptWatchdog, timedOut := withBlobInactivityTimeout(ctx, defaultOCIBlobInactivityTimeout)
		resp, err := reg.doRange(attemptCtx, "/"+imageName+"/blobs/"+layer.Digest, offset)
		if err != nil {
			attemptWatchdog.stop()
			attemptErr := err
			if timedOut() && ctx.Err() == nil {
				attemptErr = errBlobDownloadInactive
			}
			if !retryableBlobDownloadError(ctx, attemptErr) {
				return attemptErr
			}
			if err := waitBlobRetry(ctx, delay); err != nil {
				return err
			}
			delay = min(delay*2, maximumOCIBlobRetryDelay)
			continue
		}

		if offset > 0 && resp.StatusCode == http.StatusOK {
			if err := out.Truncate(0); err != nil {
				resp.Body.Close()
				attemptWatchdog.stop()
				return fmt.Errorf("restart partial layer: %w", err)
			}
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				resp.Body.Close()
				attemptWatchdog.stop()
				return fmt.Errorf("seek restarted partial layer: %w", err)
			}
			offset = 0
			digest = sha256.New()
			progress.set(layer.Digest, 0)
		} else if offset > 0 {
			if err := validateBlobContentRange(resp, offset, layer.Size); err != nil {
				resp.Body.Close()
				attemptWatchdog.stop()
				_ = os.Remove(partialPath)
				return err
			}
		}

		remaining := layer.Size - offset
		if resp.ContentLength >= 0 && resp.ContentLength != remaining {
			resp.Body.Close()
			attemptWatchdog.stop()
			return &download.LengthError{Expected: remaining, Actual: resp.ContentLength}
		}
		tracked := progress.reader(resp.Body, layer.Digest)
		resp.Body = &activityBody{
			ReadCloser: resp.Body,
			onActivity: attemptWatchdog.activity,
			reader:     tracked,
		}
		writer := &layerBlobWriter{dst: io.MultiWriter(out, digest)}
		written, copyErr := download.Copy(attemptCtx, writer, resp, download.Budget{
			MaxBytes:      remaining,
			ExpectedBytes: remaining,
		})
		attemptWatchdog.stop()
		closeErr := resp.Body.Close()
		offset += written
		if copyErr == nil && closeErr == nil {
			continue
		}
		attemptErr := errors.Join(copyErr, closeErr)
		if timedOut() && ctx.Err() == nil {
			attemptErr = errBlobDownloadInactive
		}
		if !retryableBlobDownloadError(ctx, attemptErr) {
			return attemptErr
		}
		if err := waitBlobRetry(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, maximumOCIBlobRetryDelay)
	}

	actualDigest = "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if !strings.EqualFold(actualDigest, layer.Digest) {
		if integrityRestarts == 0 {
			if err := out.Truncate(0); err != nil {
				return fmt.Errorf("discard corrupt partial layer: %w", err)
			}
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("seek restarted layer: %w", err)
			}
			offset = 0
			digest = sha256.New()
			delay = ociBlobRetryDelay
			integrityRestarts++
			progress.set(layer.Digest, 0)
			goto downloadPartial
		}
		_ = os.Remove(partialPath)
		return &download.DigestError{Expected: layer.Digest, Actual: actualDigest}
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync downloaded layer: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close downloaded layer: %w", err)
	}
	if err := os.Rename(partialPath, blobPath); err != nil {
		return err
	}
	progress.complete(layer.Digest, layer.Size)
	return nil
}

var errBlobDownloadInactive = errors.New("OCI blob download made no progress")

type layerBlobWriteError struct{ err error }

func (e *layerBlobWriteError) Error() string { return e.err.Error() }
func (e *layerBlobWriteError) Unwrap() error { return e.err }

type layerBlobWriter struct{ dst io.Writer }

func (w *layerBlobWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if err != nil {
		return n, &layerBlobWriteError{err: err}
	}
	return n, nil
}

func preparePartialBlob(out *os.File, partialPath string, layer descriptor) (int64, hash.Hash, error) {
	info, err := out.Stat()
	if err != nil {
		return 0, nil, fmt.Errorf("inspect partial layer: %w", err)
	}
	if info.Size() < 0 || info.Size() > layer.Size {
		if err := out.Truncate(0); err != nil {
			return 0, nil, fmt.Errorf("discard invalid partial layer: %w", err)
		}
		info, err = out.Stat()
		if err != nil {
			return 0, nil, fmt.Errorf("inspect reset partial layer: %w", err)
		}
	}
	digest := sha256.New()
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return 0, nil, fmt.Errorf("seek partial layer: %w", err)
	}
	if _, err := io.Copy(digest, io.LimitReader(out, info.Size())); err != nil {
		return 0, nil, fmt.Errorf("hash partial layer %q: %w", partialPath, err)
	}
	if _, err := out.Seek(info.Size(), io.SeekStart); err != nil {
		return 0, nil, fmt.Errorf("seek partial layer end: %w", err)
	}
	return info.Size(), digest, nil
}

func validateBlobContentRange(resp *http.Response, offset, total int64) error {
	var start, end, responseTotal int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &responseTotal); err != nil {
		return fmt.Errorf("invalid OCI blob Content-Range %q", resp.Header.Get("Content-Range"))
	}
	if start != offset || end != total-1 || responseTotal != total {
		return fmt.Errorf(
			"unexpected OCI blob Content-Range %q for bytes %d-%d/%d",
			resp.Header.Get("Content-Range"),
			offset,
			total-1,
			total,
		)
	}
	return nil
}

func retryableBlobDownloadError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var writeErr *layerBlobWriteError
	if errors.As(err, &writeErr) {
		return false
	}
	var limitErr *download.LimitError
	if errors.As(err, &limitErr) {
		return false
	}
	var digestErr *download.DigestError
	if errors.As(err, &digestErr) {
		return false
	}
	var statusErr *registryStatusError
	if errors.As(err, &statusErr) {
		return statusErr.code == http.StatusRequestTimeout ||
			statusErr.code == http.StatusTooManyRequests ||
			statusErr.code >= http.StatusInternalServerError
	}
	return true
}

func waitBlobRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type blobAttemptWatchdog struct {
	mu       sync.Mutex
	timer    *time.Timer
	cancel   context.CancelFunc
	finished bool
	timedOut bool
	timeout  time.Duration
}

func withBlobInactivityTimeout(parent context.Context, timeout time.Duration) (context.Context, *blobAttemptWatchdog, func() bool) {
	ctx, cancel := context.WithCancel(parent)
	watchdog := &blobAttemptWatchdog{cancel: cancel, timeout: timeout}
	watchdog.timer = time.AfterFunc(timeout, func() {
		watchdog.mu.Lock()
		defer watchdog.mu.Unlock()
		if watchdog.finished {
			return
		}
		watchdog.timedOut = true
		watchdog.cancel()
	})
	return ctx, watchdog, func() bool {
		watchdog.mu.Lock()
		defer watchdog.mu.Unlock()
		return watchdog.timedOut
	}
}

func (w *blobAttemptWatchdog) activity() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return
	}
	w.timer.Stop()
	w.timer.Reset(w.timeout)
}

func (w *blobAttemptWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return
	}
	w.finished = true
	w.timer.Stop()
	w.cancel()
}

type activityBody struct {
	io.ReadCloser
	reader     io.Reader
	onActivity func()
}

func (r *activityBody) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.onActivity()
	}
	return n, err
}

func (s *Store) cachedLayerBlobAvailable(layer descriptor) bool {
	info, err := os.Stat(filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest)))
	return err == nil && info.Mode().IsRegular() && info.Size() == layer.Size
}

func (s *Store) downloadAndPrepareLayer(
	ctx context.Context,
	reg *registryContext,
	imageName string,
	layer descriptor,
	progress func(int64),
) error {
	blobPath := filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest))
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		return err
	}
	partialBlobPath := blobPath + ".partial"
	if info, err := os.Stat(partialBlobPath); err == nil && info.Size() > 0 {
		return errStreamedLayerIncomplete
	}

	attemptCtx, attemptWatchdog, timedOut := withBlobInactivityTimeout(ctx, defaultOCIBlobInactivityTimeout)
	resp, err := reg.do(attemptCtx, "/"+imageName+"/blobs/"+layer.Digest, nil)
	if err != nil {
		attemptWatchdog.stop()
		if timedOut() && ctx.Err() == nil {
			return fmt.Errorf("%w: %v", errStreamedLayerIncomplete, errBlobDownloadInactive)
		}
		if retryableBlobDownloadError(ctx, err) {
			return fmt.Errorf("%w: %v", errStreamedLayerIncomplete, err)
		}
		return err
	}
	defer resp.Body.Close()
	if err := download.BoundResponse(resp, layer.Size); err != nil {
		attemptWatchdog.stop()
		return err
	}
	resp.Body = &activityBody{
		ReadCloser: resp.Body,
		onActivity: attemptWatchdog.activity,
		reader:     resp.Body,
	}
	blob, err := os.OpenFile(partialBlobPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		attemptWatchdog.stop()
		return err
	}
	defer func() {
		_ = blob.Close()
	}()
	hash := sha256.New()
	body := io.TeeReader(resp.Body, &layerBlobWriter{dst: io.MultiWriter(blob, hash)})
	indexPath, _, err := s.writeLayerArchiveAtomic(layer, body, progress)
	if err != nil {
		attemptWatchdog.stop()
		if timedOut() && ctx.Err() == nil {
			return fmt.Errorf("%w: %v", errStreamedLayerIncomplete, errBlobDownloadInactive)
		}
		info, statErr := blob.Stat()
		if statErr == nil && info.Size() < layer.Size {
			return fmt.Errorf("%w: %v", errStreamedLayerIncomplete, err)
		}
		return err
	}
	archiveValidated := false
	defer func() {
		if !archiveValidated {
			_ = os.RemoveAll(filepath.Dir(indexPath))
		}
	}()
	attemptWatchdog.stop()
	if err := blob.Close(); err != nil {
		return err
	}
	info, err := os.Stat(partialBlobPath)
	if err != nil {
		return err
	}
	if info.Size() != layer.Size {
		_ = os.RemoveAll(filepath.Dir(indexPath))
		return fmt.Errorf("%w: %v", errStreamedLayerIncomplete, &download.LengthError{Expected: layer.Size, Actual: info.Size()})
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actualDigest, layer.Digest) {
		_ = os.Remove(partialBlobPath)
		return &download.DigestError{Expected: layer.Digest, Actual: actualDigest}
	}
	archiveValidated = true
	if err := os.Rename(partialBlobPath, blobPath); err != nil {
		return err
	}
	_ = os.Remove(blobPath)
	return nil
}

func (s *Store) downloadAndApplyLayer(
	ctx context.Context,
	reg *registryContext,
	imageName string,
	layer descriptor,
	dstPath, tarRef string,
	merged map[string]*indexedNode,
	entries map[string]fsmeta.Entry,
	progress func(int64),
) error {
	if err := s.downloadAndPrepareLayer(ctx, reg, imageName, layer, progress); err != nil {
		return err
	}
	indexPath, contentsPath := s.layerArchivePaths(layer.Digest)
	if err := linkOrCopyLayerContents(contentsPath, dstPath); err != nil {
		return fmt.Errorf("link layer contents: %w", err)
	}
	if err := applyLayerArchive(indexPath, tarRef, merged, entries); err != nil {
		return fmt.Errorf("apply layer archive: %w", err)
	}
	return nil
}

var errStreamedLayerIncomplete = errors.New("streamed OCI layer download is incomplete")

func (s *Store) ensureLayerArchive(
	ctx context.Context,
	reg *registryContext,
	imageName string,
	artifact string,
	layer descriptor,
	reportDownload func(int64, float64),
	reportIndex func(int64),
	keepCompressed bool,
) error {
	if keepCompressed && s.cachedStargzLayerAvailable(layer) {
		reportDownload(layer.Size, 0)
		reportIndex(layer.Size)
		return nil
	}
	if s.cachedLayerArchiveAvailable(layer) {
		reportDownload(layer.Size, 0)
		reportIndex(layer.Size)
		return nil
	}
	if s.cachedLayerBlobAvailable(layer) {
		reportDownload(layer.Size, 0)
		if keepCompressed {
			recognized, err := s.prepareStargzLayerFromBlob(layer)
			if err != nil {
				return err
			}
			if recognized {
				reportIndex(layer.Size)
				return nil
			}
		}
		return s.prepareLayerArchiveFromBlob(layer, reportIndex)
	}
	if keepCompressed {
		reconstructed, err := s.tryReconstructStargzLayer(ctx, reg, imageName, layer, func(current int64, rate float64) {
			reportDownload(current, rate)
		})
		if err != nil {
			return err
		}
		if reconstructed {
			recognized, err := s.prepareStargzLayerFromBlob(layer)
			if err != nil {
				return err
			}
			if !recognized {
				return fmt.Errorf("reconstructed layer %s is not valid eStargz", layer.Digest)
			}
			reportDownload(layer.Size, 0)
			reportIndex(layer.Size)
			return nil
		}
		progress := newParallelBlobProgress(artifact, layer.Size, func(event client.ProgressEvent) {
			reportDownload(event.BytesDownloaded, event.RateBytesPerSecond)
		})
		if err := s.cacheLayerBlob(ctx, reg, imageName, layer, progress); err != nil {
			return err
		}
		reportDownload(layer.Size, 0)
		recognized, err := s.prepareStargzLayerFromBlob(layer)
		if err != nil {
			return err
		}
		if recognized {
			reportIndex(layer.Size)
			return nil
		}
		if err := s.prepareLayerArchiveFromBlob(layer, reportIndex); err != nil {
			return err
		}
		reportIndex(layer.Size)
		return nil
	}

	partialPath := filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest)) + ".partial"
	partial, partialErr := os.Stat(partialPath)
	if partialErr != nil && !errors.Is(partialErr, os.ErrNotExist) {
		return partialErr
	}
	if partialErr == nil && partial.Size() == 0 {
		_ = os.Remove(partialPath)
		partialErr = os.ErrNotExist
	}
	if errors.Is(partialErr, os.ErrNotExist) {
		err := s.downloadAndPrepareLayer(ctx, reg, imageName, layer, func(current int64) {
			reportDownload(current, 0)
			reportIndex(current)
		})
		if err == nil {
			reportDownload(layer.Size, 0)
			reportIndex(layer.Size)
			return nil
		}
		if !errors.Is(err, errStreamedLayerIncomplete) || ctx.Err() != nil {
			return err
		}
		// The streamed archive is atomic and was discarded. Keep the partial
		// compressed blob for a ranged retry, then rebuild the index from the
		// completed blob.
		reportIndex(0)
	}

	resumeProgress := newParallelBlobProgress(artifact, layer.Size, func(event client.ProgressEvent) {
		reportDownload(event.BytesDownloaded, event.RateBytesPerSecond)
	})
	if err := s.cacheLayerBlob(ctx, reg, imageName, layer, resumeProgress); err != nil {
		return err
	}
	reportDownload(layer.Size, 0)
	if err := s.prepareLayerArchiveFromBlob(layer, reportIndex); err != nil {
		return err
	}
	reportIndex(layer.Size)
	return nil
}

func (s *Store) writeAndApplyCachedLayer(
	layer descriptor,
	dstPath, tarRef string,
	merged map[string]*indexedNode,
	entries map[string]fsmeta.Entry,
	progress func(int64),
) error {
	if s.cachedStargzLayerAvailable(layer) {
		blobPath := filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest))
		if err := linkOrCopyLayerContents(blobPath, dstPath); err != nil {
			return fmt.Errorf("link cached eStargz layer: %w", err)
		}
		return applyLayerArchive(s.stargzLayerIndexPath(layer.Digest), stargzContentsPrefix+tarRef, merged, entries)
	}
	indexPath, contentsPath := s.layerArchivePaths(layer.Digest)
	if s.cachedLayerArchiveAvailable(layer) {
		if err := linkOrCopyLayerContents(contentsPath, dstPath); err != nil {
			return fmt.Errorf("link cached layer contents: %w", err)
		}
		return applyLayerArchive(indexPath, tarRef, merged, entries)
	}
	blobPath := filepath.Join(s.root, "_blobs", digestToFileName(layer.Digest))
	file, err := os.Open(blobPath)
	if err != nil {
		return err
	}
	indexPath, contentsPath, err = s.writeLayerArchiveAtomic(layer, file, progress)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	_ = os.Remove(blobPath)
	if err := linkOrCopyLayerContents(contentsPath, dstPath); err != nil {
		return fmt.Errorf("link layer contents: %w", err)
	}
	return applyLayerArchive(indexPath, tarRef, merged, entries)
}

func (s *Store) getJSONBlob(ctx context.Context, reg *registryContext, path string, accept []string) ([]byte, string, error) {
	data, mediaType, _, err := s.getJSONBlobWithDigest(ctx, reg, path, accept)
	return data, mediaType, err
}

func (s *Store) getJSONBlobWithDigest(ctx context.Context, reg *registryContext, path string, accept []string) ([]byte, string, string, error) {
	metadataProgress(ctx, "manifest:"+path, "image_manifest", "resolving", 0, -1, 0)
	resp, err := reg.do(ctx, path, accept)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	data, err := download.ReadAll(ctx, resp, download.Budget{MaxBytes: maxRegistryMetadataBytes})
	if err != nil {
		return nil, "", "", fmt.Errorf("read response body: %w", err)
	}
	digest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	sum := sha256.Sum256(data)
	actualDigest := "sha256:" + hex.EncodeToString(sum[:])
	if digest == "" {
		digest = actualDigest
	} else if strings.HasPrefix(digest, "sha256:") && !strings.EqualFold(digest, actualDigest) {
		return nil, "", "", &download.DigestError{Expected: digest, Actual: actualDigest}
	}
	metadataProgress(ctx, "manifest:"+path, "image_manifest", "ready", int64(len(data)), int64(len(data)), int64(len(data)))
	return data, resp.Header.Get("Content-Type"), digest, nil
}

func (s *Store) getRawBlob(ctx context.Context, reg *registryContext, path string, blob descriptor) ([]byte, error) {
	metadataProgress(ctx, "config:"+blob.Digest, "image_config", "queued", 0, blob.Size, 0)
	resp, err := reg.do(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if blob.Size <= 0 {
		return nil, fmt.Errorf("blob %s has invalid size %d", blob.Digest, blob.Size)
	}
	data, err := download.ReadAll(ctx, resp, download.Budget{MaxBytes: blob.Size, ExpectedBytes: blob.Size, ExpectedSHA256: blob.Digest})
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	metadataProgress(ctx, "config:"+blob.Digest, "image_config", "ready", int64(len(data)), int64(len(data)), int64(len(data)))
	return data, nil
}

type registryStatusError struct {
	code   int
	status string
	body   string
}

func (e *registryStatusError) Error() string {
	return fmt.Sprintf("registry request failed: %s (%s)", e.status, e.body)
}

func (reg *registryContext) do(ctx context.Context, path string, accept []string) (*http.Response, error) {
	return reg.doRequest(ctx, path, accept, 0, true)
}

func (reg *registryContext) doRange(ctx context.Context, path string, offset int64) (*http.Response, error) {
	// The caller applies the descriptor's exact byte budget while streaming.
	// Unlike metadata responses, OCI registries may send blobs using chunked
	// transfer encoding, so they do not require a declared Content-Length here.
	return reg.doRequest(ctx, path, nil, offset, false)
}

func (reg *registryContext) doRequest(ctx context.Context, path string, accept []string, rangeOffset int64, requireContentLength bool) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.registry+path, nil)
		if err != nil {
			return nil, err
		}
		if token := reg.bearerToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for _, value := range accept {
			req.Header.Add("Accept", value)
		}
		if rangeOffset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", rangeOffset))
		}

		resp, err := reg.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry request: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			if err := reg.authorize(ctx, resp.Header.Get("www-authenticate")); err != nil {
				resp.Body.Close()
				return nil, err
			}
			resp.Body.Close()
			continue
		}
		statusOK := resp.StatusCode == http.StatusOK ||
			(rangeOffset > 0 && resp.StatusCode == http.StatusPartialContent)
		if !statusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			return nil, &registryStatusError{
				code:   resp.StatusCode,
				status: resp.Status,
				body:   strings.TrimSpace(string(body)),
			}
		}
		if requireContentLength {
			if err := download.BoundResponse(resp, 0); err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("registry response: %w", err)
			}
		}
		return resp, nil
	}
	return nil, fmt.Errorf("registry authorization failed")
}

func (reg *registryContext) bearerToken() string {
	reg.tokenMu.RLock()
	defer reg.tokenMu.RUnlock()
	return reg.token
}

func (reg *registryContext) authorize(ctx context.Context, header string) error {
	params, err := parseAuthenticate(header)
	if err != nil {
		return err
	}
	tokenURL, err := url.Parse(params["realm"])
	if err != nil {
		return fmt.Errorf("parse token realm: %w", err)
	}
	query := tokenURL.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	if scope := params["scope"]; scope != "" {
		query.Set("scope", scope)
	}
	tokenURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return err
	}
	resp, err := reg.client.Do(req)
	if err != nil {
		return fmt.Errorf("request registry token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token request failed: %s", resp.Status)
	}
	data, err := download.ReadAll(ctx, resp, download.Budget{MaxBytes: 1 << 20})
	if err != nil {
		return fmt.Errorf("registry token response: %w", err)
	}
	var token tokenResponse
	if err := json.Unmarshal(data, &token); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	switch {
	case token.Token != "":
		reg.tokenMu.Lock()
		reg.token = token.Token
		reg.tokenMu.Unlock()
	case token.AccessToken != "":
		reg.tokenMu.Lock()
		reg.token = token.AccessToken
		reg.tokenMu.Unlock()
	default:
		return fmt.Errorf("token response missing token")
	}
	return nil
}

func (s *Store) getLocked(name string) (client.ImageState, error) {
	if s.downloading[name] {
		meta, err := s.readMetadata(name)
		if err == nil {
			return client.ImageState{Name: name, Source: meta.Source, ResolvedSource: meta.ResolvedSource, SourceKind: meta.SourceKind, Status: "downloading"}, nil
		}
		return client.ImageState{Name: name, Status: "downloading"}, nil
	}

	meta, err := s.readMetadata(name)
	if err == nil {
		return client.ImageState{Name: meta.Name, Source: meta.Source, ResolvedSource: meta.ResolvedSource, SourceKind: meta.SourceKind, Status: "downloaded"}, nil
	}
	if lastErr := s.lastErr[name]; lastErr != nil {
		return client.ImageState{Name: name, Status: "error", Error: lastErr.Error()}, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return client.ImageState{}, fmt.Errorf("image %q not found", name)
	}
	return client.ImageState{}, err
}

func (s *Store) writeMetadata(name string, meta metadata) error {
	dir := s.imageDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	buf, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.json"), buf, 0o644); err != nil {
		return fmt.Errorf("write image metadata: %w", err)
	}
	return nil
}

func (s *Store) readMetadata(name string) (metadata, error) {
	var ret metadata
	buf, err := os.ReadFile(filepath.Join(s.imageDir(name), "image.json"))
	if err != nil {
		return ret, err
	}
	if err := json.Unmarshal(buf, &ret); err != nil {
		return ret, fmt.Errorf("decode image metadata: %w", err)
	}
	if ret.Name == "" {
		ret.Name = name
	}
	if ret.Source == "" {
		return ret, errors.New("image metadata missing source")
	}
	if ret.SourceKind == "" {
		spec, err := ParseSource(ret.Source)
		if err != nil {
			return ret, fmt.Errorf("infer source kind: %w", err)
		}
		ret.SourceKind = spec.Kind
	}
	return ret, nil
}

func (s *Store) imageDir(name string) string {
	return filepath.Join(s.root, name)
}

func validateImageStoreName(name string) error {
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") {
		return fmt.Errorf("image name %q must be relative", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("image name %q escapes the image store", name)
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("image name %q contains an invalid path component", name)
		}
	}
	return nil
}

func (s *Store) sharedRoot() string {
	if root := strings.TrimSpace(s.sharedCacheRoot); root != "" {
		return root
	}
	if root := strings.TrimSpace(os.Getenv(sharedCacheEnv)); root != "" {
		return root
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil || cacheRoot == "" {
		return filepath.Join(os.TempDir(), "ccx3-oci-cache")
	}
	return filepath.Join(cacheRoot, "ccx3", "oci")
}

func (img *Image) Command(override []string) []string {
	if len(override) > 0 {
		if len(img.Config.Entrypoint) > 0 {
			return append(append([]string(nil), img.Config.Entrypoint...), override...)
		}
		return append([]string(nil), override...)
	}
	if len(img.Config.Entrypoint) > 0 && len(img.Config.Cmd) > 0 {
		out := append([]string(nil), img.Config.Entrypoint...)
		return append(out, img.Config.Cmd...)
	}
	if len(img.Config.Entrypoint) > 0 {
		return append([]string(nil), img.Config.Entrypoint...)
	}
	return append([]string(nil), img.Config.Cmd...)
}

func ParseSource(source string) (SourceSpec, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return SourceSpec{}, fmt.Errorf("image source is required")
	}
	if isScratchSource(source) {
		return SourceSpec{Kind: SourceKindInternal, Raw: internalScratchSource}, nil
	}
	lower := strings.ToLower(source)
	switch {
	case strings.HasPrefix(lower, "docker-archive:"):
		return SourceSpec{Kind: SourceKindDockerArchive, Raw: source}, nil
	case strings.HasPrefix(lower, "rootfs-tar:"):
		pathValue := strings.TrimSpace(source[len("rootfs-tar:"):])
		if pathValue == "" {
			return SourceSpec{}, fmt.Errorf("rootfs tar source is required")
		}
		return SourceSpec{Kind: SourceKindRootFSTar, Raw: "rootfs-tar:" + pathValue}, nil
	case strings.HasPrefix(lower, "http+cvmfs://"):
		return SourceSpec{Kind: SourceKindCVMFS, Raw: source}, nil
	case strings.HasPrefix(lower, "cvmfs://"):
		return SourceSpec{Kind: SourceKindCVMFS, Raw: source}, nil
	case strings.HasPrefix(lower, "/cvmfs/"):
		return SourceSpec{Kind: SourceKindCVMFS, Raw: source}, nil
	case (strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) && strings.Contains(lower, "/cvmfs/"):
		return SourceSpec{Kind: SourceKindCVMFS, Raw: source}, nil
	case strings.HasSuffix(lower, ".simg"), strings.HasSuffix(lower, ".sif"):
		return SourceSpec{Kind: SourceKindSIMG, Raw: source}, nil
	default:
		if _, _, _, err := ParseImageRef(source); err == nil {
			return SourceSpec{Kind: SourceKindOCI, Raw: source}, nil
		}
		return SourceSpec{}, fmt.Errorf("unsupported image source %q", source)
	}
}

func isScratchSource(source string) bool {
	registry, image, tag, err := ParseImageRef(strings.TrimSpace(source))
	if err != nil {
		return false
	}
	return registry == defaultRegistry && image == "library/scratch" && tag == "latest"
}

func isScratchImageName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	base, _, _ := strings.Cut(name, "@")
	return base == "scratch"
}

func ParseImageRef(imageRef string) (registry string, image string, tag string, err error) {
	if strings.TrimSpace(imageRef) == "" {
		return "", "", "", fmt.Errorf("image source is required")
	}
	if strings.ContainsAny(imageRef, " \t\r\n?#\\") || strings.Contains(imageRef, "://") {
		return "", "", "", fmt.Errorf("invalid OCI reference")
	}
	image = imageRef
	tag = "latest"
	if at := strings.IndexByte(imageRef, '@'); at >= 0 {
		image, tag = imageRef[:at], imageRef[at+1:]
		digestBytes, decodeErr := hex.DecodeString(strings.TrimPrefix(tag, "sha256:"))
		if !strings.HasPrefix(tag, "sha256:") || decodeErr != nil || len(digestBytes) != sha256.Size || tag != strings.ToLower(tag) {
			return "", "", "", fmt.Errorf("invalid image digest")
		}
		// Optional tag@digest syntax is pinned by the digest.
		if colon := strings.LastIndexByte(image, ':'); colon > strings.LastIndexByte(image, '/') {
			image = image[:colon]
		}
	} else if colon := strings.LastIndexByte(image, ':'); colon > strings.LastIndexByte(image, '/') {
		image, tag = image[:colon], image[colon+1:]
	}
	if image == "" || tag == "" || strings.ContainsAny(tag, "/@") {
		return "", "", "", fmt.Errorf("invalid OCI reference")
	}
	for _, component := range strings.Split(image, "/") {
		if component == "" || component == "." || component == ".." {
			return "", "", "", fmt.Errorf("invalid OCI repository")
		}
	}

	firstSlash := strings.Index(image, "/")
	if firstSlash != -1 {
		firstComponent := image[:firstSlash]
		isHostname := strings.Contains(firstComponent, ".") || strings.Contains(firstComponent, ":") || firstComponent == "localhost"
		if isHostname {
			registry = firstComponent
			image = image[firstSlash+1:]
		}
	}

	if registry == "" || registry == "docker.io" {
		registry = defaultRegistry
	}
	if !strings.HasPrefix(registry, "http://") && !strings.HasPrefix(registry, "https://") {
		registry = "https://" + registry
	}
	if !strings.HasSuffix(registry, "/v2") {
		registry += "/v2"
	}
	if registry == defaultRegistry && !strings.Contains(image, "/") {
		image = "library/" + image
	}
	return registry, image, tag, nil
}

func writeUncompressedTar(dst io.Writer, source string, body io.Reader) error {
	buffered := bufio.NewReader(body)
	var src io.Reader = buffered
	lower := strings.ToLower(source)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"), hasMagic(buffered, []byte{0x1f, 0x8b}):
		gzr, err := gzip.NewReader(src)
		if err != nil {
			return fmt.Errorf("open gzip rootfs tar: %w", err)
		}
		defer gzr.Close()
		src = gzr
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"), hasMagic(buffered, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		xzr, err := xz.NewReader(src)
		if err != nil {
			return fmt.Errorf("open xz rootfs tar: %w", err)
		}
		src = xzr
	}
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("write rootfs tar: %w", err)
	}
	return nil
}

type downloadProgressReader struct {
	r          io.Reader
	artifact   string
	blob       string
	total      int64
	base       int64
	downloaded int64
	started    time.Time
	lastReport time.Time
	report     func(client.ProgressEvent)
}

type parallelBlobProgress struct {
	mu           sync.Mutex
	artifact     string
	total        int64
	downloaded   int64
	networkBytes int64
	blobs        map[string]int64
	started      time.Time
	lastReport   time.Time
	report       func(client.ProgressEvent)
}

type parallelBlobProgressReader struct {
	r        io.Reader
	progress *parallelBlobProgress
	blob     string
}

func newParallelBlobProgress(artifact string, total int64, report func(client.ProgressEvent)) *parallelBlobProgress {
	return &parallelBlobProgress{
		artifact: artifact,
		total:    total,
		blobs:    make(map[string]int64),
		started:  time.Now(),
		report:   report,
	}
}

func (p *parallelBlobProgress) reader(r io.Reader, blob string) io.Reader {
	return &parallelBlobProgressReader{r: r, progress: p, blob: blob}
}

func (r *parallelBlobProgressReader) Read(buf []byte) (int, error) {
	n, err := r.r.Read(buf)
	if n > 0 {
		r.progress.add(r.blob, int64(n))
	}
	return n, err
}

func (p *parallelBlobProgress) add(blob string, count int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blobs[blob] += count
	p.downloaded += count
	p.networkBytes += count
	p.emitLocked(blob, false)
}

func (p *parallelBlobProgress) set(blob string, count int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	previous := p.blobs[blob]
	if previous == count {
		return
	}
	p.blobs[blob] = count
	p.downloaded += count - previous
	p.emitLocked(blob, true)
}

func (p *parallelBlobProgress) complete(blob string, size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if remaining := size - p.blobs[blob]; remaining > 0 {
		p.blobs[blob] += remaining
		p.downloaded += remaining
	}
	p.emitLocked(blob, true)
}

func (p *parallelBlobProgress) emitLocked(blob string, force bool) {
	if p.report == nil {
		return
	}
	now := time.Now()
	if !force && !p.lastReport.IsZero() && now.Sub(p.lastReport) < 200*time.Millisecond {
		return
	}
	p.lastReport = now
	elapsed := now.Sub(p.started)
	if elapsed <= 0 {
		elapsed = time.Second
	}
	rate := float64(p.networkBytes) / elapsed.Seconds()
	var etaSeconds float64
	if p.total > p.downloaded && rate > 0 {
		etaSeconds = float64(p.total-p.downloaded) / rate
	}
	reportPullProgress(p.report, client.ProgressEvent{
		Status:             "downloading",
		Artifact:           p.artifact,
		Blob:               blob,
		Progress:           ratio(p.downloaded, p.total),
		BytesDownloaded:    p.downloaded,
		BytesTotal:         p.total,
		RateBytesPerSecond: rate,
		ETASeconds:         etaSeconds,
	})
}

func newDownloadProgressReader(r io.Reader, artifact, blob string, total int64, report func(client.ProgressEvent)) *downloadProgressReader {
	return newAggregateDownloadProgressReader(r, artifact, blob, 0, total, report)
}

func newAggregateDownloadProgressReader(
	r io.Reader,
	artifact string,
	blob string,
	base int64,
	total int64,
	report func(client.ProgressEvent),
) *downloadProgressReader {
	p := &downloadProgressReader{
		r:          r,
		artifact:   artifact,
		blob:       blob,
		total:      total,
		base:       base,
		started:    time.Now(),
		lastReport: time.Time{},
		report:     report,
	}
	p.emit(false)
	return p
}

func (p *downloadProgressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		p.downloaded += int64(n)
		p.emit(false)
	}
	return n, err
}

func (p *downloadProgressReader) finish() {
	p.emit(true)
}

func (p *downloadProgressReader) emit(force bool) {
	if p.report == nil {
		return
	}
	now := time.Now()
	if !force && !p.lastReport.IsZero() && now.Sub(p.lastReport) < 200*time.Millisecond {
		return
	}
	p.lastReport = now
	elapsed := now.Sub(p.started)
	if elapsed <= 0 {
		elapsed = time.Second
	}
	rate := float64(p.downloaded) / elapsed.Seconds()
	current := p.base + p.downloaded
	etaSeconds := 0.0
	if p.total > current && rate > 0 {
		etaSeconds = float64(p.total-current) / rate
	}
	progress := 0.0
	if p.total > 0 {
		progress = float64(current) / float64(p.total)
	}
	reportPullProgress(p.report, client.ProgressEvent{
		Status:             "downloading",
		Artifact:           p.artifact,
		Blob:               p.blob,
		Progress:           progress,
		BytesDownloaded:    current,
		BytesTotal:         p.total,
		RateBytesPerSecond: rate,
		ETASeconds:         etaSeconds,
	})
}

func hasMagic(r *bufio.Reader, magic []byte) bool {
	buf, err := r.Peek(len(magic))
	return err == nil && bytes.Equal(buf, magic)
}

func parseAuthenticate(value string) (map[string]string, error) {
	if value == "" {
		return nil, fmt.Errorf("missing authenticate header")
	}
	value = strings.TrimPrefix(value, "Bearer ")
	parts := strings.Split(value, ",")
	ret := map[string]string{}
	for _, part := range parts {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("malformed authenticate header segment %q", part)
		}
		ret[strings.TrimSpace(key)] = strings.Trim(val, "\" ")
	}
	return ret, nil
}

func sanitizeArchivePath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("empty archive path")
	}
	name = path.Clean(strings.TrimPrefix(name, "/"))
	if name == "." || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	return name, nil
}

func isManifestMediaType(mediaType string) bool {
	return strings.Contains(mediaType, "manifest.v1+json") || strings.Contains(mediaType, "manifest.v2+json")
}

func digestToFileName(digest string) string {
	if strings.HasPrefix(digest, "sha256:") {
		return strings.TrimPrefix(digest, "sha256:")
	}
	sum := sha256.Sum256([]byte(digest))
	return hex.EncodeToString(sum[:])
}

func sharedImageKey(spec SourceSpec, architecture string) string {
	arch := normalizeArchitecture(architecture)
	if arch == "" {
		arch = nativeArch()
	}
	sum := sha256.Sum256([]byte(sharedCacheSchemaVersion + "\n" + arch + "\n" + spec.Kind + "\n" + spec.Raw))
	return hex.EncodeToString(sum[:16])
}

func normalizeArchitecture(architecture string) string {
	switch strings.ToLower(strings.TrimSpace(architecture)) {
	case "", "native":
		return ""
	case "x86_64", "x64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(architecture))
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func copyTree(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(current string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, current)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			return os.MkdirAll(target, mode.Perm())
		case mode&os.ModeSymlink != 0:
			link, err := os.Readlink(current)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case mode.IsRegular():
			if err := os.Link(current, target); err == nil {
				return nil
			}
			// Custom image and shared-cache roots may live on different
			// filesystems, where hard links are unavailable.
			return copyFile(current, target, mode.Perm())
		default:
			return fmt.Errorf("unsupported file mode %v at %s", mode, current)
		}
	})
}

func copyFile(srcPath, dstPath string, mode os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

func nativeArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	case "amd64":
		return "amd64"
	default:
		return runtime.GOARCH
	}
}

func preferredManifestArchitectures(architecture string) []string {
	if arch := normalizeArchitecture(architecture); arch != "" {
		return []string{arch}
	}
	out := []string{nativeArch()}
	if nativeArch() == "arm64" {
		out = append(out, "amd64")
	}
	return out
}

func labelPairsFromMap(labels map[string]string) []labelPair {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]labelPair, 0, len(keys))
	for _, key := range keys {
		out = append(out, labelPair{Key: key, Value: labels[key]})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func labelsFromPairs(pairs []labelPair) map[string]string {
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		out[pair.Key] = pair.Value
	}
	return out
}
