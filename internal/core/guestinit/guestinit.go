package guestinit

import (
	"context"
	"fmt"
	"runtime"
)

func Build(ctx context.Context, cacheDir string) ([]byte, error) {
	return BuildForArch(ctx, cacheDir, runtime.GOARCH)
}

func BuildForArch(ctx context.Context, cacheDir, goarch string) ([]byte, error) {
	_ = cacheDir
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	payload := embeddedPayload(goarch)
	if err := validateGuestInitPayload(goarch, payload); err != nil {
		return nil, err
	}
	return append([]byte(nil), payload...), nil
}

func validateGuestInitPayload(goarch string, payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("guest init payload for %q is empty", goarch)
	}
	if len(payload) < 4 || string(payload[:4]) != "\x7fELF" {
		return fmt.Errorf("guest init payload for %q is not a static Linux ELF", goarch)
	}
	return nil
}
