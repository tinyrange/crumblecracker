package fsmeta

import (
	"archive/tar"
	"io/fs"
	"path"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/linuxabi"
)

const (
	linuxSIFMT    = linuxabi.SIFMT
	linuxSIFSOCK  = linuxabi.SIFSOCK
	linuxSIFLNK   = linuxabi.SIFLNK
	linuxSIFREG   = linuxabi.SIFREG
	linuxSIFBLK   = linuxabi.SIFBLK
	linuxSIFDIR   = linuxabi.SIFDIR
	linuxSIFCHR   = linuxabi.SIFCHR
	linuxSIFIFO   = linuxabi.SIFIFO
	linuxPermMask = linuxabi.PermMask
)

type Entry struct {
	UID        uint32 `json:"uid"`
	GID        uint32 `json:"gid"`
	Mode       uint32 `json:"mode,omitempty"`
	RDev       uint32 `json:"rdev,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
}

func Normalize(name string) string {
	clean := path.Clean("/" + name)
	if clean == "." {
		return "/"
	}
	return clean
}

func NormalizeSymlinkTarget(target string) string {
	return strings.ReplaceAll(target, "\\", "/")
}

func LinuxModeFromTarHeader(hdr *tar.Header) uint32 {
	mode := uint32(hdr.Mode) & linuxPermMask
	switch hdr.Typeflag {
	case tar.TypeDir:
		mode |= linuxSIFDIR
	case tar.TypeSymlink:
		mode |= linuxSIFLNK
	case tar.TypeChar:
		mode |= linuxSIFCHR
	case tar.TypeBlock:
		mode |= linuxSIFBLK
	case tar.TypeFifo:
		mode |= linuxSIFIFO
	default:
		mode |= linuxSIFREG
	}
	return mode
}

func NormalizeLinuxMode(stored uint32, fallback fs.FileMode) uint32 {
	if stored == 0 {
		return LinuxModeFromFileMode(fallback)
	}
	if stored&linuxSIFMT != 0 {
		return stored & (linuxSIFMT | linuxPermMask)
	}
	return LinuxModeFromFileMode(fs.FileMode(stored))
}

func LinuxModeFromFileMode(mode fs.FileMode) uint32 {
	perm := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		perm |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		perm |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		perm |= 0o1000
	}
	switch {
	case mode&fs.ModeDir != 0:
		perm |= linuxSIFDIR
	case mode&fs.ModeSymlink != 0:
		perm |= linuxSIFLNK
	case mode&fs.ModeNamedPipe != 0:
		perm |= linuxSIFIFO
	case mode&fs.ModeDevice != 0 && mode&fs.ModeCharDevice != 0:
		perm |= linuxSIFCHR
	case mode&fs.ModeDevice != 0:
		perm |= linuxSIFBLK
	case mode&fs.ModeSocket != 0:
		perm |= linuxSIFSOCK
	default:
		perm |= linuxSIFREG
	}
	return perm
}
