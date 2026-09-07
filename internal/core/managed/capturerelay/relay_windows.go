package capturerelay

import "fmt"

// Run is only used by Unix guest-init payloads. Keep a Windows definition so
// host-side package discovery and vet can still compile the repository.
func Run([]string) error {
	return fmt.Errorf("capture relay is unavailable on Windows")
}
