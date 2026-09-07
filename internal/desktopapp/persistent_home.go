package desktopapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/protocol"
)

// Keep an existing installation's home identity when the product's default VM
// name changes. Reuse the store in place so older app versions can still use it.
// An explicit home or custom VM name always wins, as does an existing new home.
func configuredPersistentHomeMount(cacheDir, vmName, homeName string, ephemeral bool) ([]client.PersistentMount, string, error) {
	if !ephemeral && strings.TrimSpace(homeName) == "" && vmName == appConfig.DefaultVMName && appConfig.LegacyDefaultHome != "" {
		root := filepath.Join(cacheDir, "images", "_homes")
		exists := func(name string) (bool, error) {
			info, err := os.Stat(filepath.Join(root, name))
			if os.IsNotExist(err) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("inspect persistent home %q: %w", name, err)
			}
			if !info.IsDir() {
				return false, fmt.Errorf("persistent home %q is not a directory", name)
			}
			return true, nil
		}
		current, err := exists(vmName)
		if err != nil {
			return nil, "", err
		}
		if !current {
			legacy, err := exists(appConfig.LegacyDefaultHome)
			if err != nil {
				return nil, "", err
			}
			if legacy {
				homeName = appConfig.LegacyDefaultHome
			}
		}
	}
	mounts, name, err := persistentHomeMount(vmName, homeName, ephemeral)
	if err != nil {
		return nil, "", err
	}
	mapPersistentHomeOwner(mounts, appConfig.PersistentHomeOwner)
	return mounts, name, nil
}
