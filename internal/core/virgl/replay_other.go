//go:build !darwin && (!linux || !amd64)

package virgl

import (
	"errors"
	"io"
)

func ReplayCapture(string, string, int) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin and Linux/amd64 only")
}

func ReplayCaptureResource(string, string, int, uint32) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin and Linux/amd64 only")
}

func ReplayCaptureResourceLevel(string, string, int, uint32, uint32) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin and Linux/amd64 only")
}

func ReplayCaptureResourceDraw(string, string, int, uint32, int) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin and Linux/amd64 only")
}

func ReplayCaptureTraceResource(string, string, int, uint32, io.Writer) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin and Linux/amd64 only")
}

func ReplayCaptureTraceDraws(string, string, int, io.Writer) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin and Linux/amd64 only")
}

func FindCaptureProjectiveDraws(string, io.Writer) error {
	return errors.New("VirGL replay is currently available on Darwin and Linux/amd64 only")
}

func FindCaptureShaderText(string, string, io.Writer) error {
	return errors.New("VirGL replay is currently available on Darwin and Linux/amd64 only")
}

func TraceCaptureStreamout(string, io.Writer) error {
	return errors.New("VirGL replay is currently available on Darwin and Linux/amd64 only")
}
