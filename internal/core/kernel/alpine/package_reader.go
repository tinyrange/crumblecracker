package alpine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/download"
	"github.com/tinyrange/crumblecracker/internal/core/timing"
)

const repoIndexDiskCacheMaxAge = 24 * time.Hour

func (m *Manager) ReadPackageFile(ctx context.Context, repo, packageName, innerPath string) ([]byte, error) {
	cacheKey := repo + "\x00" + packageName + "\x00" + innerPath
	m.mu.Lock()
	if data, ok := m.packageFiles[cacheKey]; ok {
		cached := append([]byte(nil), data...)
		m.mu.Unlock()
		return cached, nil
	}
	m.mu.Unlock()

	entry, err := m.lookupPackageEntry(ctx, repo, packageName)
	if err != nil {
		return nil, err
	}
	filename := fmt.Sprintf("%s-%s.apk", entry.Name, entry.Version)
	destDir := filepath.Join(m.root, "packages", repo)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create package dir: %w", err)
	}
	apkPath := filepath.Join(destDir, filename)
	if _, err := os.Stat(apkPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat package file: %w", err)
		}
		if err := m.downloadFile(ctx, m.repoPackageURL(repo, filename), apkPath, entry, nil); err != nil {
			return nil, fmt.Errorf("download package %q: %w", packageName, err)
		}
	}
	tarPath := filepath.Join(destDir, fmt.Sprintf("%s-%s.tar", entry.Name, entry.Version))
	if _, err := os.Stat(tarPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat package tar file: %w", err)
		}
		if err := decompressAPKToTar(apkPath, tarPath); err != nil {
			return nil, err
		}
	}
	index, err := m.cachedTarIndex(ctx, tarPath)
	if err != nil {
		return nil, err
	}
	entryInfo, ok := index[innerPath]
	if !ok {
		return nil, fmt.Errorf("package file %q not found in %s", innerPath, packageName)
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("open package tar file: %w", err)
	}
	defer f.Close()
	data := make([]byte, entryInfo.Size)
	n, err := f.ReadAt(data, entryInfo.Offset)
	if err != nil && n != len(data) {
		return nil, fmt.Errorf("read package file %q: %w", innerPath, err)
	}
	if n != len(data) {
		return nil, fmt.Errorf("read package file %q: short read %d/%d", innerPath, n, len(data))
	}
	m.mu.Lock()
	m.packageFiles[cacheKey] = append([]byte(nil), data...)
	cached := append([]byte(nil), m.packageFiles[cacheKey]...)
	m.mu.Unlock()
	return cached, nil
}

func (m *Manager) ExtractPackageFile(ctx context.Context, repo, packageName, innerPath string) (string, error) {
	return m.ExtractPackageFileWithProgress(ctx, repo, packageName, innerPath, nil)
}

func (m *Manager) ExtractPackageFileWithProgress(
	ctx context.Context,
	repo, packageName, innerPath string,
	report progressReporter,
) (string, error) {
	start := time.Now()
	entry, err := m.lookupPackageEntry(ctx, repo, packageName)
	timing.Since(ctx, "kernel.extract_package.lookup_package_entry", start)
	if err != nil {
		return "", err
	}
	filename := fmt.Sprintf("%s-%s.apk", entry.Name, entry.Version)
	destDir := filepath.Join(m.root, "packages", repo)
	start = time.Now()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create package dir: %w", err)
	}
	timing.Since(ctx, "kernel.extract_package.mkdir_package_dir", start)
	apkPath := filepath.Join(destDir, filename)
	start = time.Now()
	if _, err := os.Stat(apkPath); err != nil {
		timing.Since(ctx, "kernel.extract_package.stat_apk_miss", start)
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat package file: %w", err)
		}
		start = time.Now()
		if err := m.downloadFile(ctx, m.repoPackageURL(repo, filename), apkPath, entry, report); err != nil {
			return "", fmt.Errorf("download package %q: %w", packageName, err)
		}
		timing.Since(ctx, "kernel.extract_package.download_apk", start)
	} else {
		timing.Since(ctx, "kernel.extract_package.stat_apk_hit", start)
	}
	tarPath := filepath.Join(destDir, fmt.Sprintf("%s-%s.tar", entry.Name, entry.Version))
	start = time.Now()
	if _, err := os.Stat(tarPath); err != nil {
		timing.Since(ctx, "kernel.extract_package.stat_tar_miss", start)
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat package tar file: %w", err)
		}
		start = time.Now()
		if err := decompressAPKToTar(apkPath, tarPath); err != nil {
			return "", err
		}
		timing.Since(ctx, "kernel.extract_package.decompress_apk_to_tar", start)
	} else {
		timing.Since(ctx, "kernel.extract_package.stat_tar_hit", start)
	}
	start = time.Now()
	index, err := m.cachedTarIndex(ctx, tarPath)
	timing.Since(ctx, "kernel.extract_package.cached_tar_index", start)
	if err != nil {
		return "", err
	}
	start = time.Now()
	entryInfo, ok := index[innerPath]
	timing.Since(ctx, "kernel.extract_package.lookup_inner_file", start)
	if !ok {
		return "", fmt.Errorf("package file %q not found in %s", innerPath, packageName)
	}
	outPath := filepath.Join(m.root, "extracted", repo, entry.Name+"-"+entry.Version, filepath.FromSlash(innerPath))
	start = time.Now()
	if info, err := os.Stat(outPath); err == nil && info.Mode().IsRegular() && info.Size() == entryInfo.Size {
		timing.Since(ctx, "kernel.extract_package.stat_extracted_hit", start)
		return outPath, nil
	} else if err != nil && !os.IsNotExist(err) {
		timing.Since(ctx, "kernel.extract_package.stat_extracted_error", start)
		return "", fmt.Errorf("stat extracted package file: %w", err)
	} else {
		timing.Since(ctx, "kernel.extract_package.stat_extracted_miss", start)
	}
	start = time.Now()
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return "", fmt.Errorf("create extracted package dir: %w", err)
	}
	timing.Since(ctx, "kernel.extract_package.mkdir_extracted_dir", start)
	start = time.Now()
	src, err := os.Open(tarPath)
	if err != nil {
		return "", fmt.Errorf("open package tar file: %w", err)
	}
	defer src.Close()
	timing.Since(ctx, "kernel.extract_package.open_tar", start)
	start = time.Now()
	if _, err := src.Seek(entryInfo.Offset, 0); err != nil {
		return "", fmt.Errorf("seek package tar file %q: %w", innerPath, err)
	}
	timing.Since(ctx, "kernel.extract_package.seek_tar", start)
	tmpPath := outPath + ".tmp"
	start = time.Now()
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(entryInfo.Mode))
	if err != nil {
		return "", fmt.Errorf("create extracted package file: %w", err)
	}
	timing.Since(ctx, "kernel.extract_package.create_tmp", start)
	start = time.Now()
	if _, err := io.CopyN(dst, src, entryInfo.Size); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("extract package file %q: %w", innerPath, err)
	}
	timing.Since(ctx, "kernel.extract_package.copy_file", start)
	start = time.Now()
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close extracted package file %q: %w", innerPath, err)
	}
	timing.Since(ctx, "kernel.extract_package.close_tmp", start)
	start = time.Now()
	if err := os.Rename(tmpPath, outPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("finalize extracted package file %q: %w", innerPath, err)
	}
	timing.Since(ctx, "kernel.extract_package.rename_tmp", start)
	return outPath, nil
}

func (m *Manager) lookupPackageEntry(ctx context.Context, repo, packageName string) (indexEntry, error) {
	start := time.Now()
	m.mu.Lock()
	if repoIndex, ok := m.repoIndexes[repo]; ok {
		if entry, ok := repoIndex[packageName]; ok {
			m.mu.Unlock()
			timing.Since(ctx, "kernel.lookup_package_entry.cache_hit", start)
			return entry, nil
		}
	}
	m.mu.Unlock()
	timing.Since(ctx, "kernel.lookup_package_entry.cache_miss", start)

	start = time.Now()
	if entry, ok, err := m.loadPackageEntryCache(repo, packageName); err != nil {
		timing.Since(ctx, "kernel.lookup_package_entry.entry_disk_cache_error", start)
	} else if ok {
		timing.Since(ctx, "kernel.lookup_package_entry.entry_disk_cache_hit", start)
		return entry, nil
	} else {
		timing.Since(ctx, "kernel.lookup_package_entry.entry_disk_cache_miss", start)
	}

	start = time.Now()
	if indexData, ok, err := m.loadRepoIndexCache(repo); err != nil {
		timing.Since(ctx, "kernel.lookup_package_entry.disk_cache_error", start)
	} else if ok {
		timing.Since(ctx, "kernel.lookup_package_entry.disk_cache_hit", start)
		entry, err := m.packageEntryFromIndex(repo, packageName, indexData)
		if err != nil {
			return indexEntry{}, err
		}
		start = time.Now()
		if err := m.savePackageEntryCache(repo, packageName, entry); err != nil {
			timing.Since(ctx, "kernel.lookup_package_entry.save_entry_disk_cache_error", start)
		} else {
			timing.Since(ctx, "kernel.lookup_package_entry.save_entry_disk_cache", start)
		}
		m.mu.Lock()
		m.repoIndexes[repo] = indexData
		m.mu.Unlock()
		return entry, nil
	} else {
		timing.Since(ctx, "kernel.lookup_package_entry.disk_cache_miss", start)
	}

	start = time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.repoIndexURL(repo), nil)
	if err != nil {
		return indexEntry{}, err
	}
	timing.Since(ctx, "kernel.lookup_package_entry.new_request", start)
	start = time.Now()
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return indexEntry{}, fmt.Errorf("request APKINDEX: %w", err)
	}
	defer resp.Body.Close()
	timing.Since(ctx, "kernel.lookup_package_entry.http_do", start)
	if resp.StatusCode != http.StatusOK {
		return indexEntry{}, fmt.Errorf("request APKINDEX: status %s", resp.Status)
	}
	if err := download.BoundResponse(resp, 64<<20); err != nil {
		return indexEntry{}, fmt.Errorf("request APKINDEX: %w", err)
	}
	start = time.Now()
	indexData, err := readAPKIndex(resp.Body)
	if err != nil {
		return indexEntry{}, err
	}
	timing.Since(ctx, "kernel.lookup_package_entry.read_apk_index", start)
	start = time.Now()
	entry, err := m.packageEntryFromIndex(repo, packageName, indexData)
	timing.Since(ctx, "kernel.lookup_package_entry.parse_entry", start)
	if err != nil {
		return indexEntry{}, err
	}
	start = time.Now()
	if err := m.savePackageEntryCache(repo, packageName, entry); err != nil {
		timing.Since(ctx, "kernel.lookup_package_entry.save_entry_disk_cache_error", start)
	} else {
		timing.Since(ctx, "kernel.lookup_package_entry.save_entry_disk_cache", start)
	}
	start = time.Now()
	if err := m.saveRepoIndexCache(repo, indexData); err != nil {
		timing.Since(ctx, "kernel.lookup_package_entry.save_disk_cache_error", start)
	} else {
		timing.Since(ctx, "kernel.lookup_package_entry.save_disk_cache", start)
	}
	start = time.Now()
	m.mu.Lock()
	m.repoIndexes[repo] = indexData
	m.mu.Unlock()
	timing.Since(ctx, "kernel.lookup_package_entry.store_index", start)
	return entry, nil
}

func (m *Manager) loadPackageEntryCache(repo, packageName string) (indexEntry, bool, error) {
	path := m.packageEntryCachePath(repo, packageName)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return indexEntry{}, false, nil
		}
		return indexEntry{}, false, fmt.Errorf("stat package entry cache: %w", err)
	}
	if time.Since(info.ModTime()) > repoIndexDiskCacheMaxAge {
		return indexEntry{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return indexEntry{}, false, fmt.Errorf("open package entry cache: %w", err)
	}
	defer file.Close()
	var entry indexEntry
	if err := json.NewDecoder(file).Decode(&entry); err != nil {
		return indexEntry{}, false, fmt.Errorf("decode package entry cache: %w", err)
	}
	if entry.Name == "" || entry.Version == "" || entry.Arch == "" {
		return indexEntry{}, false, fmt.Errorf("package entry cache is incomplete")
	}
	if entry.Arch != m.arch {
		return indexEntry{}, false, nil
	}
	return entry, true, nil
}

func (m *Manager) savePackageEntryCache(repo, packageName string, entry indexEntry) error {
	path := m.packageEntryCachePath(repo, packageName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create package entry cache dir: %w", err)
	}
	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create package entry cache: %w", err)
	}
	if err := json.NewEncoder(file).Encode(entry); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write package entry cache: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close package entry cache: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize package entry cache: %w", err)
	}
	return nil
}

func (m *Manager) packageEntryFromIndex(repo, packageName string, indexData map[string]indexEntry) (indexEntry, error) {
	entry, ok := indexData[packageName]
	if !ok {
		return indexEntry{}, fmt.Errorf("package %q not found in %s APKINDEX", packageName, repo)
	}
	if entry.Arch != m.arch {
		return indexEntry{}, fmt.Errorf("package arch %q does not match expected %q", entry.Arch, m.arch)
	}
	return entry, nil
}

func (m *Manager) loadRepoIndexCache(repo string) (map[string]indexEntry, bool, error) {
	path := m.repoIndexCachePath(repo)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat repo index cache: %w", err)
	}
	if time.Since(info.ModTime()) > repoIndexDiskCacheMaxAge {
		return nil, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open repo index cache: %w", err)
	}
	defer file.Close()
	var indexData map[string]indexEntry
	if err := json.NewDecoder(file).Decode(&indexData); err != nil {
		return nil, false, fmt.Errorf("decode repo index cache: %w", err)
	}
	if indexData == nil {
		return nil, false, fmt.Errorf("repo index cache is empty")
	}
	return indexData, true, nil
}

func (m *Manager) saveRepoIndexCache(repo string, indexData map[string]indexEntry) error {
	path := m.repoIndexCachePath(repo)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create repo index cache dir: %w", err)
	}
	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create repo index cache: %w", err)
	}
	enc := json.NewEncoder(file)
	if err := enc.Encode(indexData); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write repo index cache: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close repo index cache: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize repo index cache: %w", err)
	}
	return nil
}

func (m *Manager) repoIndexCachePath(repo string) string {
	name := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(repo)
	return filepath.Join(m.root, "repo-indexes", m.version, m.arch, name+".json")
}

func (m *Manager) packageEntryCachePath(repo, packageName string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	repo = replacer.Replace(repo)
	packageName = replacer.Replace(packageName)
	return filepath.Join(m.root, "repo-indexes", m.version, m.arch, repo, packageName+".json")
}

func (m *Manager) cachedTarIndex(ctx context.Context, tarPath string) (map[string]tarIndexEntry, error) {
	start := time.Now()
	m.mu.Lock()
	if index, ok := m.tarIndexes[tarPath]; ok {
		m.mu.Unlock()
		timing.Since(ctx, "kernel.cached_tar_index.cache_hit", start)
		return index, nil
	}
	m.mu.Unlock()
	timing.Since(ctx, "kernel.cached_tar_index.cache_miss", start)

	start = time.Now()
	index, err := buildTarIndex(tarPath)
	if err != nil {
		return nil, err
	}
	timing.Since(ctx, "kernel.cached_tar_index.build_tar_index", start)

	start = time.Now()
	m.mu.Lock()
	m.tarIndexes[tarPath] = index
	m.mu.Unlock()
	timing.Since(ctx, "kernel.cached_tar_index.store_index", start)
	return index, nil
}

func (m *Manager) repoIndexURL(repo string) string {
	return stringsTrimRightSlash(m.mirror) + "/" + m.version + "/" + repo + "/" + m.arch + "/APKINDEX.tar.gz"
}

func (m *Manager) repoPackageURL(repo, filename string) string {
	return stringsTrimRightSlash(m.mirror) + "/" + m.version + "/" + repo + "/" + m.arch + "/" + filename
}

func stringsTrimRightSlash(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}
