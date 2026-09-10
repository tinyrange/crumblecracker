//go:build !windows

package virtio

import "os"

func setHostMode(path string, mode uint32) error { return os.Chmod(path, linuxModeToGo(mode)) }
func setHostOwner(path string, valid, uid, gid uint32) error {
	u, g := -1, -1
	if valid&fattrUID != 0 {
		u = int(uid)
	}
	if valid&fattrGID != 0 {
		g = int(gid)
	}
	return os.Chown(path, u, g)
}
func applyHostMetadata(_ string, _ *FuseAttr) error   { return nil }
func initHostMetadata(_ string, _, _, _ uint32) error { return nil }
