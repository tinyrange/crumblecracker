package desktopapp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNeurodeskUpgradeKeepsExistingHome(t *testing.T) {
	previous := appConfig
	t.Cleanup(func() { appConfig = previous })
	appConfig = Config{DefaultVMName: "neurodesk", LegacyDefaultHome: "ndappx", PersistentHomeOwner: &GuestOwner{UID: 1000, GID: 100}}
	for _, tc := range []struct {
		name      string
		stores    []string
		vm, home  string
		ephemeral bool
		want      string
	}{
		{name: "upgrade", stores: []string{"ndappx"}, vm: "neurodesk", want: "ndappx"},
		{name: "new installation", vm: "neurodesk", want: "neurodesk"},
		{name: "both homes", stores: []string{"ndappx", "neurodesk"}, vm: "neurodesk", want: "neurodesk"},
		{name: "explicit home", stores: []string{"ndappx"}, vm: "neurodesk", home: "research", want: "research"},
		{name: "custom VM", stores: []string{"ndappx"}, vm: "research", want: "research"},
		{name: "ephemeral", stores: []string{"ndappx"}, vm: "neurodesk", ephemeral: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			for _, name := range tc.stores {
				dir := filepath.Join(cache, "images", "_homes", name)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "saved-data"), []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			mounts, name, err := configuredPersistentHomeMount(cache, tc.vm, tc.home, tc.ephemeral)
			if err != nil {
				t.Fatal(err)
			}
			if name != tc.want {
				t.Fatalf("home = %q, want %q", name, tc.want)
			}
			if tc.ephemeral {
				if len(mounts) != 0 {
					t.Fatalf("ephemeral home has mounts: %+v", mounts)
				}
			} else if len(mounts) != 1 || mounts[0].Name != tc.want || !mounts[0].MapOwner || mounts[0].OwnerUID != 1000 || mounts[0].OwnerGID != 100 {
				t.Fatalf("home mounts = %+v", mounts)
			}
			for _, name := range tc.stores {
				data, err := os.ReadFile(filepath.Join(cache, "images", "_homes", name, "saved-data"))
				if err != nil || string(data) != name {
					t.Fatalf("existing home %q changed: %q, %v", name, data, err)
				}
			}
		})
	}
}

func TestNeurodeskUpgradeDoesNotHideInvalidHome(t *testing.T) {
	previous := appConfig
	t.Cleanup(func() { appConfig = previous })
	appConfig = Config{DefaultVMName: "neurodesk", LegacyDefaultHome: "ndappx"}
	for _, name := range []string{"neurodesk", "ndappx"} {
		t.Run(name, func(t *testing.T) {
			cache := t.TempDir()
			root := filepath.Join(cache, "images", "_homes")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, name), []byte("unexpected file"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := configuredPersistentHomeMount(cache, "neurodesk", "", false); err == nil {
				t.Fatal("invalid home silently ignored")
			}
		})
	}
}
