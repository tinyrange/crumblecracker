package ubuntu

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/tinyrange/crumblecracker/internal/core/download"
	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
)

const (
	defaultMirror  = "https://ports.ubuntu.com/ubuntu-ports"
	defaultSuite   = "noble-updates"
	defaultRepo    = "main"
	indexCacheAge  = 24 * time.Hour
	virtualPackage = "linux-image-virtual"
)

type Tristate = alpine.Tristate

const (
	TristateNo     = alpine.TristateNo
	TristateYes    = alpine.TristateYes
	TristateModule = alpine.TristateModule
)

type Manager struct {
	root       string
	mirror     string
	suite      string
	repo       string
	arch       string
	httpClient *http.Client

	mu           sync.Mutex
	kernelBytes  []byte
	kernelConfig map[string]Tristate
	modulePaths  map[string]string
	moduleFiles  map[string][]byte
}

type packageEntry struct {
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Arch          string   `json:"arch"`
	Filename      string   `json:"filename"`
	Depends       []string `json:"depends,omitempty"`
	RawDepends    string   `json:"raw_depends,omitempty"`
	InstalledSize string   `json:"installed_size,omitempty"`
	Size          int64    `json:"size,omitempty"`
	SHA256        string   `json:"sha256,omitempty"`
}

type resolvedPackages struct {
	ImagePackage   packageEntry
	ModulesPackage packageEntry
	KernelVersion  string
}

func NewManager(root string) *Manager {
	return &Manager{
		root:         root,
		mirror:       defaultMirror,
		suite:        defaultSuite,
		repo:         defaultRepo,
		arch:         defaultArch(),
		httpClient:   http.DefaultClient,
		kernelConfig: map[string]Tristate{},
		modulePaths:  map[string]string{},
		moduleFiles:  map[string][]byte{},
	}
}

func defaultArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	case "amd64":
		return "amd64"
	default:
		return runtime.GOARCH
	}
}

func (m *Manager) ReadKernel() ([]byte, error) {
	m.mu.Lock()
	if len(m.kernelBytes) != 0 {
		data := append([]byte(nil), m.kernelBytes...)
		m.mu.Unlock()
		return data, nil
	}
	m.mu.Unlock()

	pkgs, err := m.resolvePackages(context.Background())
	if err != nil {
		return nil, err
	}
	debPath, err := m.ensurePackage(context.Background(), pkgs.ImagePackage)
	if err != nil {
		return nil, err
	}
	data, err := extractDebTarFile(debPath, func(name string) bool {
		return strings.HasPrefix(trimTarName(name), "boot/vmlinuz-")
	})
	if err != nil {
		return nil, fmt.Errorf("extract Ubuntu kernel image: %w", err)
	}
	m.mu.Lock()
	m.kernelBytes = append(m.kernelBytes[:0], data...)
	cached := append([]byte(nil), m.kernelBytes...)
	m.mu.Unlock()
	return cached, nil
}

func (m *Manager) PlanModuleLoad(configVars []string, moduleMap map[string]string) ([]alpine.Module, error) {
	config, err := m.readKernelConfig()
	if err != nil {
		return nil, err
	}
	var planned []alpine.Module
	seen := map[string]bool{}
	var loadModule func(string) error
	loadModule = func(name string) error {
		path, err := m.findModuleFile(name)
		if err != nil {
			return err
		}
		if seen[path] {
			return nil
		}
		seen[path] = true
		data, err := m.readModuleFile(path)
		if err != nil {
			return fmt.Errorf("read module %q: %w", path, err)
		}
		info, err := parseModInfo(data)
		if err != nil {
			return fmt.Errorf("parse module info %q: %w", path, err)
		}
		for _, dep := range moduleDepends(info) {
			if err := loadModule(dep); err != nil {
				return err
			}
		}
		planned = append(planned, alpine.Module{Name: moduleName(path), Data: data})
		return nil
	}

	for _, configVar := range configVars {
		if strings.HasPrefix(configVar, "MODULE:") {
			moduleName, ok := moduleMap[configVar]
			if !ok {
				return nil, fmt.Errorf("no module mapping for %q", configVar)
			}
			if err := loadModule(moduleName); err != nil {
				return nil, err
			}
			continue
		}
		state, ok := config[configVar]
		if !ok {
			return nil, fmt.Errorf("kernel config %q not found", configVar)
		}
		switch state {
		case TristateYes:
		case TristateNo:
			return nil, fmt.Errorf("required kernel config %q is disabled", configVar)
		case TristateModule:
			moduleName, ok := moduleMap[configVar]
			if !ok {
				return nil, fmt.Errorf("no module mapping for %q", configVar)
			}
			if err := loadModule(moduleName); err != nil {
				return nil, err
			}
		}
	}
	return planned, nil
}

func (m *Manager) readKernelConfig() (map[string]Tristate, error) {
	m.mu.Lock()
	if len(m.kernelConfig) != 0 {
		out := make(map[string]Tristate, len(m.kernelConfig))
		for k, v := range m.kernelConfig {
			out[k] = v
		}
		m.mu.Unlock()
		return out, nil
	}
	m.mu.Unlock()

	pkgs, err := m.resolvePackages(context.Background())
	if err != nil {
		return nil, err
	}
	debPath, err := m.ensurePackage(context.Background(), pkgs.ModulesPackage)
	if err != nil {
		return nil, err
	}
	data, err := extractDebTarFile(debPath, func(name string) bool {
		return strings.HasPrefix(trimTarName(name), "boot/config-")
	})
	if err != nil {
		return nil, fmt.Errorf("extract Ubuntu kernel config: %w", err)
	}
	config, err := parseKernelConfig(data)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.kernelConfig = make(map[string]Tristate, len(config))
	for k, v := range config {
		m.kernelConfig[k] = v
	}
	out := make(map[string]Tristate, len(m.kernelConfig))
	for k, v := range m.kernelConfig {
		out[k] = v
	}
	m.mu.Unlock()
	return out, nil
}

func parseKernelConfig(data []byte) (map[string]Tristate, error) {
	config := map[string]Tristate{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "# CONFIG_") && strings.HasSuffix(line, " is not set"):
			key := strings.TrimSuffix(strings.TrimPrefix(line, "# "), " is not set")
			config[key] = TristateNo
		case strings.HasPrefix(line, "CONFIG_"):
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch value {
			case "y":
				config[key] = TristateYes
			case "m":
				config[key] = TristateModule
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan kernel config: %w", err)
	}
	return config, nil
}

func (m *Manager) findModuleFile(name string) (string, error) {
	rawName := strings.TrimSpace(name)
	rawSuffix := strings.TrimPrefix(trimTarName(rawName), "/")
	name = normalizeModuleName(rawName)
	m.mu.Lock()
	if path := m.modulePaths[name]; path != "" {
		m.mu.Unlock()
		return path, nil
	}
	m.mu.Unlock()

	paths, err := m.listModulePaths()
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		if rawSuffix != "" && strings.HasSuffix(path, rawSuffix) {
			m.mu.Lock()
			m.modulePaths[name] = path
			m.mu.Unlock()
			return path, nil
		}
		base := moduleName(path)
		if base == name || strings.HasSuffix(path, name) {
			m.mu.Lock()
			m.modulePaths[name] = path
			m.mu.Unlock()
			return path, nil
		}
	}
	return "", fmt.Errorf("Ubuntu kernel module %q not found", name)
}

func (m *Manager) listModulePaths() ([]string, error) {
	pkgs, err := m.resolvePackages(context.Background())
	if err != nil {
		return nil, err
	}
	debPath, err := m.ensurePackage(context.Background(), pkgs.ModulesPackage)
	if err != nil {
		return nil, err
	}
	var paths []string
	err = scanDebTar(debPath, func(name string, _ *tar.Header, _ io.Reader) error {
		name = trimTarName(name)
		if strings.HasPrefix(name, "lib/modules/") && (strings.HasSuffix(name, ".ko") || strings.HasSuffix(name, ".ko.zst") || strings.HasSuffix(name, ".ko.xz") || strings.HasSuffix(name, ".ko.gz")) {
			paths = append(paths, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	return paths, nil
}

func (m *Manager) readModuleFile(path string) ([]byte, error) {
	m.mu.Lock()
	if data := m.moduleFiles[path]; len(data) != 0 {
		out := append([]byte(nil), data...)
		m.mu.Unlock()
		return out, nil
	}
	m.mu.Unlock()

	pkgs, err := m.resolvePackages(context.Background())
	if err != nil {
		return nil, err
	}
	debPath, err := m.ensurePackage(context.Background(), pkgs.ModulesPackage)
	if err != nil {
		return nil, err
	}
	compressed, err := extractDebTarFile(debPath, func(name string) bool {
		return trimTarName(name) == path
	})
	if err != nil {
		return nil, err
	}
	data, err := decompressModule(path, compressed)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.moduleFiles[path] = append([]byte(nil), data...)
	out := append([]byte(nil), m.moduleFiles[path]...)
	m.mu.Unlock()
	return out, nil
}

func decompressModule(path string, data []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(path, ".zst"):
		dec, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open zstd module %q: %w", path, err)
		}
		defer dec.Close()
		return download.ReadAllReader(context.Background(), dec, 256<<20)
	case strings.HasSuffix(path, ".xz"):
		dec, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open xz module %q: %w", path, err)
		}
		return download.ReadAllReader(context.Background(), dec, 256<<20)
	case strings.HasSuffix(path, ".gz"):
		dec, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open gzip module %q: %w", path, err)
		}
		defer dec.Close()
		return download.ReadAllReader(context.Background(), dec, 256<<20)
	default:
		return data, nil
	}
}

type modInfo map[string][]string

func parseModInfo(data []byte) (modInfo, error) {
	file, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	sect := file.Section(".modinfo")
	if sect == nil {
		return nil, fmt.Errorf(".modinfo section not found")
	}
	sectData, err := sect.Data()
	if err != nil {
		return nil, err
	}
	info := modInfo{}
	for _, token := range strings.Split(string(sectData), "\x00") {
		key, value, ok := strings.Cut(token, "=")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		info[key] = append(info[key], strings.TrimSpace(value))
	}
	return info, nil
}

func moduleDepends(info modInfo) []string {
	var deps []string
	for _, raw := range info["depends"] {
		for _, dep := range strings.Split(raw, ",") {
			dep = normalizeModuleName(dep)
			if dep != "" {
				deps = append(deps, dep)
			}
		}
	}
	return deps
}

func moduleName(path string) string {
	base := filepath.Base(path)
	for _, suffix := range []string{".ko.zst", ".ko.xz", ".ko.gz", ".ko"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return normalizeModuleName(base)
}

func normalizeModuleName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".ko.zst")
	name = strings.TrimSuffix(name, ".ko.xz")
	name = strings.TrimSuffix(name, ".ko.gz")
	name = strings.TrimSuffix(name, ".ko")
	return strings.ReplaceAll(name, "_", "-")
}

func (m *Manager) resolvePackages(ctx context.Context) (resolvedPackages, error) {
	index, err := m.packageIndex(ctx)
	if err != nil {
		return resolvedPackages{}, err
	}
	meta, ok := index[virtualPackage]
	if !ok {
		return resolvedPackages{}, fmt.Errorf("Ubuntu package %q not found", virtualPackage)
	}
	imageName := firstDependencyNamed(meta.Depends, "linux-image-")
	if imageName == "" {
		return resolvedPackages{}, fmt.Errorf("%s has no linux-image dependency", virtualPackage)
	}
	imagePkg, ok := index[imageName]
	if !ok {
		return resolvedPackages{}, fmt.Errorf("Ubuntu package %q not found", imageName)
	}
	modulesName := firstDependencyNamed(imagePkg.Depends, "linux-modules-")
	if modulesName == "" {
		return resolvedPackages{}, fmt.Errorf("%s has no linux-modules dependency", imagePkg.Name)
	}
	modulesPkg, ok := index[modulesName]
	if !ok {
		return resolvedPackages{}, fmt.Errorf("Ubuntu package %q not found", modulesName)
	}
	return resolvedPackages{
		ImagePackage:   imagePkg,
		ModulesPackage: modulesPkg,
		KernelVersion:  strings.TrimPrefix(modulesName, "linux-modules-"),
	}, nil
}

func firstDependencyNamed(deps []string, prefix string) string {
	for _, dep := range deps {
		for _, alt := range strings.Split(dep, "|") {
			name := strings.TrimSpace(alt)
			if i := strings.IndexAny(name, " ("); i >= 0 {
				name = name[:i]
			}
			if strings.HasPrefix(name, prefix) {
				return name
			}
		}
	}
	return ""
}

func (m *Manager) packageIndex(ctx context.Context) (map[string]packageEntry, error) {
	if index, ok, err := m.loadIndexCache(); err != nil {
		return nil, err
	} else if ok {
		return index, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.packagesURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Ubuntu package index: %w", err)
	}
	defer resp.Body.Close()
	if err := download.BoundResponse(resp, 64<<20); err != nil {
		return nil, fmt.Errorf("request Ubuntu package index: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request Ubuntu package index: status %s", resp.Status)
	}
	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("open Ubuntu package index gzip: %w", err)
	}
	defer gzr.Close()
	index, err := parsePackagesIndex(gzr)
	if err != nil {
		return nil, err
	}
	if err := m.saveIndexCache(index); err != nil {
		return nil, err
	}
	return index, nil
}

func parsePackagesIndex(r io.Reader) (map[string]packageEntry, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	index := map[string]packageEntry{}
	fields := map[string]string{}
	lastKey := ""
	flush := func() {
		if fields["Package"] == "" {
			fields = map[string]string{}
			lastKey = ""
			return
		}
		depends := parseDepends(fields["Depends"])
		entry := packageEntry{
			Name:          fields["Package"],
			Version:       fields["Version"],
			Arch:          fields["Architecture"],
			Filename:      fields["Filename"],
			Depends:       depends,
			RawDepends:    fields["Depends"],
			InstalledSize: fields["Installed-Size"],
			SHA256:        fields["SHA256"],
		}
		entry.Size, _ = strconv.ParseInt(fields["Size"], 10, 64)
		if entry.Name != "" && entry.Filename != "" {
			index[entry.Name] = entry
		}
		fields = map[string]string{}
		lastKey = ""
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if lastKey != "" {
				fields[lastKey] += "\n" + strings.TrimSpace(line)
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		lastKey = key
		fields[key] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan Ubuntu package index: %w", err)
	}
	flush()
	return index, nil
}

func parseDepends(raw string) []string {
	var deps []string
	for _, dep := range strings.Split(raw, ",") {
		dep = strings.TrimSpace(dep)
		if dep != "" {
			deps = append(deps, dep)
		}
	}
	return deps
}

func (m *Manager) ensurePackage(ctx context.Context, entry packageEntry) (string, error) {
	if entry.Arch != "" && entry.Arch != m.arch && entry.Arch != "all" {
		return "", fmt.Errorf("Ubuntu package %s arch %q does not match %q", entry.Name, entry.Arch, m.arch)
	}
	dest := filepath.Join(m.root, "packages", filepath.Base(entry.Filename))
	if info, err := os.Stat(dest); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return dest, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("stat Ubuntu package %q: %w", entry.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create Ubuntu package cache: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.packageURL(entry.Filename), nil)
	if err != nil {
		return "", err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download Ubuntu package %q: %w", entry.Name, err)
	}
	defer resp.Body.Close()
	if err := download.BoundResponse(resp, 0); err != nil {
		return "", fmt.Errorf("download Ubuntu package %q: %w", entry.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download Ubuntu package %q: status %s", entry.Name, resp.Status)
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", fmt.Errorf("create Ubuntu package %q: %w", entry.Name, err)
	}
	budget := download.Budget{MaxBytes: resp.ContentLength, ExpectedBytes: resp.ContentLength}
	if entry.Size > 0 {
		budget.MaxBytes = entry.Size
		budget.ExpectedBytes = entry.Size
	}
	if entry.SHA256 != "" {
		budget.ExpectedSHA256 = entry.SHA256
	}
	if _, err := download.Copy(ctx, f, resp, budget); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("write Ubuntu package %q: %w", entry.Name, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("close Ubuntu package %q: %w", entry.Name, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("finalize Ubuntu package %q: %w", entry.Name, err)
	}
	return dest, nil
}

func (m *Manager) packagesURL() string {
	return trimRightSlash(m.mirror) + "/dists/" + m.suite + "/" + m.repo + "/binary-" + m.arch + "/Packages.gz"
}

func (m *Manager) packageURL(filename string) string {
	return trimRightSlash(m.mirror) + "/" + strings.TrimLeft(filename, "/")
}

func (m *Manager) indexCachePath() string {
	return filepath.Join(m.root, "indexes", m.suite, m.repo, m.arch, "Packages.json")
}

func (m *Manager) loadIndexCache() (map[string]packageEntry, bool, error) {
	path := m.indexCachePath()
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat Ubuntu package index cache: %w", err)
	}
	if time.Since(info.ModTime()) > indexCacheAge {
		return nil, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open Ubuntu package index cache: %w", err)
	}
	defer f.Close()
	var index map[string]packageEntry
	if err := json.NewDecoder(f).Decode(&index); err != nil {
		return nil, false, fmt.Errorf("decode Ubuntu package index cache: %w", err)
	}
	if len(index) == 0 {
		return nil, false, fmt.Errorf("Ubuntu package index cache is empty")
	}
	return index, true, nil
}

func (m *Manager) saveIndexCache(index map[string]packageEntry) error {
	path := m.indexCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create Ubuntu package index cache dir: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create Ubuntu package index cache: %w", err)
	}
	if err := json.NewEncoder(f).Encode(index); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write Ubuntu package index cache: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close Ubuntu package index cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalize Ubuntu package index cache: %w", err)
	}
	return nil
}

func trimRightSlash(value string) string {
	return strings.TrimRight(value, "/")
}

type debMember struct {
	Name   string
	Offset int64
	Size   int64
}

func extractDebTarFile(debPath string, match func(string) bool) ([]byte, error) {
	var out []byte
	err := scanDebTar(debPath, func(name string, hdr *tar.Header, r io.Reader) error {
		if hdr.Typeflag != tar.TypeReg || !match(name) {
			return nil
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		out = data
		return io.EOF
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("file not found")
	}
	return out, nil
}

func scanDebTar(debPath string, visit func(string, *tar.Header, io.Reader) error) error {
	f, err := os.Open(debPath)
	if err != nil {
		return fmt.Errorf("open deb package: %w", err)
	}
	defer f.Close()
	member, err := findDebDataMember(f)
	if err != nil {
		return err
	}
	section := io.NewSectionReader(f, member.Offset, member.Size)
	r, closeReader, err := debDataReader(member.Name, section)
	if err != nil {
		return err
	}
	if closeReader != nil {
		defer closeReader()
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read deb data tar: %w", err)
		}
		if err := visit(hdr.Name, hdr, tr); err != nil {
			return err
		}
	}
}

func findDebDataMember(f *os.File) (debMember, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return debMember{}, err
	}
	var magic [8]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return debMember{}, fmt.Errorf("read deb magic: %w", err)
	}
	if string(magic[:]) != "!<arch>\n" {
		return debMember{}, fmt.Errorf("invalid deb package")
	}
	for {
		offset, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return debMember{}, err
		}
		var hdr [60]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return debMember{}, fmt.Errorf("deb data archive not found")
			}
			return debMember{}, fmt.Errorf("read deb member header: %w", err)
		}
		if string(hdr[58:60]) != "`\n" {
			return debMember{}, fmt.Errorf("invalid deb member header")
		}
		name := strings.TrimSpace(string(hdr[0:16]))
		name = strings.TrimSuffix(name, "/")
		sizeRaw := strings.TrimSpace(string(hdr[48:58]))
		var size int64
		if _, err := fmt.Sscanf(sizeRaw, "%d", &size); err != nil {
			return debMember{}, fmt.Errorf("parse deb member size %q: %w", sizeRaw, err)
		}
		dataOffset := offset + 60
		if strings.HasPrefix(name, "data.tar") {
			return debMember{Name: name, Offset: dataOffset, Size: size}, nil
		}
		next := dataOffset + size
		if size%2 != 0 {
			next++
		}
		if _, err := f.Seek(next, io.SeekStart); err != nil {
			return debMember{}, err
		}
	}
}

func debDataReader(name string, r io.Reader) (io.Reader, func(), error) {
	switch {
	case strings.HasSuffix(name, ".zst"):
		dec, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("open zstd deb data: %w", err)
		}
		return dec, dec.Close, nil
	case strings.HasSuffix(name, ".xz"):
		dec, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("open xz deb data: %w", err)
		}
		return dec, nil, nil
	case strings.HasSuffix(name, ".gz"):
		dec, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("open gzip deb data: %w", err)
		}
		return dec, func() { _ = dec.Close() }, nil
	default:
		return r, nil, nil
	}
}

func trimTarName(name string) string {
	name = strings.TrimPrefix(name, "./")
	return filepath.ToSlash(name)
}
