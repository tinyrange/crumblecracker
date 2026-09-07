//go:build !freebsd && !netbsd && !openbsd

package guestagent

func validateGuestUser(string) error {
	return nil
}
