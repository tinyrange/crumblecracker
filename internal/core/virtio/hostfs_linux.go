//go:build linux

package virtio

import (
	"os"
	"syscall"

	"github.com/tinyrange/crumblecracker/internal/core/linuxabi"
)

func hostStatFS(root string) (uint64, uint64, uint64, uint64, uint64, uint64, uint64, uint64, int32) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, errnoFromError(err)
	}
	return st.Blocks, st.Bfree, st.Bavail, st.Files, st.Ffree, uint64(st.Bsize), uint64(st.Bsize), 255, 0
}

func enrichHostFileAttr(_ string, info os.FileInfo, attr *FuseAttr) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	attr.Size = uint64(st.Size)
	attr.Ino = st.Ino
	attr.Blocks = uint64(st.Blocks)
	attr.NLink = uint32(st.Nlink)
	if st.Blksize > 0 {
		attr.BlkSize = uint32(st.Blksize)
	}
	attr.ATimeSec, attr.ATimeNsec = uint64(st.Atim.Sec), uint32(st.Atim.Nsec)
	attr.MTimeSec, attr.MTimeNsec = uint64(st.Mtim.Sec), uint32(st.Mtim.Nsec)
	attr.CTimeSec, attr.CTimeNsec = uint64(st.Ctim.Sec), uint32(st.Ctim.Nsec)
	attr.UID = st.Uid
	attr.GID = st.Gid
}

func mapHostError(err error) (int32, bool) {
	errno, ok := err.(syscall.Errno)
	if !ok {
		return 0, false
	}
	switch errno {
	case syscall.ENOENT:
		return linuxabi.ENOENT, true
	case syscall.EPERM:
		return linuxabi.EPERM, true
	case syscall.EEXIST:
		return linuxabi.EEXIST, true
	case syscall.ETIMEDOUT:
		return linuxabi.ETIMEDOUT, true
	case syscall.EISDIR:
		return linuxabi.EISDIR, true
	case syscall.ENOTDIR:
		return linuxabi.ENOTDIR, true
	case syscall.EINVAL:
		return linuxabi.EINVAL, true
	case syscall.EBADF:
		return linuxabi.EBADF, true
	case syscall.ENXIO:
		return linuxabi.ENXIO, true
	case syscall.EIO:
		return linuxabi.EIO, true
	case syscall.ERANGE:
		return linuxabi.ERANGE, true
	case syscall.ENODATA:
		return linuxabi.ENODATA, true
	case syscall.ENOSYS:
		return linuxabi.ENOSYS, true
	default:
		return int32(errno), true
	}
}
