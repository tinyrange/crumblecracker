//go:build !darwin

package virgl

import (
	"errors"
	"io"
)

func ReplayCapture(string, string, int) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin only")
}

func ReplayCaptureResource(string, string, int, uint32) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin only")
}

func ReplayCaptureResourceLevel(string, string, int, uint32, uint32) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin only")
}

func ReplayCaptureResourceDraw(string, string, int, uint32, int) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin only")
}

func ReplayCaptureTraceResource(string, string, int, uint32, io.Writer) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin only")
}

func ReplayCaptureTraceDraws(string, string, int, io.Writer) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin only")
}

func FindCaptureProjectiveDraws(string, io.Writer) error {
	return errors.New("VirGL replay is currently available on Darwin only")
}

func FindCaptureShaderText(string, string, io.Writer) error {
	return errors.New("VirGL replay is currently available on Darwin only")
}

func TraceCaptureStreamout(string, io.Writer) error {
	return errors.New("VirGL replay is currently available on Darwin only")
}
