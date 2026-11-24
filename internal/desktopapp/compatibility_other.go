//go:build !darwin

package desktopapp

import "runtime"

func platformCompatibilityError() error {
	return nil
}

func platformExperimentalGPUAccelerationAvailable() bool {
	return runtime.GOOS == "linux" && runtime.GOARCH == "amd64"
}

func platformNativeGPUScanoutAvailable() bool {
	return false
}
