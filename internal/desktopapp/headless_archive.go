package desktopapp

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	client "github.com/tinyrange/crumblecracker/internal/protocol"
)

// Snapshot into a content-addressed path: changing an operator's archive cannot
// reuse a path-keyed OCI cache or replace layers backing an already running VM.
func (d *headlessRuntimeDriver) importArchive(ctx context.Context, req headlessPullRequest, report func(headlessTransfer)) (headlessImage, string, error) {
	fail := func(err error) (headlessImage, string, error) { return headlessImage{}, "", err }
	source, err := os.Open(strings.TrimPrefix(req.Reference, "docker-archive:"))
	if err != nil {
		return fail(err)
	}
	defer source.Close()
	stat, err := source.Stat()
	if err != nil {
		return fail(err)
	}
	if !stat.Mode().IsRegular() {
		return fail(fmt.Errorf("archive must be a regular file"))
	}
	dir := filepath.Join(d.cache, "headless-archives")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fail(err)
	}
	tmp, err := os.CreateTemp(dir, ".import-*.tar")
	if err != nil {
		return fail(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	total := stat.Size()
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if _, err := tmp.Write(buffer[:n]); err != nil {
				return fail(err)
			}
			hash.Write(buffer[:n])
			count += int64(n)
			report(headlessTransfer{ID: "archive:snapshot", Kind: "image_layer", Name: "Local archive", State: "preparing", Completed: count, Total: &total, Branch: "image"})
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fail(readErr)
		}
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	digest := fmt.Sprintf("%x", hash.Sum(nil))
	snapshot := filepath.Join(dir, digest+".tar")
	if err := os.Rename(tmp.Name(), snapshot); err != nil {
		return fail(err)
	}
	if err := validateArchivePlatform(snapshot); err != nil {
		return fail(err)
	}
	name := "archive-" + digest
	state, stateErr := d.pullAPI.ImageState(name)
	hit := stateErr == nil && state.Status == "downloaded"
	err = d.pullAPI.PullImageStreamContext(ctx, name, client.PullImageRequest{Source: "docker-archive:" + snapshot, Architecture: runtime.GOARCH}, func(client.ProgressEvent) error { return ctx.Err() })
	if err != nil {
		return fail(err)
	}
	report(headlessTransfer{ID: "archive:snapshot", Kind: "image_layer", Name: "Local archive", State: "ready", Completed: count, Total: &count, Branch: "image", PlanningComplete: true})
	return headlessImage{ID: headlessID("img_"), Reference: req.Reference, Digest: "sha256:" + digest, Platform: req.Platform, CacheHit: hit}, name, nil
}

func validateArchivePlatform(path string) error {
	readMember := func(name string) ([]byte, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		reader := tar.NewReader(file)
		for {
			header, err := reader.Next()
			if err != nil {
				return nil, err
			}
			if strings.TrimPrefix(header.Name, "./") == name {
				if header.Size > 16<<20 {
					return nil, fmt.Errorf("archive metadata too large")
				}
				return io.ReadAll(io.LimitReader(reader, 16<<20))
			}
		}
	}
	data, err := readMember("manifest.json")
	if err != nil {
		return err
	}
	var entries []struct{ Config string }
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	if len(entries) != 1 {
		return fmt.Errorf("local archive must contain exactly one image")
	}
	data, err = readMember(entries[0].Config)
	if err != nil {
		return err
	}
	var config struct {
		Architecture string
		OS           string
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	if config.Architecture != runtime.GOARCH || config.OS != "linux" {
		return fmt.Errorf("archive must contain linux/%s", runtime.GOARCH)
	}
	return nil
}
