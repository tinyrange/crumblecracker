package vm

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/timing"
)

type packageFileExtractor func(ctx context.Context, repo, packageName, innerPath string) (string, error)

func NeedsAMD64Emulation(image *oci.Image) bool {
	if runtime.GOARCH != "arm64" || image == nil {
		return false
	}
	return strings.TrimSpace(image.Architecture) == "amd64"
}

func WantsAMD64Emulation(image *oci.Image, requested bool) bool {
	return runtime.GOARCH == "arm64" && (requested || NeedsAMD64Emulation(image))
}

func PrepareAMD64Emulator(ctx context.Context, image *oci.Image, extractPackageFile packageFileExtractor) (string, error) {
	return PrepareAMD64EmulatorForGuest(ctx, image, false, extractPackageFile)
}

func PrepareAMD64EmulatorForGuest(ctx context.Context, image *oci.Image, requested bool, extractPackageFile packageFileExtractor) (string, error) {
	start := time.Now()
	if !WantsAMD64Emulation(image, requested) {
		timing.Since(ctx, "backend.prepare_amd64_emulator.needs_check", start)
		return "", nil
	}
	timing.Since(ctx, "backend.prepare_amd64_emulator.needs_check", start)
	if extractPackageFile == nil {
		return "", fmt.Errorf("package file extractor is nil")
	}
	start = time.Now()
	qemu, err := extractPackageFile(ctx, "community", "qemu-x86_64", "usr/bin/qemu-x86_64")
	timing.Since(ctx, "backend.prepare_amd64_emulator.extract_package_file", start)
	if err != nil {
		return "", fmt.Errorf("extract qemu-x86_64 package file: %w", err)
	}
	return qemu, nil
}
