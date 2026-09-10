package alpine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/download"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

var debugTiming = strings.TrimSpace(os.Getenv("CCX3_DEBUG_TIMING")) != ""

func timingLog(format string, args ...any) {
	if !debugTiming {
		return
	}
	fmt.Fprintf(os.Stderr, "ccx3 timing: "+format+"\n", args...)
}

const (
	defaultMirror  = "https://dl-cdn.alpinelinux.org"
	defaultVersion = "latest-stable"
	defaultRepo    = "main"
)

type Manager struct {
	root        string
	mirror      string
	version     string
	repo        string
	arch        string
	packageName string
	httpClient  *http.Client

	mu           sync.Mutex
	downloading  bool
	lastErr      error
	kernelBytes  []byte
	kernelConfig map[string]Tristate
	moduleDeps   map[string][]string
	modulePrefix string
	packageFiles map[string][]byte
	modulePaths  map[string]string
	packageIndex map[string]tarIndexEntry
	repoIndexes  map[string]map[string]indexEntry
	tarIndexes   map[string]map[string]tarIndexEntry
	indexedTar   string
}

type metadata struct {
	Version           string `json:"version"`
	Source            string `json:"source"`
	PackageName       string `json:"package_name"`
	PackageFile       string `json:"package_file"`
	ModuleSymversFile string `json:"module_symvers_file,omitempty"`
	Arch              string `json:"arch"`
}

type KernelMetadata struct {
	Release       string
	ModuleSymvers []byte
}

type indexEntry struct {
	Name     string
	Version  string
	Arch     string
	Size     int64
	Checksum string
}

type tarIndexEntry struct {
	Offset int64
	Size   int64
	Mode   int64
}

type Tristate int

const (
	TristateNo Tristate = iota
	TristateYes
	TristateModule
)

type Module struct {
	Name string
	Data []byte
}

type progressReporter func(client.ProgressEvent)

func NewManager(root string) *Manager {
	return &Manager{
		root:         root,
		mirror:       defaultMirror,
		version:      defaultVersion,
		repo:         defaultRepo,
		arch:         defaultArch(),
		packageName:  defaultPackageName(),
		httpClient:   http.DefaultClient,
		packageFiles: map[string][]byte{},
		modulePaths:  map[string]string{},
		packageIndex: map[string]tarIndexEntry{},
		repoIndexes:  map[string]map[string]indexEntry{},
		tarIndexes:   map[string]map[string]tarIndexEntry{},
	}
}

func (m *Manager) Status() client.KernelState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

func (m *Manager) PackagePath() (string, error) {
	meta, err := m.readMetadata()
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(meta.PackageFile, ".tar") {
		return meta.PackageFile, nil
	}
	if !strings.HasSuffix(meta.PackageFile, ".apk") {
		return meta.PackageFile, nil
	}
	tarPath := strings.TrimSuffix(meta.PackageFile, ".apk") + ".tar"
	if _, err := os.Stat(tarPath); err == nil {
		meta.PackageFile = tarPath
		if err := m.writeMetadata(meta); err != nil {
			return "", err
		}
		return tarPath, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat kernel tar package: %w", err)
	}
	if err := decompressAPKToTar(meta.PackageFile, tarPath); err != nil {
		return "", err
	}
	meta.PackageFile = tarPath
	if err := m.writeMetadata(meta); err != nil {
		return "", err
	}
	return tarPath, nil
}

func (m *Manager) ReadKernel() ([]byte, error) {
	m.mu.Lock()
	if len(m.kernelBytes) > 0 {
		data := append([]byte(nil), m.kernelBytes...)
		m.mu.Unlock()
		return data, nil
	}
	m.mu.Unlock()

	meta, err := m.readMetadata()
	if err != nil {
		return nil, err
	}
	candidates := []string{"boot/vmlinuz-virt", "boot/vmlinuz-lts"}
	for _, candidate := range candidates {
		data, err := m.readRawPackageFile(candidate)
		if err == nil {
			m.mu.Lock()
			m.kernelBytes = append(m.kernelBytes[:0], data...)
			cached := append([]byte(nil), m.kernelBytes...)
			m.mu.Unlock()
			return cached, nil
		}
	}
	return nil, fmt.Errorf("kernel image not found in package %s", meta.PackageFile)
}

func (m *Manager) KernelVersion() (string, error) {
	path, err := m.findModuleFile("modules.dep")
	if err != nil {
		return "", err
	}
	parts := strings.Split(path, "/")
	if len(parts) < 4 {
		return "", fmt.Errorf("unexpected modules.dep path %q", path)
	}
	return parts[len(parts)-2], nil
}

func (m *Manager) ReadKernelMetadata() (KernelMetadata, error) {
	meta, err := m.readMetadata()
	if err != nil {
		return KernelMetadata{}, err
	}
	if strings.TrimSpace(meta.ModuleSymversFile) == "" {
		return KernelMetadata{}, fmt.Errorf("matching Module.symvers is not cached for kernel %s", meta.Version)
	}
	data, err := os.ReadFile(meta.ModuleSymversFile)
	if err != nil {
		return KernelMetadata{}, fmt.Errorf("read Module.symvers for kernel %s: %w", meta.Version, err)
	}
	if len(data) == 0 {
		return KernelMetadata{}, fmt.Errorf("Module.symvers for kernel %s is empty", meta.Version)
	}
	if err := validateModuleSymvers(data); err != nil {
		return KernelMetadata{}, fmt.Errorf("validate Module.symvers for kernel %s: %w", meta.Version, err)
	}
	release, err := m.KernelVersion()
	if err != nil {
		return KernelMetadata{}, err
	}
	return KernelMetadata{Release: release, ModuleSymvers: data}, nil
}

func (m *Manager) PlanModuleLoad(configVars []string, moduleMap map[string]string) ([]Module, error) {
	start := time.Now()
	config, err := m.readKernelConfig()
	if err != nil {
		return nil, err
	}
	timingLog("kernel.PlanModuleLoad readKernelConfig took=%s", time.Since(start))
	depends, prefix, err := m.readModuleDependencies()
	if err != nil {
		return nil, err
	}
	timingLog("kernel.PlanModuleLoad readModuleDependencies took=%s", time.Since(start))

	var planned []Module
	seen := map[string]bool{}
	var loadModule func(string) error
	loadModule = func(name string) error {
		if seen[name] {
			return nil
		}
		seen[name] = true

		for _, dep := range depends[name] {
			if err := loadModule(dep); err != nil {
				return err
			}
		}

		moduleStart := time.Now()
		data, err := m.readModuleFile(prefix + name)
		if err != nil {
			return fmt.Errorf("read module %q: %w", name, err)
		}
		timingLog("kernel.PlanModuleLoad readModuleFile module=%q took=%s bytes=%d", name, time.Since(moduleStart), len(data))
		planned = append(planned, Module{Name: strings.TrimSuffix(filepath.Base(name), ".ko.gz"), Data: data})
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

	timingLog("kernel.PlanModuleLoad total=%s modules=%d", time.Since(start), len(planned))
	return planned, nil
}

func (m *Manager) Ensure(ctx context.Context) error {
	return m.EnsureWithProgress(ctx, nil)
}

func (m *Manager) EnsureWithProgress(ctx context.Context, report progressReporter) error {
	m.mu.Lock()
	if m.downloading {
		m.mu.Unlock()
		return fmt.Errorf("kernel download already in progress")
	}
	m.downloading = true
	m.lastErr = nil
	m.mu.Unlock()

	err := m.ensureDownloaded(ctx, report)

	m.mu.Lock()
	m.downloading = false
	m.lastErr = err
	m.mu.Unlock()

	return err
}

func (m *Manager) statusLocked() client.KernelState {
	if m.downloading {
		return client.KernelState{
			Status: "downloading",
			Source: m.sourceName(),
		}
	}

	meta, err := m.readMetadata()
	if err == nil && strings.TrimSpace(meta.ModuleSymversFile) != "" {
		if _, statErr := os.Stat(meta.PackageFile); statErr != nil {
			err = statErr
		} else if _, statErr := os.Stat(meta.ModuleSymversFile); statErr != nil {
			err = statErr
		}
	}
	if err == nil && strings.TrimSpace(meta.ModuleSymversFile) != "" {
		return client.KernelState{
			Status:  "downloaded",
			Version: meta.Version,
			Source:  meta.Source,
		}
	}
	if m.lastErr != nil {
		return client.KernelState{
			Status: "error",
			Error:  m.lastErr.Error(),
			Source: m.sourceName(),
		}
	}
	return client.KernelState{
		Status: "missing",
		Source: m.sourceName(),
	}
}

func (m *Manager) ensureDownloaded(ctx context.Context, report progressReporter) error {
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return fmt.Errorf("create kernel cache dir: %w", err)
	}
	if meta, err := m.readMetadata(); err == nil {
		_, packageErr := os.Stat(meta.PackageFile)
		_, symversErr := os.Stat(meta.ModuleSymversFile)
		if packageErr == nil && strings.TrimSpace(meta.ModuleSymversFile) != "" && symversErr == nil {
			return nil
		}
	}

	destDir := filepath.Join(m.root, "packages")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create kernel package dir: %w", err)
	}
	var entry, developmentEntry indexEntry
	var apkPath string
	var developmentAPKPath string
	var downloadErr error
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		entry, developmentEntry, err = m.fetchIndexEntries(ctx, report)
		if err != nil {
			return err
		}
		if report != nil {
			for _, planned := range []indexEntry{entry, developmentEntry} {
				artifact := fmt.Sprintf("%s-%s.apk", planned.Name, planned.Version)
				kind := "kernel"
				if planned.Name != m.packageName {
					kind = "dependency"
				}
				report(client.ProgressEvent{Artifact: artifact, Transfer: &client.TransferProgress{ID: artifact, Kind: kind, State: "queued", Total: planned.Size}})
			}
			report(client.ProgressEvent{PlanningComplete: true})
		}
		filename := fmt.Sprintf("%s-%s.apk", entry.Name, entry.Version)
		apkPath = filepath.Join(destDir, filename)
		packageURL := m.packageURL(filename)
		if attempt > 0 {
			separator := "?"
			if strings.Contains(packageURL, "?") {
				separator = "&"
			}
			packageURL += separator + "cc-repository-retry=" + strconv.Itoa(attempt)
		}
		downloadErr = m.downloadFile(ctx, packageURL, apkPath, entry, report)
		if downloadErr != nil {
			if !isAlpineRepositoryRace(downloadErr) {
				return downloadErr
			}
			continue
		}
		developmentFilename := fmt.Sprintf("%s-%s.apk", developmentEntry.Name, developmentEntry.Version)
		developmentAPKPath = filepath.Join(destDir, developmentFilename)
		developmentURL := m.packageURL(developmentFilename)
		if attempt > 0 {
			developmentURL += "?cc-repository-retry=" + strconv.Itoa(attempt)
		}
		downloadErr = m.downloadFile(ctx, developmentURL, developmentAPKPath, developmentEntry, report)
		if downloadErr == nil {
			break
		}
		if !isAlpineRepositoryRace(downloadErr) {
			return downloadErr
		}
	}
	if downloadErr != nil {
		return fmt.Errorf("Alpine repository index and kernel package remained inconsistent after refresh: %w", downloadErr)
	}
	tarPath := filepath.Join(destDir, fmt.Sprintf("%s-%s.tar", entry.Name, entry.Version))
	if err := decompressAPKToTar(apkPath, tarPath); err != nil {
		return err
	}
	_ = os.Remove(apkPath)
	developmentTarPath := filepath.Join(destDir, fmt.Sprintf("%s-%s.tar", developmentEntry.Name, developmentEntry.Version))
	if err := decompressAPKToTar(developmentAPKPath, developmentTarPath); err != nil {
		return err
	}
	_ = os.Remove(developmentAPKPath)
	symvers, err := readTarFileWithSuffix(developmentTarPath, "/Module.symvers")
	_ = os.Remove(developmentTarPath)
	if err != nil {
		return fmt.Errorf("extract matching Module.symvers: %w", err)
	}
	if err := validateModuleSymvers(symvers); err != nil {
		return fmt.Errorf("validate matching Module.symvers: %w", err)
	}
	symversPath := filepath.Join(destDir, fmt.Sprintf("Module.symvers-%s", entry.Version))
	tmpSymversPath := symversPath + ".tmp"
	if err := os.WriteFile(tmpSymversPath, symvers, 0o644); err != nil {
		return fmt.Errorf("write matching Module.symvers: %w", err)
	}
	if err := os.Rename(tmpSymversPath, symversPath); err != nil {
		_ = os.Remove(tmpSymversPath)
		return fmt.Errorf("finalize matching Module.symvers: %w", err)
	}

	meta := metadata{
		Version:           entry.Version,
		Source:            m.sourceName(),
		PackageName:       entry.Name,
		PackageFile:       tarPath,
		ModuleSymversFile: symversPath,
		Arch:              entry.Arch,
	}

	if err := m.writeMetadata(meta); err != nil {
		return err
	}
	m.mu.Lock()
	m.kernelBytes = nil
	m.kernelConfig = nil
	m.moduleDeps = nil
	m.modulePrefix = ""
	m.packageFiles = map[string][]byte{}
	m.modulePaths = map[string]string{}
	m.packageIndex = map[string]tarIndexEntry{}
	m.tarIndexes = map[string]map[string]tarIndexEntry{}
	m.indexedTar = ""
	m.mu.Unlock()

	return nil
}

func isAlpineRepositoryRace(err error) bool {
	var digestErr *download.DigestError
	var lengthErr *download.LengthError
	return errors.As(err, &digestErr) || errors.As(err, &lengthErr)
}

func (m *Manager) readKernelConfig() (map[string]Tristate, error) {
	m.mu.Lock()
	if len(m.kernelConfig) > 0 {
		cfg := make(map[string]Tristate, len(m.kernelConfig))
		for key, value := range m.kernelConfig {
			cfg[key] = value
		}
		m.mu.Unlock()
		return cfg, nil
	}
	m.mu.Unlock()

	index, _, err := m.ensurePackageIndex()
	if err != nil {
		return nil, err
	}
	for path := range index {
		if !strings.HasPrefix(path, "boot/config-") {
			continue
		}
		data, err := m.readRawPackageFile(path)
		if err != nil {
			return nil, err
		}
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
		m.mu.Lock()
		m.kernelConfig = make(map[string]Tristate, len(config))
		for key, value := range config {
			m.kernelConfig[key] = value
		}
		cached := make(map[string]Tristate, len(m.kernelConfig))
		for key, value := range m.kernelConfig {
			cached[key] = value
		}
		m.mu.Unlock()
		return cached, nil
	}
	return nil, fmt.Errorf("kernel config not found in package")
}

func (m *Manager) readModuleDependencies() (map[string][]string, string, error) {
	m.mu.Lock()
	if len(m.moduleDeps) > 0 && m.modulePrefix != "" {
		deps := cloneModuleDeps(m.moduleDeps)
		prefix := m.modulePrefix
		m.mu.Unlock()
		return deps, prefix, nil
	}
	m.mu.Unlock()

	path, err := m.findModuleFile("modules.dep")
	if err != nil {
		return nil, "", err
	}
	data, err := m.readRawPackageFile(path)
	if err != nil {
		return nil, "", err
	}
	prefix := strings.TrimSuffix(path, "modules.dep")
	depends := map[string][]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		moduleName, depsRaw, ok := strings.Cut(line, ":")
		if !ok {
			return nil, "", fmt.Errorf("invalid modules.dep line %q", line)
		}
		moduleName = strings.TrimSpace(moduleName)
		for _, dep := range strings.Fields(strings.TrimSpace(depsRaw)) {
			depends[moduleName] = append(depends[moduleName], dep)
		}
		if _, ok := depends[moduleName]; !ok {
			depends[moduleName] = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("scan modules.dep: %w", err)
	}
	m.mu.Lock()
	m.moduleDeps = cloneModuleDeps(depends)
	m.modulePrefix = prefix
	deps := cloneModuleDeps(m.moduleDeps)
	cachedPrefix := m.modulePrefix
	m.mu.Unlock()
	return deps, cachedPrefix, nil
}

func (m *Manager) findModuleFile(suffix string) (string, error) {
	m.mu.Lock()
	if path := m.modulePaths[suffix]; path != "" {
		m.mu.Unlock()
		return path, nil
	}
	m.mu.Unlock()

	index, _, err := m.ensurePackageIndex()
	if err != nil {
		return "", err
	}
	for path := range index {
		if strings.HasSuffix(path, suffix) {
			m.mu.Lock()
			m.modulePaths[suffix] = path
			m.mu.Unlock()
			return path, nil
		}
	}
	return "", fmt.Errorf("module file with suffix %q not found", suffix)
}

func (m *Manager) readModuleFile(path string) ([]byte, error) {
	data, err := m.readRawPackageFile(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") {
		gzr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open module gzip %q: %w", path, err)
		}
		defer gzr.Close()
		data, err = download.ReadAllReader(context.Background(), gzr, 256<<20)
		if err != nil {
			return nil, fmt.Errorf("read module %q: %w", path, err)
		}
	}
	return data, nil
}

func (m *Manager) readRawPackageFile(path string) ([]byte, error) {
	start := time.Now()
	m.mu.Lock()
	if data, ok := m.packageFiles[path]; ok {
		cached := append([]byte(nil), data...)
		m.mu.Unlock()
		timingLog("kernel.readRawPackageFile cache_hit path=%q took=%s bytes=%d", path, time.Since(start), len(cached))
		return cached, nil
	}
	m.mu.Unlock()

	packagePath, err := m.PackagePath()
	if err != nil {
		return nil, err
	}
	index, tarPath, err := m.ensurePackageIndex()
	if err != nil {
		return nil, err
	}
	entry, ok := index[path]
	if !ok {
		return nil, fmt.Errorf("package file %q not found", path)
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("open kernel package: %w", err)
	}
	defer f.Close()
	data := make([]byte, entry.Size)
	n, err := f.ReadAt(data, entry.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read package file %q: %w", path, err)
	}
	if int64(n) != entry.Size {
		return nil, fmt.Errorf("read package file %q: short read %d/%d", path, n, entry.Size)
	}
	m.mu.Lock()
	m.packageFiles[path] = append([]byte(nil), data...)
	cached := append([]byte(nil), m.packageFiles[path]...)
	m.mu.Unlock()
	timingLog("kernel.readRawPackageFile cache_miss path=%q took=%s bytes=%d package=%q", path, time.Since(start), len(cached), packagePath)
	return cached, nil
}

func cloneModuleDeps(src map[string][]string) map[string][]string {
	out := make(map[string][]string, len(src))
	for key, value := range src {
		out[key] = append([]string(nil), value...)
	}
	return out
}

func (m *Manager) fetchIndexEntries(ctx context.Context, reporters ...progressReporter) (indexEntry, indexEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.indexURL(), nil)
	if err != nil {
		return indexEntry{}, indexEntry{}, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return indexEntry{}, indexEntry{}, fmt.Errorf("download kernel index: %w", err)
	}
	defer resp.Body.Close()
	if err := download.BoundResponse(resp, 64<<20); err != nil {
		return indexEntry{}, indexEntry{}, fmt.Errorf("download kernel index: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return indexEntry{}, indexEntry{}, fmt.Errorf("download kernel index: status %s", resp.Status)
	}

	var body io.Reader = resp.Body
	if len(reporters) > 0 && reporters[0] != nil {
		var buffer bytes.Buffer
		if err := copyWithProgress(&buffer, resp.Body, resp.ContentLength, "APKINDEX.tar.gz", reporters[0]); err != nil {
			return indexEntry{}, indexEntry{}, err
		}
		body = &buffer
	}
	indexData, err := readAPKIndex(body)
	if err != nil {
		return indexEntry{}, indexEntry{}, err
	}

	entry, ok := indexData[m.packageName]
	if !ok {
		return indexEntry{}, indexEntry{}, fmt.Errorf("kernel package %q not found in APKINDEX", m.packageName)
	}
	if entry.Arch != m.arch {
		return indexEntry{}, indexEntry{}, fmt.Errorf("kernel package arch %q does not match expected %q", entry.Arch, m.arch)
	}
	developmentName := m.packageName + "-dev"
	developmentEntry, ok := indexData[developmentName]
	if !ok {
		return indexEntry{}, indexEntry{}, fmt.Errorf("kernel development package %q not found in APKINDEX", developmentName)
	}
	if developmentEntry.Arch != m.arch {
		return indexEntry{}, indexEntry{}, fmt.Errorf("kernel development package arch %q does not match expected %q", developmentEntry.Arch, m.arch)
	}
	if developmentEntry.Version != entry.Version {
		return indexEntry{}, indexEntry{}, fmt.Errorf("kernel package versions do not match: %s=%s, %s=%s", entry.Name, entry.Version, developmentEntry.Name, developmentEntry.Version)
	}
	return entry, developmentEntry, nil
}

func (m *Manager) downloadFile(ctx context.Context, url, destPath string, entry indexEntry, report progressReporter) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download kernel package: %w", err)
	}
	defer resp.Body.Close()
	if err := download.BoundResponse(resp, 0); err != nil {
		return fmt.Errorf("download kernel package: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download kernel package: status %s", resp.Status)
	}

	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create kernel package file: %w", err)
	}
	if err := copyWithProgress(f, resp.Body, resp.ContentLength, filepath.Base(destPath), report); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write kernel package: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close kernel package: %w", err)
	}
	if entry.Size > 0 && resp.ContentLength != entry.Size {
		_ = os.Remove(tmpPath)
		return &download.LengthError{Expected: entry.Size, Actual: resp.ContentLength}
	}
	if expected, ok := alpineChecksumHex(entry.Checksum); ok {
		actual, err := apkChecksumHex(tmpPath)
		if err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("verify kernel package checksum: %w", err)
		}
		if !strings.EqualFold(expected, actual) {
			_ = os.Remove(tmpPath)
			return &download.DigestError{Expected: "sha1:" + expected, Actual: "sha1:" + actual}
		}
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize kernel package: %w", err)
	}
	return nil
}

func copyWithProgress(dst io.Writer, src io.Reader, total int64, artifact string, report progressReporter) error {
	if report == nil {
		_, err := io.Copy(dst, src)
		return err
	}

	started := time.Now()
	lastReported := time.Time{}
	downloaded := int64(0)
	buffer := make([]byte, 128*1024)
	emit := func(status string, force bool) {
		now := time.Now()
		if !force && !lastReported.IsZero() && now.Sub(lastReported) < 200*time.Millisecond {
			return
		}
		lastReported = now
		elapsed := now.Sub(started).Seconds()
		progress := 0.0
		if total > 0 {
			progress = float64(downloaded) / float64(total)
		}
		rate := 0.0
		if elapsed > 0 {
			rate = float64(downloaded) / elapsed
		}
		eta := 0.0
		if total > 0 && rate > 0 && downloaded < total {
			eta = float64(total-downloaded) / rate
		}
		report(client.ProgressEvent{
			Status:             status,
			Artifact:           artifact,
			Progress:           progress,
			BytesDownloaded:    downloaded,
			BytesTotal:         total,
			RateBytesPerSecond: rate,
			ETASeconds:         eta,
		})
	}

	emit("downloading", true)
	for {
		n, readErr := src.Read(buffer)
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			downloaded += int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			emit("downloading", false)
		}
		if readErr != nil {
			if readErr == io.EOF {
				emit("downloaded", true)
				return nil
			}
			return readErr
		}
	}
}

func (m *Manager) ensurePackageIndex() (map[string]tarIndexEntry, string, error) {
	packagePath, err := m.PackagePath()
	if err != nil {
		return nil, "", err
	}

	m.mu.Lock()
	if m.indexedTar == packagePath && len(m.packageIndex) > 0 {
		index := make(map[string]tarIndexEntry, len(m.packageIndex))
		for path, entry := range m.packageIndex {
			index[path] = entry
		}
		m.mu.Unlock()
		return index, packagePath, nil
	}
	m.mu.Unlock()

	index, err := buildTarIndex(packagePath)
	if err != nil {
		return nil, "", err
	}

	m.mu.Lock()
	m.packageIndex = make(map[string]tarIndexEntry, len(index))
	for path, entry := range index {
		m.packageIndex[path] = entry
	}
	m.indexedTar = packagePath
	cached := make(map[string]tarIndexEntry, len(m.packageIndex))
	for path, entry := range m.packageIndex {
		cached[path] = entry
	}
	m.mu.Unlock()
	return cached, packagePath, nil
}

func buildTarIndex(packagePath string) (map[string]tarIndexEntry, error) {
	f, err := os.Open(packagePath)
	if err != nil {
		return nil, fmt.Errorf("open kernel package: %w", err)
	}
	defer f.Close()

	cr := &countingReader{r: f}
	tr := tar.NewReader(cr)
	index := make(map[string]tarIndexEntry)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return index, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read kernel package tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		index[hdr.Name] = tarIndexEntry{
			Offset: cr.n,
			Size:   hdr.Size,
			Mode:   int64(hdr.FileInfo().Mode().Perm()),
		}
	}
}

func readTarFileWithSuffix(packagePath, suffix string) ([]byte, error) {
	index, err := buildTarIndex(packagePath)
	if err != nil {
		return nil, err
	}
	var match string
	for name := range index {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		if match != "" {
			return nil, fmt.Errorf("package contains multiple files ending in %q", suffix)
		}
		match = name
	}
	if match == "" {
		return nil, fmt.Errorf("package file ending in %q not found", suffix)
	}
	entry := index[match]
	f, err := os.Open(packagePath)
	if err != nil {
		return nil, fmt.Errorf("open package: %w", err)
	}
	defer f.Close()
	data := make([]byte, entry.Size)
	n, err := f.ReadAt(data, entry.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read package file %q: %w", match, err)
	}
	if int64(n) != entry.Size {
		return nil, fmt.Errorf("read package file %q: short read %d/%d", match, n, entry.Size)
	}
	return data, nil
}

func validateModuleSymvers(data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[1] != "module_layout" {
			continue
		}
		crc := strings.TrimPrefix(fields[0], "0x")
		if len(crc) != 8 {
			return fmt.Errorf("module_layout has invalid CRC %q", fields[0])
		}
		if _, err := strconv.ParseUint(crc, 16, 32); err != nil {
			return fmt.Errorf("module_layout has invalid CRC %q", fields[0])
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read symbol versions: %w", err)
	}
	return fmt.Errorf("module_layout CRC is missing")
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func decompressAPKToTar(srcPath, destPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open kernel package apk: %w", err)
	}
	defer src.Close()

	gzr, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("open kernel package gzip: %w", err)
	}
	defer gzr.Close()

	tmpPath := destPath + ".tmp"
	dst, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create kernel tar file: %w", err)
	}
	if _, err := io.Copy(dst, gzr); err != nil {
		dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write kernel tar file: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close kernel tar file: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize kernel tar file: %w", err)
	}
	return nil
}

func readAPKIndex(r io.Reader) (map[string]indexEntry, error) {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("open APKINDEX gzip: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("APKINDEX entry not found")
		}
		if err != nil {
			return nil, fmt.Errorf("read APKINDEX tar: %w", err)
		}
		if hdr.Name != "APKINDEX" {
			continue
		}
		return parseAPKIndex(tr)
	}
}

func parseAPKIndex(r io.Reader) (map[string]indexEntry, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	out := map[string]indexEntry{}
	var current indexEntry

	flush := func() {
		if current.Name != "" {
			out[current.Name] = current
		}
		current = indexEntry{}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			flush()
			continue
		}
		if len(line) < 3 || line[1] != ':' {
			return nil, fmt.Errorf("invalid APKINDEX line %q", line)
		}

		key := line[:1]
		value := line[2:]

		switch key {
		case "P":
			current.Name = value
		case "V":
			current.Version = value
		case "A":
			current.Arch = value
		case "S":
			current.Size, _ = strconv.ParseInt(value, 10, 64)
		case "C":
			current.Checksum = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan APKINDEX: %w", err)
	}
	flush()
	return out, nil
}

func alpineChecksumHex(value string) (string, bool) {
	if !strings.HasPrefix(value, "Q1") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "Q1"))
	if err != nil || len(raw) != sha1.Size {
		return "", false
	}
	return hex.EncodeToString(raw), true
}

func apkChecksumHex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	type memberRange struct{ start, end int64 }
	var members []memberRange
	for {
		if _, err := reader.Peek(1); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return "", err
		}
		position, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return "", err
		}
		start := position - int64(reader.Buffered())
		gzr, err := gzip.NewReader(reader)
		if err != nil {
			return "", fmt.Errorf("open gzip member %d: %w", len(members)+1, err)
		}
		gzr.Multistream(false)
		if _, err := io.Copy(io.Discard, gzr); err != nil {
			_ = gzr.Close()
			return "", fmt.Errorf("read gzip member %d: %w", len(members)+1, err)
		}
		if err := gzr.Close(); err != nil {
			return "", fmt.Errorf("close gzip member %d: %w", len(members)+1, err)
		}
		position, err = f.Seek(0, io.SeekCurrent)
		if err != nil {
			return "", err
		}
		end := position - int64(reader.Buffered())
		if end <= start {
			return "", fmt.Errorf("gzip member %d has invalid range", len(members)+1)
		}
		members = append(members, memberRange{start: start, end: end})
	}
	if len(members) == 0 {
		return "", fmt.Errorf("package has no gzip members")
	}
	checksumMember := members[0]
	if len(members) > 1 {
		// APK v2's Q1 value covers the control stream, which follows the
		// signature stream. A single-member fallback keeps unsigned packages
		// and older test repositories usable.
		checksumMember = members[1]
	}
	hasher := sha1.New()
	if _, err := io.Copy(hasher, io.NewSectionReader(f, checksumMember.start, checksumMember.end-checksumMember.start)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (m *Manager) readMetadata() (metadata, error) {
	var ret metadata
	buf, err := os.ReadFile(m.metadataPath())
	if err != nil {
		return ret, err
	}
	if err := json.Unmarshal(buf, &ret); err != nil {
		return ret, fmt.Errorf("decode kernel metadata: %w", err)
	}
	if ret.Version == "" || ret.Source == "" || ret.PackageFile == "" {
		return ret, errors.New("kernel metadata is incomplete")
	}
	return ret, nil
}

func (m *Manager) writeMetadata(meta metadata) error {
	buf, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal kernel metadata: %w", err)
	}
	path := m.metadataPath()
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, buf, 0o644); err != nil {
		return fmt.Errorf("write kernel metadata: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize kernel metadata: %w", err)
	}
	return nil
}

func (m *Manager) metadataPath() string {
	return filepath.Join(m.root, "kernel.json")
}

func (m *Manager) sourceName() string {
	return "alpine:" + m.version
}

func (m *Manager) indexURL() string {
	return strings.TrimRight(m.mirror, "/") + "/" + m.version + "/" + m.repo + "/" + m.arch + "/APKINDEX.tar.gz"
}

func (m *Manager) packageURL(filename string) string {
	return strings.TrimRight(m.mirror, "/") + "/" + m.version + "/" + m.repo + "/" + m.arch + "/" + filename
}

func defaultArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

func defaultPackageName() string {
	if defaultArch() == "riscv64" {
		return "linux-lts"
	}
	return "linux-virt"
}
