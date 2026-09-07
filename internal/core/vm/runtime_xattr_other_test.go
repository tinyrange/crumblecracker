//go:build !linux && !freebsd && !netbsd && !darwin

package vm

import "errors"

func setRuntimeTestXattr(string, string, []byte) error {
	return errors.New("extended attributes are unsupported on this test platform")
}
