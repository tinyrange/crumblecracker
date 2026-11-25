//go:build !darwin && ((!linux && !windows) || !amd64)

package virgl

import (
	"errors"
	"io"
)

func ReplayCapture(string, string, int) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin, Linux/amd64 and Windows/amd64")
}

func ReplayCaptureResource(string, string, int, uint32) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin, Linux/amd64 and Windows/amd64")
}

func ReplayCaptureResourceLevel(string, string, int, uint32, uint32) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin, Linux/amd64 and Windows/amd64")
}

func ReplayCaptureResourceDraw(string, string, int, uint32, int) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin, Linux/amd64 and Windows/amd64")
}

func ReplayCaptureTraceResource(string, string, int, uint32, io.Writer) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin, Linux/amd64 and Windows/amd64")
}

func ReplayCaptureTraceDraws(string, string, int, io.Writer) (int, error) {
	return 0, errors.New("VirGL replay is currently available on Darwin, Linux/amd64 and Windows/amd64")
}

func FindCaptureProjectiveDraws(string, io.Writer) error {
	return errors.New("VirGL replay is currently available on Darwin, Linux/amd64 and Windows/amd64")
}

func FindCaptureShaderText(string, string, io.Writer) error {
	return errors.New("VirGL replay is currently available on Darwin, Linux/amd64 and Windows/amd64")
}

func TraceCaptureStreamout(string, io.Writer) error {
	return errors.New("VirGL replay is currently available on Darwin, Linux/amd64 and Windows/amd64")
}
