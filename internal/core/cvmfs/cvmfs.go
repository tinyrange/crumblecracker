package cvmfs

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/download"
	"github.com/tinyrange/crumblecracker/internal/core/linuxabi"
	intsqlite "github.com/tinyrange/crumblecracker/internal/core/sqlite"
)

const DefaultMirror = "https://cvmfs.neurodesk.org/cvmfs"
const maxCVMFSExpandedObjectBytes int64 = 4 << 30

const (
	flagDir          = 1
	flagRegularFile  = 4
	flagSymlink      = 8
	flagChunkedFile  = 64
	flagExternalFile = 128

	linuxSIFMT    = linuxabi.SIFMT
	linuxSIFSOCK  = linuxabi.SIFSOCK
	linuxSIFLNK   = linuxabi.SIFLNK
	linuxSIFREG   = linuxabi.SIFREG
	linuxSIFBLK   = linuxabi.SIFBLK
	linuxSIFDIR   = linuxabi.SIFDIR
	linuxSIFCHR   = linuxabi.SIFCHR
	linuxSIFIFO   = linuxabi.SIFIFO
	linuxPermMask = linuxabi.PermMask
)

var errStopWalk = errors.New("stop walk")

var cachePublicationLocks = struct {
	sync.Mutex
	locks map[string]*cachePublicationLock
}{locks: make(map[string]*cachePublicationLock)}

type cachePublicationLock struct {
	refs int
	mu   sync.Mutex
}

type Target struct {
	Raw       string
	Mirror    string
	Repo      string
	Path      string
	LocalPath string
	Remote    bool
}

func FormatTarget(target Target) (string, error) {
	if !target.Remote {
		if strings.TrimSpace(target.LocalPath) == "" {
			return "", fmt.Errorf("local CVMFS target is missing LocalPath")
		}
		return target.LocalPath, nil
	}
	if strings.TrimSpace(target.Mirror) == "" {
		return "", fmt.Errorf("remote CVMFS target is missing Mirror")
	}
	if strings.TrimSpace(target.Repo) == "" {
		return "", fmt.Errorf("remote CVMFS target is missing Repo")
	}
	u, err := url.Parse(target.Mirror)
	if err != nil {
		return "", err
	}
	segments := []string{strings.TrimSuffix(u.EscapedPath(), "/"), url.PathEscape(target.Repo)}
	for _, part := range strings.Split(strings.TrimPrefix(target.Path, "/"), "/") {
		if part == "" {
			continue
		}
		segments = append(segments, url.PathEscape(part))
	}
	escaped := strings.Join(segments, "/")
	if escaped == "" {
		escaped = "/"
	}
	u.RawPath = escaped
	u.Path, err = url.PathUnescape(escaped)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

type DirEntry struct {
	Name    string
	Mode    fs.FileMode
	Size    int64
	ModTime time.Time
}

type WalkEntry struct {
	Path    string
	Mode    fs.FileMode
	Size    int64
	ModTime time.Time
	UID     uint32
	GID     uint32
	RDev    uint32
	Symlink string
}

// Entry describes one logical node in a CVMFS repository.
type Entry = WalkEntry

type Client struct {
	HTTPClient             *http.Client
	Context                context.Context
	MaxExpandedObjectBytes int64
	CacheDir               string
	TraceLogPath           string
	OnActivity             func(ActivityEvent)
	OnTransfer             func(TransferEvent)
	Mirrors                []string

	mu           sync.Mutex
	repos        map[string]*repository
	mirrorStats  map[string]*mirrorStat
	mirrorCursor uint64
	preferred    string
}

func (c *Client) expandedObjectBudget() int64 {
	if c != nil && c.MaxExpandedObjectBytes > 0 {
		return c.MaxExpandedObjectBytes
	}
	return maxCVMFSExpandedObjectBytes
}

type ActivityEvent struct {
	Time      time.Time
	Op        string
	Bytes     int
	Target    string
	URL       string
	CachePath string
}

type TransferEvent struct {
	Time       time.Time
	ID         uint64
	State      string
	Repo       string
	Path       string
	URL        string
	Mirror     string
	Bytes      int64
	TotalBytes int64
	Error      string
}

type traceEvent struct {
	Time       string  `json:"time"`
	ID         uint64  `json:"id"`
	Event      string  `json:"event"`
	Op         string  `json:"op"`
	Target     string  `json:"target,omitempty"`
	URL        string  `json:"url,omitempty"`
	CachePath  string  `json:"cache_path,omitempty"`
	Bytes      int     `json:"bytes,omitempty"`
	StatusCode int     `json:"status_code,omitempty"`
	DurationMS float64 `json:"duration_ms,omitempty"`
	Error      string  `json:"error,omitempty"`
}

var traceID atomic.Uint64
var transferID atomic.Uint64

type manifest struct {
	RootCatalogHash string
}

type repository struct {
	client   *Client
	mirrors  []string
	repo     string
	manifest *manifest
	catalogs map[string]*catalog

	indexMu         sync.RWMutex
	indexedCatalogs map[*catalog]bool
	globalPaths     map[string]string
	entriesByPath   map[string]catalogEntry
	childrenByPath  map[string][]catalogEntry
}

type mirrorStat struct {
	successes int
	failures  int
	ewma      time.Duration
	lastUsed  time.Time
}

type catalog struct {
	entries []catalogEntry
	nested  []nestedCatalog
}

type nestedCatalog struct {
	Path string
	Sha1 string
}

type catalogChunk struct {
	Md5Path1 int64
	Md5Path2 int64
	Offset   int64
	Size     int64
	Hash     []byte
}

type catalogEntry struct {
	Md5Path1 int64
	Md5Path2 int64
	Parent1  int64
	Parent2  int64

	Hash    []byte
	Size    int64
	Mode    int64
	Mtime   int64
	MtimeNS int64
	Flags   int64
	Name    string
	Symlink string
	UID     int64
	GID     int64

	FullPath string
	Chunks   []catalogChunk
}

func NewClient() *Client {
	return &Client{HTTPClient: http.DefaultClient}
}

func ParseTarget(raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Target{}, fmt.Errorf("empty CVMFS target")
	}
	if strings.HasPrefix(raw, "/cvmfs/") {
		clean := path.Clean(raw)
		trimmed := strings.TrimPrefix(clean, "/cvmfs/")
		parts := strings.SplitN(trimmed, "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			return Target{}, fmt.Errorf("invalid mounted CVMFS path %q", raw)
		}
		inner := "/"
		if len(parts) == 2 {
			inner = "/" + strings.TrimPrefix(parts[1], "/")
		}
		return Target{
			Raw:       raw,
			Repo:      parts[0],
			Path:      path.Clean(inner),
			LocalPath: clean,
		}, nil
	}
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, ".") {
		return Target{
			Raw:       raw,
			Path:      "/",
			LocalPath: raw,
		}, nil
	}
	if strings.HasPrefix(raw, "cvmfs://") {
		u, err := url.Parse(raw)
		if err != nil {
			return Target{}, err
		}
		if u.Host == "" {
			return Target{}, fmt.Errorf("missing CVMFS repository in %q", raw)
		}
		return Target{
			Raw:    raw,
			Remote: true,
			Mirror: DefaultMirror,
			Repo:   u.Host,
			Path:   cleanRepoPath(u.EscapedPath(), u.Path),
		}, nil
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err != nil {
			return Target{}, err
		}
		parts := strings.Split(strings.TrimPrefix(path.Clean(u.Path), "/"), "/")
		for i, part := range parts {
			if part != "cvmfs" || i+1 >= len(parts) {
				continue
			}
			mirrorPath := "/"
			if i > 0 {
				mirrorPath = "/" + strings.Join(parts[:i+1], "/")
			} else {
				mirrorPath = "/cvmfs"
			}
			repo := parts[i+1]
			inner := "/"
			if i+2 < len(parts) {
				inner = "/" + strings.Join(parts[i+2:], "/")
			}
			return Target{
				Raw:    raw,
				Remote: true,
				Mirror: (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: mirrorPath}).String(),
				Repo:   repo,
				Path:   path.Clean(inner),
			}, nil
		}
		return Target{}, fmt.Errorf("expected /cvmfs/<repo>/... in %q", raw)
	}
	return Target{}, fmt.Errorf("unsupported CVMFS target %q", raw)
}

func (c *Client) ReadDir(target string) ([]DirEntry, error) {
	id, started := c.traceStart("ReadDir", target, "", "")
	parsed, err := ParseTarget(target)
	if err != nil {
		c.traceDone(id, started, "ReadDir", target, "", "", 0, 0, err)
		return nil, err
	}
	if !parsed.Remote {
		entries, err := readLocalDir(parsed.LocalPath)
		c.traceDone(id, started, "ReadDir", target, "", "", len(entries), 0, err)
		return entries, err
	}
	repo := c.newRepository(parsed)
	entries, err := repo.ReadDir(parsed.Path)
	c.traceDone(id, started, "ReadDir", target, "", "", len(entries), 0, err)
	return entries, err
}

// Stat resolves a single logical CVMFS path without reading its contents.
func (c *Client) Stat(target string) (Entry, error) {
	parsed, err := ParseTarget(target)
	if err != nil {
		return Entry{}, err
	}
	if !parsed.Remote {
		info, err := os.Lstat(parsed.LocalPath)
		if err != nil {
			return Entry{}, err
		}
		entry := Entry{Path: parsed.LocalPath, Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime()}
		if info.Mode()&fs.ModeSymlink != 0 {
			entry.Symlink, err = os.Readlink(parsed.LocalPath)
		}
		return entry, err
	}
	return c.newRepository(parsed).Stat(parsed.Path)
}

func (c *Client) ReadFile(target string) ([]byte, error) {
	id, started := c.traceStart("ReadFile", target, "", "")
	parsed, err := ParseTarget(target)
	if err != nil {
		c.traceDone(id, started, "ReadFile", target, "", "", 0, 0, err)
		return nil, err
	}
	if !parsed.Remote {
		data, err := os.ReadFile(parsed.LocalPath)
		c.traceDone(id, started, "ReadFile", target, "", "", len(data), 0, err)
		return data, err
	}
	if data, err := c.readCachedFile(parsed); err == nil {
		c.traceDone(id, started, "ReadFile", target, "", cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path), len(data), 0, nil)
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		c.traceDone(id, started, "ReadFile", target, "", cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path), 0, 0, err)
		return nil, err
	}
	repo := c.newRepository(parsed)
	data, err := repo.ReadFile(parsed.Path)
	if err != nil {
		c.traceDone(id, started, "ReadFile", target, "", "", 0, 0, err)
		return nil, err
	}
	if err := c.writeCachedFile(parsed, data); err != nil {
		c.traceDone(id, started, "ReadFile", target, "", cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path), len(data), 0, err)
		return nil, err
	}
	c.traceDone(id, started, "ReadFile", target, "", cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path), len(data), 0, nil)
	return data, nil
}

func (c *Client) PrefetchFile(target string) (uint64, error) {
	id, started := c.traceStart("PrefetchFile", target, "", "")
	parsed, err := ParseTarget(target)
	if err != nil {
		c.traceDone(id, started, "PrefetchFile", target, "", "", 0, 0, err)
		return 0, err
	}
	if !parsed.Remote {
		info, err := os.Stat(parsed.LocalPath)
		if err != nil {
			c.traceDone(id, started, "PrefetchFile", target, "", parsed.LocalPath, 0, 0, err)
			return 0, err
		}
		if info.IsDir() {
			err := fmt.Errorf("%q is a directory", parsed.LocalPath)
			c.traceDone(id, started, "PrefetchFile", target, "", parsed.LocalPath, 0, 0, err)
			return 0, err
		}
		c.traceDone(id, started, "PrefetchFile", target, "", parsed.LocalPath, int(info.Size()), 0, nil)
		return uint64(info.Size()), nil
	}
	cachePath := cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path)
	if size, err := privateCacheFileSize(c.CacheDir, cachePath); err == nil {
		c.traceDone(id, started, "PrefetchFile", target, "", cachePath, int(size), 0, nil)
		return uint64(size), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		c.traceDone(id, started, "PrefetchFile", target, "", cachePath, 0, 0, err)
		return 0, err
	}
	repo := c.newRepository(parsed)
	size, err := repo.PrefetchFile(parsed.Path)
	if err != nil {
		c.traceDone(id, started, "PrefetchFile", target, "", cachePath, 0, 0, err)
		return 0, err
	}
	c.traceDone(id, started, "PrefetchFile", target, "", cachePath, int(size), 0, nil)
	return size, nil
}

func (c *Client) CachedFileSize(target string) (uint64, bool, error) {
	parsed, err := ParseTarget(target)
	if err != nil {
		return 0, false, err
	}
	if !parsed.Remote {
		info, err := os.Stat(parsed.LocalPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return 0, false, nil
			}
			return 0, false, err
		}
		if info.IsDir() {
			return 0, false, fmt.Errorf("%q is a directory", parsed.LocalPath)
		}
		return uint64(info.Size()), true, nil
	}
	cachePath := cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path)
	if cachePath == "" {
		return 0, false, nil
	}
	size, err := privateCacheFileSize(c.CacheDir, cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return uint64(size), true, nil
}

func (c *Client) WriteFileTo(target string, dst io.Writer) (uint64, error) {
	id, started := c.traceStart("WriteFileTo", target, "", "")
	parsed, err := ParseTarget(target)
	if err != nil {
		c.traceDone(id, started, "WriteFileTo", target, "", "", 0, 0, err)
		return 0, err
	}
	if !parsed.Remote {
		file, err := os.Open(parsed.LocalPath)
		if err != nil {
			c.traceDone(id, started, "WriteFileTo", target, "", parsed.LocalPath, 0, 0, err)
			return 0, err
		}
		defer file.Close()
		n, err := io.Copy(dst, file)
		c.traceDone(id, started, "WriteFileTo", target, "", parsed.LocalPath, int(n), 0, err)
		return uint64(n), err
	}
	cachePath := cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path)
	if file, err := openPrivateCacheFile(c.CacheDir, cachePath); err == nil {
		defer file.Close()
		n, err := io.Copy(dst, file)
		c.traceDone(id, started, "WriteFileTo", target, "", cachePath, int(n), 0, err)
		return uint64(n), err
	} else if !errors.Is(err, os.ErrNotExist) {
		c.traceDone(id, started, "WriteFileTo", target, "", cachePath, 0, 0, err)
		return 0, err
	}
	repo := c.newRepository(parsed)
	n, err := repo.WriteFileTo(parsed.Path, dst)
	c.traceDone(id, started, "WriteFileTo", target, "", cachePath, int(n), 0, err)
	return n, err
}

func (c *Client) ReadFileRange(target string, offset, length int64) ([]byte, bool, error) {
	id, started := c.traceStart("ReadFileRange", target, "", "")
	if offset < 0 {
		err := fmt.Errorf("offset must be >= 0")
		c.traceDone(id, started, "ReadFileRange", target, "", "", 0, 0, err)
		return nil, false, err
	}
	if length < 0 {
		err := fmt.Errorf("length must be >= 0")
		c.traceDone(id, started, "ReadFileRange", target, "", "", 0, 0, err)
		return nil, false, err
	}
	parsed, err := ParseTarget(target)
	if err != nil {
		c.traceDone(id, started, "ReadFileRange", target, "", "", 0, 0, err)
		return nil, false, err
	}
	if !parsed.Remote {
		data, eof, err := readLocalFileRange(parsed.LocalPath, offset, length)
		if err != nil {
			c.traceDone(id, started, "ReadFileRange", target, "", "", 0, 0, err)
			return nil, false, err
		}
		c.traceDone(id, started, "ReadFileRange", target, "", parsed.LocalPath, len(data), 0, nil)
		return data, eof, nil
	}
	cachePath := cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path)
	if file, err := openPrivateCacheFile(c.CacheDir, cachePath); err == nil {
		defer file.Close()
		info, statErr := file.Stat()
		if statErr != nil {
			c.traceDone(id, started, "ReadFileRange", target, "", cachePath, 0, 0, statErr)
			return nil, false, statErr
		}
		if offset >= info.Size() {
			c.traceDone(id, started, "ReadFileRange", target, "", cachePath, 0, 0, nil)
			return []byte{}, true, nil
		}
		end := info.Size()
		if length > 0 && offset+length < end {
			end = offset + length
		}
		buf := make([]byte, end-offset)
		n, readErr := file.ReadAt(buf, offset)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			c.traceDone(id, started, "ReadFileRange", target, "", cachePath, n, 0, readErr)
			return nil, false, readErr
		}
		c.traceDone(id, started, "ReadFileRange", target, "", cachePath, n, 0, nil)
		return buf[:n], end == info.Size(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		c.traceDone(id, started, "ReadFileRange", target, "", cachePath, 0, 0, err)
		return nil, false, err
	}
	if offset == 0 && length == 0 {
		data, err := c.ReadFile(target)
		if err != nil {
			c.traceDone(id, started, "ReadFileRange", target, "", "", 0, 0, err)
			return nil, false, err
		}
		c.traceDone(id, started, "ReadFileRange", target, "", "", len(data), 0, nil)
		return data, true, nil
	}
	repo := c.newRepository(parsed)
	data, eof, err := repo.ReadFileRange(parsed.Path, offset, length)
	if err != nil {
		c.traceDone(id, started, "ReadFileRange", target, "", "", 0, 0, err)
		return nil, false, err
	}
	c.traceDone(id, started, "ReadFileRange", target, "", "", len(data), 0, nil)
	return data, eof, nil
}

func readLocalFileRange(path string, offset, length int64) ([]byte, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	start, end, eof := byteRange(info.Size(), offset, length)
	if start == end {
		return []byte{}, eof, nil
	}
	buf := make([]byte, end-start)
	n, err := file.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	return buf[:n], eof, nil
}

func byteRange(size, offset, length int64) (int64, int64, bool) {
	if offset >= size {
		return size, size, true
	}
	end := size
	if length > 0 && offset+length < end {
		end = offset + length
	}
	return offset, end, end == size
}

func (c *Client) Walk(target string, visit func(WalkEntry) error) error {
	id, started := c.traceStart("Walk", target, "", "")
	parsed, err := ParseTarget(target)
	if err != nil {
		c.traceDone(id, started, "Walk", target, "", "", 0, 0, err)
		return err
	}
	if !parsed.Remote {
		err := walkLocal(parsed.LocalPath, visit)
		c.traceDone(id, started, "Walk", target, "", "", 0, 0, err)
		return err
	}
	repo := c.newRepository(parsed)
	err = repo.Walk(parsed.Path, visit)
	c.traceDone(id, started, "Walk", target, "", "", 0, 0, err)
	return err
}

func (c *Client) ManifestRootHash(target string) (string, error) {
	id, started := c.traceStart("ManifestRootHash", target, "", "")
	parsed, err := ParseTarget(target)
	if err != nil {
		c.traceDone(id, started, "ManifestRootHash", target, "", "", 0, 0, err)
		return "", err
	}
	if !parsed.Remote {
		err := fmt.Errorf("manifest root hash is only available for remote CVMFS targets")
		c.traceDone(id, started, "ManifestRootHash", target, "", "", 0, 0, err)
		return "", err
	}
	repo := c.newRepository(parsed)
	manifest, err := repo.getManifest()
	if err != nil {
		c.traceDone(id, started, "ManifestRootHash", target, "", "", 0, 0, err)
		return "", err
	}
	c.traceDone(id, started, "ManifestRootHash", target, "", "", len(manifest.RootCatalogHash), 0, nil)
	return manifest.RootCatalogHash, nil
}

func (c *Client) newRepository(target Target) *repository {
	client := c
	if client == nil {
		client = NewClient()
	}
	mirrors := client.candidateMirrors(target.Mirror)
	key := strings.Join(mirrors, "\x00") + "\x00" + target.Repo
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.HTTPClient == nil {
		client.HTTPClient = http.DefaultClient
	}
	if client.repos == nil {
		client.repos = map[string]*repository{}
	}
	if repo := client.repos[key]; repo != nil {
		return repo
	}
	repo := &repository{
		client:          client,
		mirrors:         mirrors,
		repo:            target.Repo,
		catalogs:        map[string]*catalog{},
		indexedCatalogs: map[*catalog]bool{},
		globalPaths:     map[string]string{"0:0": "/"},
		entriesByPath:   map[string]catalogEntry{},
		childrenByPath:  map[string][]catalogEntry{},
	}
	client.repos[key] = repo
	return repo
}

func (c *Client) candidateMirrors(primary string) []string {
	c.mu.Lock()
	preferred := c.preferred
	configured := append([]string(nil), c.Mirrors...)
	c.mu.Unlock()
	seen := map[string]bool{}
	candidates := make([]string, 0, len(configured)+2)
	for _, raw := range append([]string{preferred, primary}, configured...) {
		mirror := normalizeMirror(raw)
		if mirror == "" || seen[mirror] {
			continue
		}
		seen[mirror] = true
		candidates = append(candidates, mirror)
	}
	if len(candidates) == 0 {
		return []string{normalizeMirror(DefaultMirror)}
	}
	return candidates
}

// NormalizeMirror returns the canonical CVMFS HTTP base used by the client.
func NormalizeMirror(raw string) string {
	raw = strings.TrimSpace(strings.TrimRight(raw, "/"))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/cvmfs") {
		u.Path = strings.TrimRight(u.Path, "/") + "/cvmfs"
	}
	return strings.TrimRight(u.String(), "/")
}

func normalizeMirror(raw string) string { return NormalizeMirror(raw) }

// SetPreferredMirror makes a measured or user-selected mirror the first
// choice for future repository operations while retaining failover mirrors.
func (c *Client) SetPreferredMirror(mirror string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.preferred = normalizeMirror(mirror)
	c.mu.Unlock()
}

func readLocalDir(dir string) ([]DirEntry, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(items))
	for _, item := range items {
		info, err := item.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, DirEntry{
			Name:    item.Name(),
			Mode:    info.Mode(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func walkLocal(root string, visit func(WalkEntry) error) error {
	root = filepath.Clean(root)
	if _, err := os.Lstat(root); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		guestPath := "/"
		if rel != "." {
			guestPath = "/" + filepath.ToSlash(rel)
		}
		walkEntry := WalkEntry{
			Path:    guestPath,
			Mode:    info.Mode(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			walkEntry.Symlink = target
		}
		return visit(walkEntry)
	})
}

func (r *repository) ReadDir(dirPath string) ([]DirEntry, error) {
	dirPath = path.Clean("/" + strings.TrimPrefix(dirPath, "/"))
	if err := r.ensurePrefixIndexed(dirPath); err != nil {
		return nil, err
	}
	r.indexMu.RLock()
	dir, foundDir := r.entriesByPath[dirPath]
	children := append([]catalogEntry(nil), r.childrenByPath[dirPath]...)
	r.indexMu.RUnlock()
	if dirPath != "/" && (!foundDir || !dir.isDir()) {
		return nil, os.ErrNotExist
	}
	byName := make(map[string]DirEntry, len(children))
	for _, ent := range children {
		byName[ent.Name] = DirEntry{
			Name:    ent.Name,
			Mode:    linuxModeToGo(uint32(ent.Mode)),
			Size:    ent.Size,
			ModTime: time.Unix(ent.Mtime, ent.MtimeNS),
		}
	}
	out := make([]DirEntry, 0, len(byName))
	for _, entry := range byName {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *repository) Stat(nodePath string) (Entry, error) {
	nodePath = path.Clean("/" + strings.TrimPrefix(nodePath, "/"))
	if nodePath == "/" {
		if _, err := r.rootCatalog(); err != nil {
			return Entry{}, err
		}
		return Entry{Path: "/", Mode: fs.ModeDir | 0o555}, nil
	}
	found, err := r.lookupIndexedEntry(nodePath)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Path: found.FullPath, Mode: linuxModeToGo(uint32(found.Mode)), Size: found.Size,
		ModTime: time.Unix(found.Mtime, found.MtimeNS), UID: uint32(found.UID), GID: uint32(found.GID),
		Symlink: found.Symlink,
	}, nil
}

func (r *repository) ReadFile(filePath string) ([]byte, error) {
	filePath = path.Clean("/" + strings.TrimPrefix(filePath, "/"))
	found, err := r.lookupIndexedEntry(filePath)
	if err != nil {
		return nil, err
	}
	if found.isDir() {
		return nil, fmt.Errorf("%q is a directory", filePath)
	}
	if found.isSymlink() {
		return nil, fmt.Errorf("%q is a symlink", filePath)
	}
	return r.readEntryData(*found)
}

func (r *repository) ReadFileRange(filePath string, offset, length int64) ([]byte, bool, error) {
	filePath = path.Clean("/" + strings.TrimPrefix(filePath, "/"))
	found, err := r.lookupFileEntry(filePath)
	if err != nil {
		return nil, false, err
	}
	start, end, eof := byteRange(found.Size, offset, length)
	if start == end {
		return []byte{}, eof, nil
	}
	var buf bytes.Buffer
	buf.Grow(int(end - start))
	if err := r.streamEntryDataRange(*found, start, end-start, &buf); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), eof, nil
}

func (r *repository) PrefetchFile(filePath string) (uint64, error) {
	filePath = path.Clean("/" + strings.TrimPrefix(filePath, "/"))
	found, err := r.lookupFileEntry(filePath)
	if err != nil {
		return 0, err
	}
	cachePath := cvmfsFileCachePath(r.client.CacheDir, r.repo, filePath)
	if cachePath == "" {
		data, err := r.readEntryData(*found)
		if err != nil {
			return 0, err
		}
		return uint64(len(data)), nil
	}
	if size, err := privateCacheFileSize(r.client.CacheDir, cachePath); err == nil {
		return uint64(size), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if err := writeAtomicFile(cachePath, func(dst io.Writer) error {
		return r.streamEntryData(*found, dst, false)
	}); err != nil {
		return 0, err
	}
	return uint64(found.Size), nil
}

func (r *repository) WriteFileTo(filePath string, dst io.Writer) (uint64, error) {
	filePath = path.Clean("/" + strings.TrimPrefix(filePath, "/"))
	found, err := r.lookupFileEntry(filePath)
	if err != nil {
		return 0, err
	}
	counting := &countingWriter{w: dst}
	if err := r.streamEntryData(*found, counting, false); err != nil {
		return 0, err
	}
	return uint64(counting.n), nil
}

func (r *repository) orderedMirrors() []string {
	if len(r.mirrors) <= 1 {
		return append([]string(nil), r.mirrors...)
	}
	r.client.mu.Lock()
	defer r.client.mu.Unlock()
	if r.client.mirrorStats == nil {
		r.client.mirrorStats = map[string]*mirrorStat{}
	}
	cursor := r.client.mirrorCursor
	r.client.mirrorCursor++
	type rankedMirror struct {
		mirror string
		index  int
		score  time.Duration
		known  bool
	}
	ranked := make([]rankedMirror, 0, len(r.mirrors))
	for i, mirror := range r.mirrors {
		stat := r.client.mirrorStats[mirror]
		if stat == nil {
			ranked = append(ranked, rankedMirror{
				mirror: mirror,
				index:  int((uint64(i) + cursor) % uint64(len(r.mirrors))),
			})
			continue
		}
		score := stat.ewma + time.Duration(stat.failures)*2*time.Second
		if !stat.lastUsed.IsZero() && time.Since(stat.lastUsed) < 3*time.Second {
			score += 250 * time.Millisecond
		}
		ranked = append(ranked, rankedMirror{
			mirror: mirror,
			index:  int((uint64(i) + cursor) % uint64(len(r.mirrors))),
			score:  score,
			known:  true,
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left := ranked[i]
		right := ranked[j]
		if left.known != right.known {
			return !left.known
		}
		if left.score != right.score {
			return left.score < right.score
		}
		return left.index < right.index
	})
	out := make([]string, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.mirror)
	}
	preferred := r.client.preferred
	if preferred != "" {
		stat := r.client.mirrorStats[preferred]
		if stat == nil || stat.failures == 0 {
			for index, mirror := range out {
				if mirror == preferred {
					copy(out[1:index+1], out[:index])
					out[0] = preferred
					break
				}
			}
		}
	}
	return out
}

func (c *Client) recordMirrorResult(mirror string, duration time.Duration, err error, statusCode int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mirrorStats == nil {
		c.mirrorStats = map[string]*mirrorStat{}
	}
	stat := c.mirrorStats[mirror]
	if stat == nil {
		stat = &mirrorStat{}
		c.mirrorStats[mirror] = stat
	}
	stat.lastUsed = time.Now()
	if err != nil || statusCode >= 500 || statusCode == http.StatusTooManyRequests {
		stat.failures++
		return
	}
	stat.successes++
	if stat.ewma == 0 {
		stat.ewma = duration
	} else {
		stat.ewma = (stat.ewma*7 + duration) / 8
	}
	if stat.failures > 0 {
		stat.failures--
	}
}

func (r *repository) getFromMirrors(op, logicalPath string, urlFor func(string) string, handle func(url string, resp *http.Response, id uint64, started time.Time) error) error {
	var lastErr error
	for _, mirror := range r.orderedMirrors() {
		url := urlFor(mirror)
		id, started := r.client.traceStart(op, "", url, "")
		ctx := r.client.Context
		if ctx == nil {
			ctx = context.Background()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := r.client.HTTPClient.Do(req)
		if err != nil {
			r.client.traceDone(id, started, op, "", url, "", 0, 0, err)
			r.client.recordMirrorResult(mirror, time.Since(started), err, 0)
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			what := "fetch object"
			if op == "HTTPManifest" {
				what = "fetch manifest"
			}
			err := fmt.Errorf("%s: unexpected status %s", what, resp.Status)
			_ = resp.Body.Close()
			r.client.traceDone(id, started, op, "", url, "", 0, resp.StatusCode, err)
			r.client.recordMirrorResult(mirror, time.Since(started), err, resp.StatusCode)
			lastErr = err
			continue
		}
		maxBytes := int64(0)
		if op == "HTTPManifest" {
			maxBytes = 16 << 20
		}
		if err := download.BoundResponse(resp, maxBytes); err != nil {
			_ = resp.Body.Close()
			r.client.traceDone(id, started, op, "", url, "", 0, resp.StatusCode, err)
			r.client.recordMirrorResult(mirror, time.Since(started), err, resp.StatusCode)
			lastErr = err
			continue
		}
		var transfer *transferReader
		if logicalPath != "" && r.client != nil && r.client.OnTransfer != nil {
			total := resp.ContentLength
			if total < 0 {
				total = 0
			}
			transfer = &transferReader{
				reader: resp.Body, client: r.client,
				event: TransferEvent{ID: transferID.Add(1), Repo: r.repo, Path: logicalPath, URL: url, Mirror: mirror, TotalBytes: total},
			}
			transfer.emit("started", "")
			resp.Body = struct {
				io.Reader
				io.Closer
			}{Reader: transfer, Closer: resp.Body}
		}
		err = handle(url, resp, id, started)
		closeErr := resp.Body.Close()
		if err == nil {
			err = closeErr
		}
		r.client.recordMirrorResult(mirror, time.Since(started), err, resp.StatusCode)
		if transfer != nil {
			state := "completed"
			detail := ""
			if err != nil {
				state = "failed"
				detail = err.Error()
			}
			transfer.emit(state, detail)
		}
		if err == nil {
			return nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no CVMFS mirrors configured for %s", r.repo)
	}
	return lastErr
}

type transferReader struct {
	reader io.Reader
	client *Client
	event  TransferEvent
}

func (r *transferReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.event.Bytes += int64(n)
		r.emit("progress", "")
	}
	return n, err
}

func (r *transferReader) emit(state, detail string) {
	if r == nil || r.client == nil || r.client.OnTransfer == nil {
		return
	}
	event := r.event
	event.Time = time.Now()
	event.State = state
	event.Error = detail
	r.client.OnTransfer(event)
}

func (r *repository) Walk(rootPath string, visit func(WalkEntry) error) error {
	rootPath = path.Clean("/" + strings.TrimPrefix(rootPath, "/"))
	found := false
	err := r.walkPrefixWithNested(rootPath, shouldWalkNestedCatalogForWalk, func(ent catalogEntry) error {
		if !isWithinPrefix(ent.FullPath, rootPath) {
			return nil
		}
		found = true
		return visit(WalkEntry{
			Path:    ent.FullPath,
			Mode:    linuxModeToGo(uint32(ent.Mode)),
			Size:    ent.Size,
			ModTime: time.Unix(ent.Mtime, ent.MtimeNS),
			UID:     uint32(ent.UID),
			GID:     uint32(ent.GID),
			RDev:    0,
			Symlink: ent.Symlink,
		})
	})
	if err != nil {
		return err
	}
	if !found {
		return os.ErrNotExist
	}
	return nil
}

func (r *repository) readEntryData(ent catalogEntry) ([]byte, error) {
	var buf bytes.Buffer
	if ent.Size > 0 && ent.Size <= int64(^uint(0)>>1) {
		buf.Grow(int(ent.Size))
	}
	if err := r.streamEntryData(ent, &buf, true); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	if int64(len(data)) > ent.Size {
		data = data[:ent.Size]
	}
	return data, nil
}

func (r *repository) streamEntryData(ent catalogEntry, dst io.Writer, cacheCompressed bool) error {
	if ent.Flags&flagExternalFile == 0 && !ent.isChunked() {
		return r.streamDataObject(stringifyHash(ent.Hash), false, ent.FullPath, dst, cacheCompressed)
	}
	if len(ent.Chunks) == 0 {
		return fmt.Errorf("chunk metadata missing for %q", ent.FullPath)
	}
	var written int64
	for _, chunk := range ent.Chunks {
		remaining := ent.Size - written
		if remaining <= 0 {
			break
		}
		chunkWriter := io.Writer(dst)
		if chunk.Size > remaining {
			chunkWriter = &limitedWriter{w: dst, n: remaining}
		}
		counting := &countingWriter{w: chunkWriter}
		if err := r.streamDataObject(stringifyHash(chunk.Hash), true, ent.FullPath, counting, cacheCompressed); err != nil {
			return err
		}
		written += counting.n
	}
	return nil
}

func (r *repository) streamEntryDataRange(ent catalogEntry, offset, length int64, dst io.Writer) error {
	if length <= 0 {
		return nil
	}
	if ent.Flags&flagExternalFile == 0 && !ent.isChunked() {
		return r.streamDataObjectRange(stringifyHash(ent.Hash), false, ent.FullPath, offset, length, dst)
	}
	if len(ent.Chunks) == 0 {
		return fmt.Errorf("chunk metadata missing for %q", ent.FullPath)
	}
	chunks := append([]catalogChunk(nil), ent.Chunks...)
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].Offset == chunks[j].Offset {
			return stringifyHash(chunks[i].Hash) < stringifyHash(chunks[j].Hash)
		}
		return chunks[i].Offset < chunks[j].Offset
	})
	rangeEnd := offset + length
	for _, chunk := range chunks {
		chunkStart := chunk.Offset
		chunkEnd := chunk.Offset + chunk.Size
		if chunkEnd <= offset || chunkStart >= rangeEnd {
			continue
		}
		overlapStart := maxInt64(offset, chunkStart)
		overlapEnd := minInt64(rangeEnd, chunkEnd)
		if overlapEnd <= overlapStart {
			continue
		}
		if err := r.streamDataObjectRange(
			stringifyHash(chunk.Hash),
			true,
			ent.FullPath,
			overlapStart-chunkStart,
			overlapEnd-overlapStart,
			dst,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *repository) streamDataObject(hash string, partial bool, logicalPath string, dst io.Writer, cacheCompressed bool) error {
	suffix := ""
	if partial {
		suffix = "P"
	}
	if raw, err := r.readCachedObject(hash, suffix); err == nil {
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		defer zr.Close()
		_, err = io.Copy(dst, zr)
		return err
	} else if !errors.Is(err, os.ErrNotExist) && !(r.client == nil || strings.TrimSpace(r.client.CacheDir) == "") {
		return err
	}

	cachePath := cvmfsObjectCachePath(r.client.CacheDir, hash, suffix)
	return r.getFromMirrors("HTTPObject", logicalPath, func(mirror string) string {
		return r.objectURL(mirror, hash, suffix)
	}, func(url string, resp *http.Response, id uint64, started time.Time) error {
		reader := r.client.activityReader("HTTPObject", "", url, cachePath, resp.Body)
		var cacheWriter *atomicFile
		if cacheCompressed && cachePath != "" {
			cacheFile, err := createAtomicFile(cachePath)
			if err != nil {
				r.client.traceDone(id, started, "HTTPObject", "", url, cachePath, 0, resp.StatusCode, err)
				return err
			}
			cacheWriter = cacheFile
			reader = io.TeeReader(reader, cacheFile)
		}

		zr, err := zlib.NewReader(reader)
		if err != nil {
			if cacheWriter != nil {
				_ = cacheWriter.Abort()
			}
			r.client.traceDone(id, started, "HTTPObject", "", url, cachePath, 0, resp.StatusCode, err)
			return err
		}
		counting := &countingWriter{w: dst}
		_, err = io.Copy(counting, zr)
		closeErr := zr.Close()
		if err == nil {
			err = closeErr
		}
		if cacheWriter != nil {
			if err == nil {
				err = cacheWriter.Close()
			} else {
				_ = cacheWriter.Abort()
			}
			cacheWriter = nil
		}
		r.client.traceDone(id, started, "HTTPObject", "", url, cachePath, int(counting.n), resp.StatusCode, err)
		return err
	})
}

func (r *repository) streamDataObjectRange(hash string, partial bool, logicalPath string, offset, length int64, dst io.Writer) error {
	suffix := ""
	if partial {
		suffix = "P"
	}
	if raw, err := r.readCachedObject(hash, suffix); err == nil {
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		defer zr.Close()
		return copyCompressedRange(zr, offset, length, dst)
	} else if !errors.Is(err, os.ErrNotExist) && !(r.client == nil || strings.TrimSpace(r.client.CacheDir) == "") {
		return err
	}

	cachePath := cvmfsObjectCachePath(r.client.CacheDir, hash, suffix)
	return r.getFromMirrors("HTTPObjectRange", logicalPath, func(mirror string) string {
		return r.objectURL(mirror, hash, suffix)
	}, func(url string, resp *http.Response, id uint64, started time.Time) error {
		reader := r.client.activityReader("HTTPObjectRange", "", url, cachePath, resp.Body)
		var cacheWriter *atomicFile
		if cachePath != "" {
			cacheFile, err := createAtomicFile(cachePath)
			if err != nil {
				r.client.traceDone(id, started, "HTTPObjectRange", "", url, cachePath, 0, resp.StatusCode, err)
				return err
			}
			cacheWriter = cacheFile
			reader = io.TeeReader(reader, cacheFile)
		}

		zr, err := zlib.NewReader(reader)
		if err != nil {
			if cacheWriter != nil {
				_ = cacheWriter.Abort()
			}
			r.client.traceDone(id, started, "HTTPObjectRange", "", url, cachePath, 0, resp.StatusCode, err)
			return err
		}
		counting := &countingWriter{w: dst}
		err = copyCompressedRange(zr, offset, length, counting)
		if err == nil && cacheWriter != nil {
			_, err = io.Copy(io.Discard, zr)
		}
		closeErr := zr.Close()
		if err == nil {
			err = closeErr
		}
		if cacheWriter != nil {
			if err == nil {
				err = cacheWriter.Close()
			} else {
				_ = cacheWriter.Abort()
			}
			cacheWriter = nil
		}
		r.client.traceDone(id, started, "HTTPObjectRange", "", url, cachePath, int(counting.n), resp.StatusCode, err)
		return err
	})
}

func copyCompressedRange(src io.Reader, offset, length int64, dst io.Writer) error {
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, src, offset); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	if length <= 0 {
		return nil
	}
	_, err := io.CopyN(dst, src, length)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (r *repository) lookupFileEntry(filePath string) (*catalogEntry, error) {
	found, err := r.lookupIndexedEntry(filePath)
	if err != nil {
		return nil, err
	}
	if found.isDir() {
		return nil, fmt.Errorf("%q is a directory", filePath)
	}
	if found.isSymlink() {
		return nil, fmt.Errorf("%q is a symlink", filePath)
	}
	return found, nil
}

func (r *repository) lookupIndexedEntry(entryPath string) (*catalogEntry, error) {
	entryPath = path.Clean("/" + strings.TrimPrefix(entryPath, "/"))
	if err := r.ensurePrefixIndexed(entryPath); err != nil {
		return nil, err
	}
	r.indexMu.RLock()
	entry, ok := r.entriesByPath[entryPath]
	r.indexMu.RUnlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	return &entry, nil
}

func (r *repository) ensurePrefixIndexed(prefix string) error {
	root, err := r.rootCatalog()
	if err != nil {
		return err
	}
	return r.ensureCatalogPrefixIndexed(root, "/", prefix)
}

func (r *repository) ensureCatalogPrefixIndexed(cat *catalog, baseParent, prefix string) error {
	if err := r.indexCatalog(cat, baseParent); err != nil {
		return err
	}
	for _, nested := range cat.nested {
		nestedPath := path.Clean("/" + strings.TrimPrefix(nested.Path, "/"))
		if !shouldWalkNestedCatalog(nestedPath, prefix) {
			continue
		}
		child, err := r.openCatalog(nested.Sha1)
		if err != nil {
			return err
		}
		if err := r.ensureCatalogPrefixIndexed(child, path.Dir(nestedPath), prefix); err != nil {
			return err
		}
	}
	return nil
}

func (r *repository) indexCatalog(cat *catalog, baseParent string) error {
	r.indexMu.Lock()
	defer r.indexMu.Unlock()
	if r.indexedCatalogs == nil {
		r.indexedCatalogs = make(map[*catalog]bool)
	}
	if r.globalPaths == nil {
		r.globalPaths = map[string]string{"0:0": "/"}
	}
	if r.entriesByPath == nil {
		r.entriesByPath = make(map[string]catalogEntry)
	}
	if r.childrenByPath == nil {
		r.childrenByPath = make(map[string][]catalogEntry)
	}
	if r.indexedCatalogs[cat] {
		return nil
	}
	local := make(map[string]catalogEntry, len(cat.entries))
	for _, ent := range cat.entries {
		local[ent.pathHash()] = ent
	}
	memo := make(map[string]string, len(cat.entries))
	resolving := make(map[string]bool)
	var resolve func(catalogEntry) (string, error)
	resolve = func(ent catalogEntry) (string, error) {
		hash := ent.pathHash()
		if full, ok := memo[hash]; ok {
			return full, nil
		}
		if resolving[hash] {
			return "", fmt.Errorf("catalog parent cycle at %q in repo %q", ent.Name, r.repo)
		}
		resolving[hash] = true
		defer delete(resolving, hash)
		parentPath, ok := r.globalPaths[ent.parentHash()]
		if !ok {
			if ent.parentHash() == "0:0" {
				parentPath = baseParent
			} else if parent, exists := local[ent.parentHash()]; exists {
				full, err := resolve(parent)
				if err != nil {
					return "", err
				}
				parentPath = full
			} else {
				return "", fmt.Errorf("parent not found for %q in repo %q", ent.Name, r.repo)
			}
		}
		full := path.Clean(path.Join(parentPath, ent.Name))
		memo[hash] = full
		r.globalPaths[hash] = full
		return full, nil
	}
	for index := range cat.entries {
		full, err := resolve(cat.entries[index])
		if err != nil {
			return err
		}
		cat.entries[index].FullPath = full
		entry := cat.entries[index]
		r.entriesByPath[full] = entry
		parent := path.Dir(full)
		r.childrenByPath[parent] = append(r.childrenByPath[parent], entry)
	}
	r.indexedCatalogs[cat] = true
	return nil
}

func (r *repository) walkPrefix(prefix string, visit func(ent catalogEntry) error) error {
	return r.walkPrefixWithNested(prefix, shouldWalkNestedCatalog, visit)
}

func (r *repository) walkPrefixWithNested(prefix string, shouldWalkNested func(string, string) bool, visit func(ent catalogEntry) error) error {
	root, err := r.rootCatalog()
	if err != nil {
		return err
	}
	globalPaths := map[string]string{"0:0": "/"}
	return r.walkCatalog(root, "/", prefix, globalPaths, shouldWalkNested, visit)
}

func (r *repository) walkCatalog(cat *catalog, baseParent, prefix string, globalPaths map[string]string, shouldWalkNested func(string, string) bool, visit func(ent catalogEntry) error) error {
	if err := r.indexCatalog(cat, baseParent); err != nil {
		return err
	}
	for i := range cat.entries {
		globalPaths[cat.entries[i].pathHash()] = cat.entries[i].FullPath
		if hasCommonFragment(cat.entries[i].FullPath, prefix) {
			if err := visit(cat.entries[i]); err != nil {
				return err
			}
		}
	}
	for _, nested := range cat.nested {
		nestedPath := path.Clean("/" + strings.TrimPrefix(nested.Path, "/"))
		if !shouldWalkNested(nestedPath, prefix) {
			continue
		}
		child, err := r.openCatalog(nested.Sha1)
		if err != nil {
			return err
		}
		if err := r.walkCatalog(child, path.Dir(nestedPath), prefix, globalPaths, shouldWalkNested, visit); err != nil {
			return err
		}
	}
	return nil
}

func (r *repository) rootCatalog() (*catalog, error) {
	manifest, err := r.getManifest()
	if err != nil {
		return nil, err
	}
	return r.openCatalog(manifest.RootCatalogHash)
}

func (r *repository) getManifest() (*manifest, error) {
	if r.manifest != nil {
		return r.manifest, nil
	}
	body, err := r.fetchManifest()
	if err != nil {
		return nil, err
	}
	out, err := parseManifest(body)
	if err != nil {
		return nil, err
	}
	r.manifest = &out
	return r.manifest, nil
}

func (r *repository) fetchManifest() ([]byte, error) {
	var body []byte
	err := r.getFromMirrors("HTTPManifest", "", func(mirror string) string {
		return fmt.Sprintf("%s/%s/.cvmfspublished", mirror, r.repo)
	}, func(url string, resp *http.Response, id uint64, started time.Time) error {
		ctx := r.client.Context
		if ctx == nil {
			ctx = context.Background()
		}
		data, readErr := download.ReadAllReader(ctx, resp.Body, 16<<20)
		if readErr != nil {
			r.client.traceDone(id, started, "HTTPManifest", "", url, "", len(data), resp.StatusCode, readErr)
			return readErr
		}
		if writeErr := r.writeCachedManifest(data); writeErr != nil {
			r.client.traceDone(id, started, "HTTPManifest", "", url, "", len(data), resp.StatusCode, writeErr)
			return writeErr
		}
		body = data
		r.client.traceDone(id, started, "HTTPManifest", "", url, "", len(data), resp.StatusCode, nil)
		return nil
	})
	if err == nil {
		return body, nil
	}
	if body, cacheErr := r.readCachedManifest(); cacheErr == nil {
		return body, nil
	}
	return nil, err
}

func parseManifest(body []byte) (manifest, error) {
	var out manifest
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "--" {
			continue
		}
		switch line[0] {
		case 'C':
			out.RootCatalogHash = line[1:]
		}
	}
	if out.RootCatalogHash == "" {
		return manifest{}, fmt.Errorf("manifest missing root catalog hash")
	}
	return out, nil
}

func (r *repository) readCachedManifest() ([]byte, error) {
	if r.client == nil || strings.TrimSpace(r.client.CacheDir) == "" {
		return nil, os.ErrNotExist
	}
	return readPrivateCacheFile(r.client.CacheDir, filepath.Join(r.client.CacheDir, "state", r.repo, "manifest"))
}

func (r *repository) writeCachedManifest(data []byte) error {
	if r.client == nil || strings.TrimSpace(r.client.CacheDir) == "" {
		return nil
	}
	manifestPath := filepath.Join(r.client.CacheDir, "state", r.repo, "manifest")
	return writeAtomicFile(manifestPath, func(dst io.Writer) error {
		_, err := dst.Write(data)
		return err
	})
}

func (r *repository) openCatalog(hash string) (*catalog, error) {
	if cat, ok := r.catalogs[hash]; ok {
		return cat, nil
	}
	data, err := r.fetchCatalogDB(hash)
	if err != nil {
		return nil, err
	}
	db, err := intsqlite.ParseDatabase(data)
	if err != nil {
		return nil, err
	}
	cat, err := loadCatalog(db)
	if err != nil {
		return nil, err
	}
	r.catalogs[hash] = cat
	return cat, nil
}

func (r *repository) fetchCatalogDB(hash string) ([]byte, error) {
	raw, err := r.fetchCompressedObject(hash, "C")
	if err != nil {
		return nil, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	ctx := r.client.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return download.ReadAllReader(ctx, zr, r.client.expandedObjectBudget())
}

func (r *repository) fetchCompressedObject(hash, suffix string) ([]byte, error) {
	cachePath := cvmfsObjectCachePath(r.client.CacheDir, hash, suffix)
	if cachePath != "" {
		cacheID, cacheStarted := r.client.traceStart("CacheObject", "", "", cachePath)
		if data, err := r.readCachedObject(hash, suffix); err == nil {
			r.client.traceDone(cacheID, cacheStarted, "CacheObject", "", "", cachePath, len(data), 0, nil)
			return data, nil
		} else {
			r.client.traceDone(cacheID, cacheStarted, "CacheObject", "", "", cachePath, 0, 0, err)
			if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}

	var buf bytes.Buffer
	err := r.getFromMirrors("HTTPObject", "", func(mirror string) string {
		return r.objectURL(mirror, hash, suffix)
	}, func(url string, resp *http.Response, id uint64, started time.Time) error {
		buf.Reset()
		reader := r.client.activityReader("HTTPObject", "", url, cachePath, resp.Body)
		_, err := io.Copy(&buf, reader)
		data := buf.Bytes()
		if err != nil {
			r.client.traceDone(id, started, "HTTPObject", "", url, "", len(data), resp.StatusCode, err)
			return err
		}
		if err := r.writeCachedObject(hash, suffix, data); err != nil {
			r.client.traceDone(id, started, "HTTPObject", "", url, cachePath, len(data), resp.StatusCode, err)
			return err
		}
		r.client.traceDone(id, started, "HTTPObject", "", url, cachePath, len(data), resp.StatusCode, nil)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), buf.Bytes()...), nil
}

func (r *repository) readCachedObject(hash, suffix string) ([]byte, error) {
	if r.client == nil || strings.TrimSpace(r.client.CacheDir) == "" {
		return nil, os.ErrNotExist
	}
	return readPrivateCacheFile(r.client.CacheDir, cvmfsObjectCachePath(r.client.CacheDir, hash, suffix))
}

func (r *repository) writeCachedObject(hash, suffix string, data []byte) error {
	if r.client == nil || strings.TrimSpace(r.client.CacheDir) == "" {
		return nil
	}
	cachePath := cvmfsObjectCachePath(r.client.CacheDir, hash, suffix)
	return writeAtomicFile(cachePath, func(dst io.Writer) error {
		_, err := dst.Write(data)
		return err
	})
}

func (r *repository) objectURL(mirror, hash, suffix string) string {
	return fmt.Sprintf("%s/%s/data/%s/%s%s", mirror, r.repo, hash[:2], hash[2:], suffix)
}

func (c *Client) traceStart(op, target, url, cachePath string) (uint64, time.Time) {
	if c == nil || c.tracePath() == "" {
		return 0, time.Time{}
	}
	id := traceID.Add(1)
	started := time.Now()
	c.writeTrace(traceEvent{
		Time:      started.UTC().Format(time.RFC3339Nano),
		ID:        id,
		Event:     "start",
		Op:        op,
		Target:    target,
		URL:       url,
		CachePath: cachePath,
	})
	return id, started
}

func (c *Client) reportActivity(op string, bytes int, target string, url string, cachePath string) {
	if c == nil || c.OnActivity == nil {
		return
	}
	c.OnActivity(ActivityEvent{
		Time:      time.Now(),
		Op:        op,
		Bytes:     bytes,
		Target:    target,
		URL:       url,
		CachePath: cachePath,
	})
}

func (c *Client) activityReader(op string, target string, url string, cachePath string, reader io.Reader) io.Reader {
	if c == nil || c.OnActivity == nil {
		return reader
	}
	return &activityReader{
		reader: reader,
		onRead: func(n int) {
			c.reportActivity(op, n, target, url, cachePath)
		},
	}
}

type activityReader struct {
	reader io.Reader
	onRead func(int)
}

func (r *activityReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && r.onRead != nil {
		r.onRead(n)
	}
	return n, err
}

func (c *Client) traceDone(
	id uint64,
	started time.Time,
	op string,
	target string,
	url string,
	cachePath string,
	bytes int,
	statusCode int,
	err error,
) {
	if c == nil || id == 0 || c.tracePath() == "" {
		return
	}
	event := traceEvent{
		Time:       time.Now().UTC().Format(time.RFC3339Nano),
		ID:         id,
		Event:      "done",
		Op:         op,
		Target:     target,
		URL:        url,
		CachePath:  cachePath,
		Bytes:      bytes,
		StatusCode: statusCode,
		DurationMS: float64(time.Since(started).Microseconds()) / 1000.0,
	}
	if err != nil {
		event.Error = err.Error()
	}
	c.writeTrace(event)
}

func (c *Client) tracePath() string {
	if c == nil {
		return ""
	}
	if path := strings.TrimSpace(c.TraceLogPath); path != "" {
		return path
	}
	if path := strings.TrimSpace(os.Getenv("CCX3_CVMFS_LOG")); path != "" {
		return path
	}
	if cacheDir := strings.TrimSpace(c.CacheDir); cacheDir != "" {
		return filepath.Join(cacheDir, "requests.log")
	}
	return ""
}

func (c *Client) writeTrace(event traceEvent) {
	tracePath := c.tracePath()
	if tracePath == "" {
		return
	}
	cacheRoot := strings.TrimSpace(c.CacheDir)
	if cacheRoot != "" && pathWithinBase(cacheRoot, tracePath) {
		if err := ensurePrivateCacheDir(cacheRoot, filepath.Dir(tracePath)); err != nil {
			return
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(tracePath), 0o700); err != nil {
			return
		}
	}
	file, err := os.OpenFile(tracePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return
	}
	_ = json.NewEncoder(file).Encode(event)
}

func loadCatalog(db *intsqlite.SQLiteDatabase) (*catalog, error) {
	chunks, err := loadChunks(db)
	if err != nil {
		return nil, err
	}
	entries, err := loadEntries(db, chunks)
	if err != nil {
		return nil, err
	}
	nested, err := loadNestedCatalogs(db)
	if err != nil {
		return nil, err
	}
	return &catalog{entries: entries, nested: nested}, nil
}

func loadEntries(db *intsqlite.SQLiteDatabase, chunks map[string][]catalogChunk) ([]catalogEntry, error) {
	tbl, err := db.Table("catalog")
	if err != nil {
		return nil, err
	}
	cols := tableColumnNames(tbl.Sql)
	var out []catalogEntry
	if err := tbl.Read(func(row []any) error {
		byName := sliceToRowMap(cols, row)
		entry := catalogEntry{
			Md5Path1: asInt64(byName["md5path_1"]),
			Md5Path2: asInt64(byName["md5path_2"]),
			Parent1:  asInt64(byName["parent_1"]),
			Parent2:  asInt64(byName["parent_2"]),
			Hash:     asBytes(byName["hash"]),
			Size:     asInt64(byName["size"]),
			Mode:     asInt64(byName["mode"]),
			Mtime:    asInt64(byName["mtime"]),
			MtimeNS:  asInt64(byName["mtimens"]),
			Flags:    asInt64(byName["flags"]),
			Name:     asString(byName["name"]),
			Symlink:  asString(byName["symlink"]),
			UID:      asInt64(byName["uid"]),
			GID:      asInt64(byName["gid"]),
		}
		if entry.Flags&flagChunkedFile != 0 {
			entry.Chunks = chunks[entry.pathHash()]
			sort.Slice(entry.Chunks, func(i, j int) bool { return entry.Chunks[i].Offset < entry.Chunks[j].Offset })
		}
		out = append(out, entry)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func loadNestedCatalogs(db *intsqlite.SQLiteDatabase) ([]nestedCatalog, error) {
	tbl, err := db.Table("nested_catalogs")
	if _, ok := err.(intsqlite.ErrTableNotFound); ok {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cols := tableColumnNames(tbl.Sql)
	var out []nestedCatalog
	if err := tbl.Read(func(row []any) error {
		byName := sliceToRowMap(cols, row)
		out = append(out, nestedCatalog{
			Path: asString(byName["path"]),
			Sha1: asString(byName["sha1"]),
		})
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func loadChunks(db *intsqlite.SQLiteDatabase) (map[string][]catalogChunk, error) {
	tbl, err := db.Table("chunks")
	if _, ok := err.(intsqlite.ErrTableNotFound); ok {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cols := tableColumnNames(tbl.Sql)
	out := map[string][]catalogChunk{}
	if err := tbl.Read(func(row []any) error {
		byName := sliceToRowMap(cols, row)
		chunk := catalogChunk{
			Md5Path1: asInt64(byName["md5path_1"]),
			Md5Path2: asInt64(byName["md5path_2"]),
			Offset:   asInt64(byName["offset"]),
			Size:     asInt64(byName["size"]),
			Hash:     asBytes(byName["hash"]),
		}
		key := fmt.Sprintf("%x:%x", chunk.Md5Path1, chunk.Md5Path2)
		out[key] = append(out[key], chunk)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func asString(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case []byte:
		return string(val)
	default:
		return fmt.Sprint(val)
	}
}

func asInt64(v any) int64 {
	switch val := v.(type) {
	case nil:
		return 0
	case int64:
		return val
	case int:
		return int64(val)
	case int32:
		return int64(val)
	case int16:
		return int64(val)
	case int8:
		return int64(val)
	case uint64:
		return int64(val)
	case uint32:
		return int64(val)
	case uint16:
		return int64(val)
	case uint8:
		return int64(val)
	default:
		return 0
	}
}

func asBytes(v any) []byte {
	switch val := v.(type) {
	case nil:
		return nil
	case []byte:
		return append([]byte(nil), val...)
	case string:
		return []byte(val)
	default:
		return nil
	}
}

func sliceToRowMap(cols []string, values []any) map[string]any {
	out := make(map[string]any, len(cols))
	for i := range cols {
		if i < len(values) {
			out[cols[i]] = values[i]
		}
	}
	return out
}

func tableColumnNames(createSQL string) []string {
	start := strings.IndexByte(createSQL, '(')
	end := strings.LastIndexByte(createSQL, ')')
	if start == -1 || end == -1 || end <= start {
		return nil
	}
	defs := strings.Split(createSQL[start+1:end], ",")
	cols := make([]string, 0, len(defs))
	for _, def := range defs {
		def = strings.TrimSpace(def)
		if def == "" {
			continue
		}
		field := strings.Fields(def)
		if len(field) == 0 {
			continue
		}
		name := strings.Trim(field[0], "`\"[]")
		upper := strings.ToUpper(name)
		if upper == "PRIMARY" || upper == "UNIQUE" || upper == "CONSTRAINT" || upper == "FOREIGN" || upper == "CHECK" {
			continue
		}
		cols = append(cols, name)
	}
	return cols
}

func cleanRepoPath(escaped, raw string) string {
	if escaped != "" {
		if unescaped, err := url.PathUnescape(escaped); err == nil {
			return path.Clean("/" + strings.TrimPrefix(unescaped, "/"))
		}
	}
	return path.Clean("/" + strings.TrimPrefix(raw, "/"))
}

func hasCommonFragment(a, b string) bool {
	a = path.Clean("/" + strings.TrimPrefix(a, "/"))
	b = path.Clean("/" + strings.TrimPrefix(b, "/"))
	if strings.HasPrefix(a, b) {
		return true
	}
	return len(b) > len(a) && strings.HasPrefix(b, a+"/")
}

func shouldWalkNestedCatalog(nestedPath, prefix string) bool {
	nestedPath = path.Clean("/" + strings.TrimPrefix(nestedPath, "/"))
	prefix = path.Clean("/" + strings.TrimPrefix(prefix, "/"))
	if prefix == nestedPath {
		return true
	}
	return len(prefix) > len(nestedPath) && strings.HasPrefix(prefix, nestedPath+"/")
}

func shouldWalkNestedCatalogForWalk(nestedPath, prefix string) bool {
	nestedPath = path.Clean("/" + strings.TrimPrefix(nestedPath, "/"))
	prefix = path.Clean("/" + strings.TrimPrefix(prefix, "/"))
	return isWithinPrefix(nestedPath, prefix) || isWithinPrefix(prefix, nestedPath)
}

func isWithinPrefix(candidate, prefix string) bool {
	candidate = path.Clean("/" + strings.TrimPrefix(candidate, "/"))
	prefix = path.Clean("/" + strings.TrimPrefix(prefix, "/"))
	return candidate == prefix || strings.HasPrefix(candidate, prefix+"/")
}

func stringifyHash(hash []byte) string {
	return fmt.Sprintf("%x", hash)
}

func cvmfsFileCachePath(cacheDir, repo, filePath string) string {
	cacheDir = strings.TrimSpace(cacheDir)
	if cacheDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(repo + "\n" + path.Clean("/"+strings.TrimPrefix(filePath, "/"))))
	hexSum := hex.EncodeToString(sum[:])
	return filepath.Join(cacheDir, "files", repo, hexSum[:2], hexSum[2:4], hexSum[4:])
}

func cvmfsObjectCachePath(cacheDir, hash, suffix string) string {
	cacheDir = strings.TrimSpace(cacheDir)
	if cacheDir == "" {
		return ""
	}
	key := strings.ToLower(hash) + suffix
	if len(key) < 4 {
		return filepath.Join(cacheDir, "objects", key)
	}
	return filepath.Join(cacheDir, "objects", key[:2], key[2:4], key[4:])
}

func (c *Client) readCachedFile(parsed Target) ([]byte, error) {
	if strings.TrimSpace(c.CacheDir) == "" {
		return nil, os.ErrNotExist
	}
	return readPrivateCacheFile(c.CacheDir, cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path))
}

func (c *Client) writeCachedFile(parsed Target, data []byte) error {
	if strings.TrimSpace(c.CacheDir) == "" {
		return nil
	}
	cachePath := cvmfsFileCachePath(c.CacheDir, parsed.Repo, parsed.Path)
	return writeAtomicFile(cachePath, func(dst io.Writer) error {
		_, err := dst.Write(data)
		return err
	})
}

func writeAtomicFile(cachePath string, write func(dst io.Writer) error) error {
	tmp, err := createAtomicFile(cachePath)
	if err != nil {
		return err
	}
	if err := write(tmp); err != nil {
		_ = tmp.Abort()
		return fmt.Errorf("write cvmfs file cache temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return nil
}

type atomicFile struct {
	file       *os.File
	targetPath string
	closed     bool
	unlock     func()
}

func createAtomicFile(targetPath string) (*atomicFile, error) {
	cacheRoot, err := cacheRootForPath(targetPath)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateCacheDir(cacheRoot, filepath.Dir(targetPath)); err != nil {
		return nil, fmt.Errorf("create cvmfs cache dir: %w", err)
	}
	unlock, err := lockCachePublication(targetPath)
	if err != nil {
		return nil, fmt.Errorf("lock cvmfs cache entry: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			unlock()
		}
	}()
	tmp, err := os.CreateTemp(filepath.Dir(targetPath), ".cvmfs-cache-*")
	if err != nil {
		return nil, fmt.Errorf("create cvmfs cache temp file: %w", err)
	}
	failed = false
	return &atomicFile{file: tmp, targetPath: targetPath, unlock: unlock}, nil
}

func (a *atomicFile) Write(p []byte) (int, error) {
	return a.file.Write(p)
}

func (a *atomicFile) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	defer a.unlock()
	tmpPath := a.file.Name()
	if err := a.file.Sync(); err != nil {
		_ = a.file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync cvmfs cache temp file: %w", err)
	}
	if err := a.file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close cvmfs cache temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("secure cvmfs cache temp file: %w", err)
	}
	if err := os.Rename(tmpPath, a.targetPath); err != nil {
		_ = os.Remove(tmpPath)
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("commit cvmfs cache: %w", err)
	}
	return nil
}

func (a *atomicFile) Abort() error {
	if a.closed {
		return nil
	}
	a.closed = true
	defer a.unlock()
	tmpPath := a.file.Name()
	closeErr := a.file.Close()
	removeErr := os.Remove(tmpPath)
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

func lockCachePublication(path string) (func(), error) {
	path = filepath.Clean(path)
	cachePublicationLocks.Lock()
	lock := cachePublicationLocks.locks[path]
	if lock == nil {
		lock = &cachePublicationLock{}
		cachePublicationLocks.locks[path] = lock
	}
	lock.refs++
	cachePublicationLocks.Unlock()

	lock.mu.Lock()
	unlockFile, err := lockCacheFile(path + ".lock")
	if err != nil {
		lock.mu.Unlock()
		cachePublicationLocks.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(cachePublicationLocks.locks, path)
		}
		cachePublicationLocks.Unlock()
		return nil, err
	}
	return func() {
		unlockFile()
		lock.mu.Unlock()
		cachePublicationLocks.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(cachePublicationLocks.locks, path)
		}
		cachePublicationLocks.Unlock()
	}, nil
}

func cacheRootForPath(targetPath string) (string, error) {
	clean := filepath.Clean(targetPath)
	for dir := filepath.Dir(clean); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		switch filepath.Base(dir) {
		case "files", "objects", "state":
			return filepath.Dir(dir), nil
		}
	}
	return "", fmt.Errorf("CVMFS cache path %q is outside a cache namespace", targetPath)
}

func ensurePrivateCacheDir(cacheRoot, targetDir string) error {
	cacheRoot = filepath.Clean(cacheRoot)
	targetDir = filepath.Clean(targetDir)
	if !pathWithinBase(cacheRoot, targetDir) {
		return fmt.Errorf("cache directory %q escapes root %q", targetDir, cacheRoot)
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return err
	}
	if err := validatePrivateCacheDir(cacheRoot); err != nil {
		return err
	}
	rel, err := filepath.Rel(cacheRoot, targetDir)
	if err != nil {
		return err
	}
	current := cacheRoot
	if rel == "." {
		return nil
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := validatePrivateCacheDir(current); err != nil {
			return err
		}
	}
	return nil
}

func validatePrivateCacheDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("CVMFS cache path %q is not a directory", dir)
	}
	if err := validateCacheOwner(info); err != nil {
		return fmt.Errorf("validate CVMFS cache directory %q: %w", dir, err)
	}
	if err := secureCacheDirectoryMode(dir, info); err != nil {
		return fmt.Errorf("secure CVMFS cache directory %q: %w", dir, err)
	}
	return nil
}

func readPrivateCacheFile(cacheRoot, cachePath string) ([]byte, error) {
	file, err := openPrivateCacheFile(cacheRoot, cachePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func openPrivateCacheFile(cacheRoot, cachePath string) (*os.File, error) {
	if strings.TrimSpace(cacheRoot) == "" || strings.TrimSpace(cachePath) == "" {
		return nil, os.ErrNotExist
	}
	if err := ensurePrivateCacheDir(cacheRoot, filepath.Dir(cachePath)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(cachePath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("CVMFS cache entry %q is not a regular file", cachePath)
	}
	if err := validateCacheOwner(info); err != nil {
		return nil, fmt.Errorf("validate CVMFS cache entry %q: %w", cachePath, err)
	}
	if err := validateCacheFileMode(cachePath, info); err != nil {
		return nil, err
	}
	file, err := os.Open(cachePath)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("CVMFS cache entry %q changed while opening", cachePath)
	}
	return file, nil
}

func privateCacheFileSize(cacheRoot, cachePath string) (int64, error) {
	file, err := openPrivateCacheFile(cacheRoot, cachePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func pathWithinBase(base, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type countingWriter struct {
	w       io.Writer
	n       int64
	onWrite func(int)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	if n > 0 && w.onWrite != nil {
		w.onWrite(n)
	}
	return n, err
}

type limitedWriter struct {
	w io.Writer
	n int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, nil
	}
	if int64(len(p)) > w.n {
		p = p[:w.n]
	}
	n, err := w.w.Write(p)
	w.n -= int64(n)
	return n, err
}

func (ent catalogEntry) pathHash() string {
	return fmt.Sprintf("%x:%x", ent.Md5Path1, ent.Md5Path2)
}

func (ent catalogEntry) parentHash() string {
	return fmt.Sprintf("%x:%x", ent.Parent1, ent.Parent2)
}

func (ent catalogEntry) isDir() bool {
	return ent.Flags&flagDir != 0
}

func (ent catalogEntry) isSymlink() bool {
	return ent.Flags&flagSymlink != 0
}

func (ent catalogEntry) isChunked() bool {
	return ent.Flags&flagChunkedFile != 0
}

func linuxModeToGo(mode uint32) fs.FileMode {
	out := fs.FileMode(mode & linuxPermMask)
	switch mode & linuxSIFMT {
	case linuxSIFDIR:
		out |= fs.ModeDir
	case linuxSIFLNK:
		out |= fs.ModeSymlink
	case linuxSIFBLK:
		out |= fs.ModeDevice
	case linuxSIFCHR:
		out |= fs.ModeDevice | fs.ModeCharDevice
	case linuxSIFIFO:
		out |= fs.ModeNamedPipe
	case linuxSIFSOCK:
		out |= fs.ModeSocket
	case linuxSIFREG:
	}
	return out
}
