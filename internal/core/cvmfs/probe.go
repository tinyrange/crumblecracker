package cvmfs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	manifestProbeLimit = int64(16 << 20)
	catalogProbeLimit  = int64(64 << 20)
	probeFinalists     = 5
)

type MirrorProbeResult struct {
	Mirror                 string
	ManifestLatency        time.Duration
	RootCatalogDuration    time.Duration
	RootCatalogBytes       int64
	RootCatalogBytesPerSec float64
	Error                  string
	rootHash               string
}

// ProbeMirrors measures repository-manifest latency in parallel, then measures
// root-catalog throughput sequentially for the quickest reachable finalists.
func ProbeMirrors(ctx context.Context, httpClient *http.Client, repo string, mirrors []string) (string, []MirrorProbeResult, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "/") {
		return "", nil, fmt.Errorf("invalid CVMFS repository %q", repo)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	candidates := uniqueProbeMirrors(mirrors)
	if len(candidates) == 0 {
		return "", nil, fmt.Errorf("no CVMFS mirrors configured")
	}
	results := make([]MirrorProbeResult, len(candidates))
	var group sync.WaitGroup
	for index, mirror := range candidates {
		group.Add(1)
		go func(index int, mirror string) {
			defer group.Done()
			results[index] = probeManifest(ctx, httpClient, repo, mirror)
		}(index, mirror)
	}
	group.Wait()

	var reachable []int
	for index := range results {
		if results[index].rootHash != "" {
			reachable = append(reachable, index)
		}
	}
	sort.Slice(reachable, func(i, j int) bool {
		return results[reachable[i]].ManifestLatency < results[reachable[j]].ManifestLatency
	})
	if len(reachable) > probeFinalists {
		reachable = reachable[:probeFinalists]
	}
	selected := ""
	bestRate := float64(0)
	for _, index := range reachable {
		probeRootCatalog(ctx, httpClient, repo, &results[index])
		if results[index].RootCatalogBytesPerSec > bestRate {
			bestRate = results[index].RootCatalogBytesPerSec
			selected = results[index].Mirror
		}
	}
	for index := range results {
		results[index].rootHash = ""
	}
	if selected == "" {
		return "", results, fmt.Errorf("no CVMFS mirror served the root catalog for %s", repo)
	}
	return selected, results, nil
}

func uniqueProbeMirrors(mirrors []string) []string {
	seen := make(map[string]bool, len(mirrors))
	out := make([]string, 0, len(mirrors))
	for _, raw := range mirrors {
		mirror := normalizeMirror(raw)
		if mirror == "" || seen[mirror] {
			continue
		}
		seen[mirror] = true
		out = append(out, mirror)
	}
	return out
}

func probeManifest(ctx context.Context, httpClient *http.Client, repo, mirror string) MirrorProbeResult {
	result := MirrorProbeResult{Mirror: mirror}
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	started := time.Now()
	body, err := probeDownload(probeCtx, httpClient, fmt.Sprintf("%s/%s/.cvmfspublished?cvmfs_probe=%d", mirror, repo, started.UnixNano()), manifestProbeLimit)
	result.ManifestLatency = time.Since(started)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	manifest, err := parseManifest(body)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.rootHash = manifest.RootCatalogHash
	return result
}

func probeRootCatalog(ctx context.Context, httpClient *http.Client, repo string, result *MirrorProbeResult) {
	if result == nil || len(result.rootHash) < 3 {
		return
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		started := time.Now()
		url := fmt.Sprintf("%s/%s/data/%s/%sC?cvmfs_probe=%d-%d", result.Mirror, repo, result.rootHash[:2], result.rootHash[2:], started.UnixNano(), attempt)
		body, err := probeDownload(probeCtx, httpClient, url, catalogProbeLimit)
		duration := time.Since(started)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		bytes := int64(len(body))
		rate := float64(0)
		if seconds := duration.Seconds(); seconds > 0 && bytes > 0 {
			rate = float64(bytes) / seconds
		}
		if rate > result.RootCatalogBytesPerSec {
			result.RootCatalogDuration = duration
			result.RootCatalogBytes = bytes
			result.RootCatalogBytesPerSec = rate
		}
	}
	if result.RootCatalogBytesPerSec > 0 {
		result.Error = ""
	} else if lastErr != nil {
		result.Error = lastErr.Error()
	}
}

func probeDownload(ctx context.Context, httpClient *http.Client, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
		return nil, fmt.Errorf("HTTP %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}
