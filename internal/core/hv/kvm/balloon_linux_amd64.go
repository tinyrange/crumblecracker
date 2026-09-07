//go:build linux && amd64

package kvm

func balloonTargetPages(mb uint64) uint32 {
	pages := mb * 1024 * 1024 / 4096
	if pages > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(pages)
}
