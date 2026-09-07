package virtio

import (
	"encoding/binary"
	"fmt"
	"path"
	"strings"
	"time"
)

// fsRequestDispatcher is the only FUSE-aware dependency of the virtqueue
// transport. The queue driver owns descriptor movement and completion ordering;
// the dispatcher owns protocol decoding and filesystem policy.
type fsRequestDispatcher interface {
	Dispatch(raw []byte) (fsDispatchResult, error)
}

type fsDispatchResult struct {
	reply  fsReply
	opcode uint32
	unique uint64
}

// fuseServer owns the protocol-facing filesystem service. In particular, the
// backend and backing-usage mutation accounting do not belong to the virtqueue
// transport even though both share the FS device's logging and timing hooks.
type fuseServer struct {
	device              *FS
	backend             FSBackend
	backingUsageTracker *FSBackingUsageTracker
	locks               *fuseLockManager
}

func (s *fuseServer) Dispatch(raw []byte) (fsDispatchResult, error) {
	req, err := decodeFUSERequest(raw)
	if err != nil {
		return fsDispatchResult{opcode: req.opcode, unique: req.unique}, err
	}
	reply, err := s.dispatch(req)
	return fsDispatchResult{reply: reply, opcode: req.opcode, unique: req.unique}, err
}

func (s *fuseServer) dispatch(request fuseRequest) (fsReply, error) {
	f := s.device
	req := request.raw
	opcode := request.opcode
	tracker := s.backingUsageTracker
	if fuseMayChangeBacking(opcode) {
		defer tracker.TrackMutation()()
	}
	unique := request.unique
	nodeID := request.nodeID
	callerUID := request.callerUID
	callerGID := request.callerGID
	opStart := time.Now()
	defer f.recordFUSEDispatchTiming(opcode, opStart)
	logEnabled := f.Log != nil
	if logEnabled {
		f.logf("opcode=%d unique=%d node=%d", opcode, unique, nodeID)
	}

	reply := func(errno int32, extra []byte) fsReply {
		return fuseReply(unique, errno, extra)
	}

	switch opcode {
	case fuseForget:
		return fsReply{}, nil
	case fuseInit:
		if err := request.requireBody(16, "INIT"); err != nil {
			return fsReply{}, err
		}
		reqMajor := binary.LittleEndian.Uint32(req[40:44])
		reqMinor := binary.LittleEndian.Uint32(req[44:48])
		maxWrite, flags := s.backend.Init()
		flags |= fuseCapDoReadDirPlus
		if maxWrite == 0 {
			maxWrite = 128 << 10
		}
		if maxWrite > 4096 {
			flags |= fuseCapBigWrites | fuseCapMaxPages
		}
		if f.writebackCache {
			flags |= fuseCapWritebackCache
		}
		maxPages := (maxWrite + 4095) / 4096
		if maxPages == 0 {
			maxPages = 1
		}
		if maxPages > 0xffff {
			maxPages = 0xffff
		}
		extra := make([]byte, fuseInitOutSize)
		replyMajor := uint32(7)
		replyMinor := uint32(31)
		if reqMajor > 0 && reqMajor < replyMajor {
			replyMajor = reqMajor
		}
		if reqMajor == replyMajor && reqMinor > 0 && reqMinor < replyMinor {
			replyMinor = reqMinor
		}
		binary.LittleEndian.PutUint32(extra[0:4], replyMajor)
		binary.LittleEndian.PutUint32(extra[4:8], replyMinor)
		binary.LittleEndian.PutUint32(extra[8:12], 128<<10)
		binary.LittleEndian.PutUint32(extra[12:16], flags)
		maxBackground := uint16(16)
		congestionThreshold := uint16(32)
		if f.writebackCache {
			maxBackground = 256
			congestionThreshold = 192
		}
		binary.LittleEndian.PutUint16(extra[16:18], maxBackground)
		binary.LittleEndian.PutUint16(extra[18:20], congestionThreshold)
		binary.LittleEndian.PutUint32(extra[20:24], maxWrite)
		binary.LittleEndian.PutUint32(extra[24:28], 1)
		binary.LittleEndian.PutUint16(extra[28:30], uint16(maxPages))
		if logEnabled {
			f.logf("init-reply major=%d minor=%d max_write=%d", replyMajor, replyMinor, maxWrite)
		}
		return reply(0, extra), nil
	case fuseGetAttr:
		if logEnabled {
			s.logPathf("getattr", nodeID, "")
		}
		attr, errno := s.backend.GetAttr(nodeID)
		if errno != 0 {
			return reply(errno, nil), nil
		}
		extra := make([]byte, fuseAttrOutSize)
		encodeFuseAttrTTL(extra[0:16], s.cachePolicy(nodeID).AttrTTL)
		encodeFuseAttr(extra[16:], attr)
		return reply(0, extra), nil
	case fuseSetAttr:
		if err := request.requireBody(88, "SETATTR"); err != nil {
			return fsReply{}, err
		}
		if be, ok := s.backend.(fsSetAttrBackend); ok {
			valid := binary.LittleEndian.Uint32(req[40:44])
			fh := binary.LittleEndian.Uint64(req[48:56])
			size := binary.LittleEndian.Uint64(req[56:64])
			atime := time.Unix(int64(binary.LittleEndian.Uint64(req[72:80])), int64(binary.LittleEndian.Uint32(req[96:100])))
			mtime := time.Unix(int64(binary.LittleEndian.Uint64(req[80:88])), int64(binary.LittleEndian.Uint32(req[100:104])))
			mode := binary.LittleEndian.Uint32(req[108:112])
			uid := binary.LittleEndian.Uint32(req[116:120])
			gid := binary.LittleEndian.Uint32(req[120:124])
			var attr FuseAttr
			var errno int32
			if callerBE, ok := s.backend.(fsSetAttrCallerBackend); ok {
				attr, errno = callerBE.SetAttrForCaller(nodeID, valid, fh, size, mode, uid, gid, atime, mtime, callerUID, callerGID)
			} else {
				attr, errno = be.SetAttr(nodeID, valid, fh, size, mode, uid, gid, atime, mtime)
			}
			if errno != 0 {
				return reply(errno, nil), nil
			}
			extra := make([]byte, fuseAttrOutSize)
			encodeFuseAttrTTL(extra[0:16], s.cachePolicy(nodeID).AttrTTL)
			encodeFuseAttr(extra[16:], attr)
			return reply(0, extra), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseLookup:
		name := readCStringName(req[fuseInHeaderSize:])
		if logEnabled {
			s.logPathf("lookup-parent", nodeID, fmt.Sprintf(" name=%q", name))
		}
		childID, attr, errno := s.backend.Lookup(nodeID, path.Clean(name))
		if errno != 0 {
			return reply(errno, nil), nil
		}
		if logEnabled {
			s.logPathf("lookup-child", childID, "")
		}
		extra := make([]byte, fuseEntryOutSize)
		s.encodeFuseEntryOut(extra, childID)
		encodeFuseAttr(extra[40:], attr)
		return reply(0, extra), nil
	case fuseMkdir:
		if err := request.requireBody(8, "MKDIR"); err != nil {
			return fsReply{}, err
		}
		name := readCStringName(req[fuseInHeaderSize+8:])
		mode := binary.LittleEndian.Uint32(req[40:44])
		if logEnabled {
			s.logPathf("mkdir-parent", nodeID, fmt.Sprintf(" name=%q mode=%#o", name, mode))
		}
		if be, ok := s.backend.(fsMkdirBackend); ok {
			childID, attr, errno := be.Mkdir(nodeID, path.Clean(name), mode, callerUID, callerGID)
			if errno != 0 {
				return reply(errno, nil), nil
			}
			extra := make([]byte, fuseEntryOutSize)
			s.encodeFuseEntryOut(extra, childID)
			encodeFuseAttr(extra[40:], attr)
			return reply(0, extra), nil
		}
		return fsReply{}, fmt.Errorf("virtio-fs missing mkdir backend for parent=%d name=%q", nodeID, name)
	case fuseMknod:
		if err := request.requireBody(16, "MKNOD"); err != nil {
			return fsReply{}, err
		}
		name := readCStringName(req[fuseInHeaderSize+16:])
		mode := binary.LittleEndian.Uint32(req[40:44])
		rdev := binary.LittleEndian.Uint32(req[44:48])
		if logEnabled {
			s.logPathf("mknod-parent", nodeID, fmt.Sprintf(" name=%q mode=%#o rdev=%#x", name, mode, rdev))
		}
		if be, ok := s.backend.(fsMknodBackend); ok {
			childID, attr, errno := be.Mknod(nodeID, path.Clean(name), mode, rdev, callerUID, callerGID)
			if errno != 0 {
				return reply(errno, nil), nil
			}
			extra := make([]byte, fuseEntryOutSize)
			s.encodeFuseEntryOut(extra, childID)
			encodeFuseAttr(extra[40:], attr)
			return reply(0, extra), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseSymlink:
		name, target, ok := readTwoCStringNames(req[fuseInHeaderSize:])
		if !ok {
			return fsReply{}, fmt.Errorf("virtio-fs SYMLINK malformed payload")
		}
		if logEnabled {
			s.logPathf("symlink-parent", nodeID, fmt.Sprintf(" name=%q target=%q", name, target))
		}
		if be, ok := s.backend.(fsSymlinkBackend); ok {
			childID, attr, errno := be.Symlink(nodeID, path.Clean(name), target, callerUID, callerGID)
			if errno != 0 {
				return reply(errno, nil), nil
			}
			extra := make([]byte, fuseEntryOutSize)
			s.encodeFuseEntryOut(extra, childID)
			encodeFuseAttr(extra[40:], attr)
			return reply(0, extra), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseUnlink:
		name := readCStringName(req[fuseInHeaderSize:])
		if be, ok := s.backend.(fsUnlinkBackend); ok {
			if callerBE, ok := s.backend.(fsUnlinkCallerBackend); ok {
				return reply(callerBE.UnlinkForCaller(nodeID, path.Clean(name), callerUID, callerGID), nil), nil
			}
			return reply(be.Unlink(nodeID, path.Clean(name)), nil), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseOpen:
		if err := request.requireBody(8, "OPEN"); err != nil {
			return fsReply{}, err
		}
		flags := binary.LittleEndian.Uint32(req[40:44])
		if logEnabled {
			s.logPathf("open", nodeID, fmt.Sprintf(" flags=%#x", flags))
		}
		var fh uint64
		var errno int32
		if be, ok := s.backend.(fsOpenCallerBackend); ok {
			fh, errno = be.OpenForCaller(nodeID, flags, callerUID, callerGID)
		} else {
			fh, errno = s.backend.Open(nodeID, flags)
		}
		if errno != 0 {
			return reply(errno, nil), nil
		}
		extra := make([]byte, fuseOpenOutSize)
		binary.LittleEndian.PutUint64(extra[0:8], fh)
		binary.LittleEndian.PutUint32(extra[8:12], s.openResponseFlags(nodeID, flags, false))
		return reply(0, extra), nil
	case fuseRead:
		if err := request.requireBody(24, "READ"); err != nil {
			return fsReply{}, err
		}
		fh := binary.LittleEndian.Uint64(req[40:48])
		off := binary.LittleEndian.Uint64(req[48:56])
		size := binary.LittleEndian.Uint32(req[56:60])
		if logEnabled {
			s.logPathf("read", nodeID, fmt.Sprintf(" fh=%d off=%d size=%d", fh, off, size))
		}
		data, errno := s.backend.Read(nodeID, fh, off, size)
		if errno != 0 {
			return reply(errno, nil), nil
		}
		return reply(0, data), nil
	case fuseWrite:
		if err := request.requireBody(40, "WRITE"); err != nil {
			return fsReply{}, err
		}
		if be, ok := s.backend.(fsWriteBackend); ok {
			fh := binary.LittleEndian.Uint64(req[40:48])
			off := binary.LittleEndian.Uint64(req[48:56])
			size := binary.LittleEndian.Uint32(req[56:60])
			writeFlags := binary.LittleEndian.Uint32(req[60:64])
			data, err := request.bodyBytes(40, size, "WRITE")
			if err != nil {
				return fsReply{}, err
			}
			var count uint32
			var errno int32
			if callerBE, ok := s.backend.(fsWriteCallerBackend); ok {
				count, errno = callerBE.WriteForCaller(nodeID, fh, off, data, writeFlags, callerUID, callerGID)
			} else {
				count, errno = be.Write(nodeID, fh, off, data, writeFlags)
			}
			if errno != 0 {
				return reply(errno, nil), nil
			}
			extra := make([]byte, fuseWriteOutSize)
			binary.LittleEndian.PutUint32(extra[0:4], count)
			return reply(0, extra), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseRelease:
		if err := request.requireBody(24, "RELEASE"); err != nil {
			return fsReply{}, err
		}
		fh := binary.LittleEndian.Uint64(req[40:48])
		releaseFlags := binary.LittleEndian.Uint32(req[52:56])
		lockOwner := binary.LittleEndian.Uint64(req[56:64])
		if logEnabled {
			s.logPathf("release", nodeID, fmt.Sprintf(" fh=%d", fh))
		}
		if releaseFlags&fuseReleaseFlockUnlock != 0 {
			s.locks.releaseOwner(nodeID, lockOwner)
		}
		s.backend.Release(nodeID, fh)
		return reply(0, nil), nil
	case fuseFsync:
		if err := request.requireBody(16, "FSYNC"); err != nil {
			return fsReply{}, err
		}
		fh := binary.LittleEndian.Uint64(req[40:48])
		flags := binary.LittleEndian.Uint32(req[48:52])
		if logEnabled {
			s.logPathf("fsync", nodeID, fmt.Sprintf(" fh=%d flags=%#x", fh, flags))
		}
		if be, ok := s.backend.(fsFsyncBackend); ok {
			return reply(be.Fsync(nodeID, fh, flags), nil), nil
		}
		if f.Strict {
			return fsReply{}, fmt.Errorf("virtio-fs missing fsync backend for FSYNC node=%d fh=%d", nodeID, fh)
		}
		return reply(0, nil), nil
	case fuseOpenDir:
		if err := request.requireBody(8, "OPENDIR"); err != nil {
			return fsReply{}, err
		}
		flags := binary.LittleEndian.Uint32(req[40:44])
		if logEnabled {
			s.logPathf("opendir", nodeID, fmt.Sprintf(" flags=%#x", flags))
		}
		fh, errno := s.backend.OpenDir(nodeID, flags)
		if errno != 0 {
			return reply(errno, nil), nil
		}
		extra := make([]byte, fuseOpenOutSize)
		binary.LittleEndian.PutUint64(extra[0:8], fh)
		binary.LittleEndian.PutUint32(extra[8:12], s.openResponseFlags(nodeID, flags, true))
		return reply(0, extra), nil
	case fuseReadDir, fuseReadDirPlus:
		if err := request.requireBody(24, fuseOpcodeName(opcode)); err != nil {
			return fsReply{}, err
		}
		fh := binary.LittleEndian.Uint64(req[40:48])
		off := binary.LittleEndian.Uint64(req[48:56])
		size := binary.LittleEndian.Uint32(req[56:60])
		if logEnabled {
			s.logPathf("readdir", nodeID, fmt.Sprintf(" fh=%d off=%d size=%d", fh, off, size))
		}
		var data []byte
		var errno int32
		if opcode == fuseReadDirPlus {
			data, errno = s.readDirPlus(nodeID, fh, off, size)
		} else {
			data, errno = s.backend.ReadDir(nodeID, fh, off, size)
		}
		if errno != 0 {
			return reply(errno, nil), nil
		}
		return reply(0, data), nil
	case fuseReleaseDir:
		if err := request.requireBody(24, "RELEASEDIR"); err != nil {
			return fsReply{}, err
		}
		if logEnabled {
			s.logPathf("releasedir", nodeID, fmt.Sprintf(" fh=%d", binary.LittleEndian.Uint64(req[40:48])))
		}
		s.backend.ReleaseDir(nodeID, binary.LittleEndian.Uint64(req[40:48]))
		return reply(0, nil), nil
	case fuseFsyncDir:
		if err := request.requireBody(16, "FSYNCDIR"); err != nil {
			return fsReply{}, err
		}
		fh := binary.LittleEndian.Uint64(req[40:48])
		flags := binary.LittleEndian.Uint32(req[48:52])
		if logEnabled {
			s.logPathf("fsyncdir", nodeID, fmt.Sprintf(" fh=%d flags=%#x", fh, flags))
		}
		if be, ok := s.backend.(fsFsyncDirBackend); ok {
			return reply(be.FsyncDir(nodeID, fh, flags), nil), nil
		}
		if f.Strict {
			return fsReply{}, fmt.Errorf("virtio-fs missing fsyncdir backend for FSYNCDIR node=%d fh=%d", nodeID, fh)
		}
		return reply(0, nil), nil
	case fuseGetLK:
		if err := request.requireBody(fuseLKInSize, "GETLK"); err != nil {
			return fsReply{}, err
		}
		fh := binary.LittleEndian.Uint64(req[40:48])
		owner := binary.LittleEndian.Uint64(req[48:56])
		requested := fuseFileLock{
			nodeID: nodeID, owner: owner,
			start:  binary.LittleEndian.Uint64(req[56:64]),
			end:    binary.LittleEndian.Uint64(req[64:72]),
			typeID: binary.LittleEndian.Uint32(req[72:76]),
			pid:    binary.LittleEndian.Uint32(req[76:80]),
		}
		if logEnabled {
			s.logPathf("getlk", nodeID, fmt.Sprintf(" fh=%d", fh))
		}
		if !validFuseGetLock(requested) {
			return reply(-linuxEINVAL, nil), nil
		}
		conflict, found := s.locks.get(requested)
		if !found {
			conflict = fuseFileLock{typeID: linuxFUnlck}
		}
		extra := make([]byte, fuseLKOutSize)
		binary.LittleEndian.PutUint64(extra[0:8], conflict.start)
		binary.LittleEndian.PutUint64(extra[8:16], conflict.end)
		binary.LittleEndian.PutUint32(extra[16:20], conflict.typeID)
		binary.LittleEndian.PutUint32(extra[20:24], conflict.pid)
		return reply(0, extra), nil
	case fuseSetLK, fuseSetLKW:
		if err := request.requireBody(fuseLKInSize, fuseOpcodeName(opcode)); err != nil {
			return fsReply{}, err
		}
		fh := binary.LittleEndian.Uint64(req[40:48])
		lock := fuseFileLock{
			nodeID: nodeID,
			owner:  binary.LittleEndian.Uint64(req[48:56]),
			start:  binary.LittleEndian.Uint64(req[56:64]),
			end:    binary.LittleEndian.Uint64(req[64:72]),
			typeID: binary.LittleEndian.Uint32(req[72:76]),
			pid:    binary.LittleEndian.Uint32(req[76:80]),
		}
		if logEnabled {
			s.logPathf(strings.ToLower(fuseOpcodeName(opcode)), nodeID, fmt.Sprintf(" fh=%d type=%d", fh, lock.typeID))
		}
		return reply(s.locks.set(lock, opcode == fuseSetLKW), nil), nil
	case fuseRmDir:
		name := readCStringName(req[fuseInHeaderSize:])
		if logEnabled {
			s.logPathf("rmdir-parent", nodeID, fmt.Sprintf(" name=%q", name))
		}
		if be, ok := s.backend.(fsRmDirBackend); ok {
			var errno int32
			if callerBE, ok := s.backend.(fsRmDirCallerBackend); ok {
				errno = callerBE.RmDirForCaller(nodeID, path.Clean(name), callerUID, callerGID)
			} else {
				errno = be.RmDir(nodeID, path.Clean(name))
			}
			return reply(errno, nil), nil
		}
		return fsReply{}, fmt.Errorf("virtio-fs missing rmdir backend for parent=%d name=%q", nodeID, name)
	case fuseRename:
		if err := request.requireBody(8, "RENAME"); err != nil {
			return fsReply{}, err
		}
		if be, ok := s.backend.(fsRenameBackend); ok {
			newParent := binary.LittleEndian.Uint64(req[40:48])
			names := req[fuseInHeaderSize+8:]
			split := bytesIndexByte(names, 0)
			if split < 0 {
				return fsReply{}, fmt.Errorf("virtio-fs RENAME missing old name")
			}
			oldName := string(names[:split])
			newName := readCStringName(names[split+1:])
			if callerBE, ok := s.backend.(fsRenameCallerBackend); ok {
				return reply(callerBE.RenameForCaller(nodeID, path.Clean(oldName), newParent, path.Clean(newName), 0, callerUID, callerGID), nil), nil
			}
			return reply(be.Rename(nodeID, path.Clean(oldName), newParent, path.Clean(newName), 0), nil), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseRename2:
		if err := request.requireBody(16, "RENAME2"); err != nil {
			return fsReply{}, err
		}
		if be, ok := s.backend.(fsRenameBackend); ok {
			newParent := binary.LittleEndian.Uint64(req[40:48])
			flags := binary.LittleEndian.Uint32(req[48:52])
			// The in-memory backend can exchange both directory entries
			// atomically, but Linux's mounted FUSE path has exhibited stale
			// dentry aliasing after a successful exchange. Refuse the operation
			// until the kernel-cache coherence contract is implemented; an
			// explicit unsupported result is safer than reporting success after
			// data loss.
			if flags&linuxRenameExchange != 0 {
				// ENOSYS makes Linux permanently disable the entire RENAME2
				// opcode for this mount. Reject only this flag so later
				// RENAME_NOREPLACE requests continue reaching the backend.
				return reply(-linuxEOPNOTSUPP, nil), nil
			}
			names := req[fuseInHeaderSize+16:]
			split := bytesIndexByte(names, 0)
			if split < 0 {
				return fsReply{}, fmt.Errorf("virtio-fs RENAME2 missing old name")
			}
			oldName := string(names[:split])
			newName := readCStringName(names[split+1:])
			if callerBE, ok := s.backend.(fsRenameCallerBackend); ok {
				return reply(callerBE.RenameForCaller(nodeID, path.Clean(oldName), newParent, path.Clean(newName), flags, callerUID, callerGID), nil), nil
			}
			return reply(be.Rename(nodeID, path.Clean(oldName), newParent, path.Clean(newName), flags), nil), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseLink:
		if err := request.requireBody(8, "LINK"); err != nil {
			return fsReply{}, err
		}
		if be, ok := s.backend.(fsLinkBackend); ok {
			oldNodeID := binary.LittleEndian.Uint64(req[40:48])
			newParent := nodeID
			newName := readCStringName(req[fuseInHeaderSize+8:])
			var childID uint64
			var attr FuseAttr
			var errno int32
			if callerBE, ok := s.backend.(fsLinkCallerBackend); ok {
				childID, attr, errno = callerBE.LinkForCaller(oldNodeID, newParent, path.Clean(newName), callerUID, callerGID)
			} else {
				childID, attr, errno = be.Link(oldNodeID, newParent, path.Clean(newName))
			}
			if errno != 0 {
				return reply(errno, nil), nil
			}
			extra := make([]byte, fuseEntryOutSize)
			s.encodeFuseEntryOut(extra, childID)
			encodeFuseAttr(extra[40:], attr)
			return reply(0, extra), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseReadlink:
		if logEnabled {
			s.logPathf("readlink", nodeID, "")
		}
		target, errno := s.backend.Readlink(nodeID)
		if errno != 0 {
			return reply(errno, nil), nil
		}
		return reply(0, []byte(target)), nil
	case fuseSetXattr:
		if err := request.requireBody(8, "SETXATTR"); err != nil {
			return fsReply{}, err
		}
		size := binary.LittleEndian.Uint32(req[40:44])
		flags := binary.LittleEndian.Uint32(req[44:48])
		payload := req[fuseInHeaderSize+8:]
		split := bytesIndexByte(payload, 0)
		if split < 0 || uint64(split+1)+uint64(size) > uint64(len(payload)) {
			return fsReply{}, fmt.Errorf("virtio-fs SETXATTR malformed payload")
		}
		name := string(payload[:split])
		value := payload[split+1 : split+1+int(size)]
		if be, ok := s.backend.(fsXattrMutationBackend); ok {
			return reply(be.SetXattr(nodeID, name, value, flags), nil), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseGetXattr:
		if err := request.requireBody(8, "GETXATTR"); err != nil {
			return fsReply{}, err
		}
		size := binary.LittleEndian.Uint32(req[40:44])
		name := readCStringName(req[fuseInHeaderSize+8:])
		if logEnabled {
			s.logPathf("getxattr", nodeID, fmt.Sprintf(" name=%q size=%d", name, size))
		}
		if be, ok := s.backend.(fsXattrBackend); ok {
			value, errno := be.GetXattr(nodeID, name)
			if errno != 0 {
				return reply(errno, nil), nil
			}
			if size == 0 {
				extra := make([]byte, 8)
				binary.LittleEndian.PutUint32(extra[0:4], uint32(len(value)))
				return reply(0, extra), nil
			}
			if uint32(len(value)) > size {
				return reply(-linuxERANGE, nil), nil
			}
			return reply(0, value), nil
		}
		if f.Strict {
			return fsReply{}, fmt.Errorf("virtio-fs missing xattr backend for GETXATTR node=%d", nodeID)
		}
		return reply(-linuxENODATA, nil), nil
	case fuseListXattr:
		if err := request.requireBody(8, "LISTXATTR"); err != nil {
			return fsReply{}, err
		}
		if logEnabled {
			s.logPathf("listxattr", nodeID, "")
		}
		if be, ok := s.backend.(fsXattrBackend); ok {
			value, errno := be.ListXattr(nodeID)
			if errno != 0 {
				return reply(errno, nil), nil
			}
			size := binary.LittleEndian.Uint32(req[40:44])
			if size == 0 {
				extra := make([]byte, 8)
				binary.LittleEndian.PutUint32(extra[0:4], uint32(len(value)))
				return reply(0, extra), nil
			}
			if uint32(len(value)) > size {
				return reply(-linuxERANGE, nil), nil
			}
			return reply(0, value), nil
		}
		if f.Strict {
			return fsReply{}, fmt.Errorf("virtio-fs missing xattr backend for LISTXATTR node=%d", nodeID)
		}
		return reply(0, nil), nil
	case fuseRemoveXattr:
		name := readCStringName(req[fuseInHeaderSize:])
		if be, ok := s.backend.(fsXattrMutationBackend); ok {
			return reply(be.RemoveXattr(nodeID, name), nil), nil
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseFlush:
		if err := request.requireBody(24, "FLUSH"); err != nil {
			return fsReply{}, err
		}
		fh := binary.LittleEndian.Uint64(req[40:48])
		flushFlags := binary.LittleEndian.Uint32(req[48:52])
		lockOwner := binary.LittleEndian.Uint64(req[56:64])
		if logEnabled {
			s.logPathf("flush", nodeID, fmt.Sprintf(" fh=%d lockOwner=%d", fh, lockOwner))
		}
		if flushFlags&fuseFlushLockOwner != 0 {
			s.locks.releaseOwner(nodeID, lockOwner)
		}
		if be, ok := s.backend.(fsFlushBackend); ok {
			return reply(be.Flush(nodeID, fh, lockOwner), nil), nil
		}
		if f.Strict {
			return fsReply{}, fmt.Errorf("virtio-fs missing flush backend for FLUSH node=%d fh=%d", nodeID, fh)
		}
		return reply(0, nil), nil
	case fuseAccess:
		if f.Strict {
			return fsReply{}, fmt.Errorf("virtio-fs unsupported opcode %s node=%d", fuseOpcodeName(opcode), nodeID)
		}
		return reply(-linuxENOSYS, nil), nil
	case fusePoll:
		// Regular files are always ready for I/O and this device has no
		// FUSE_NOTIFY_POLL implementation. ENOSYS makes the kernel remember
		// that poll is unsupported and use its default regular-file mask.
		// Returning an empty mask instead registers a waiter that can never be
		// notified, which stalls io_uring reads indefinitely.
		return reply(-linuxENOSYS, nil), nil
	case fuseLseek:
		if err := request.requireBody(24, "LSEEK"); err != nil {
			return fsReply{}, err
		}
		if be, ok := s.backend.(fsLseekBackend); ok {
			fh := binary.LittleEndian.Uint64(req[40:48])
			offset := binary.LittleEndian.Uint64(req[48:56])
			whence := binary.LittleEndian.Uint32(req[56:60])
			if logEnabled {
				s.logPathf("lseek", nodeID, fmt.Sprintf(" fh=%d off=%d whence=%d", fh, offset, whence))
			}
			newOff, errno := be.Lseek(nodeID, fh, offset, whence)
			if errno != 0 {
				return reply(errno, nil), nil
			}
			extra := make([]byte, 8)
			binary.LittleEndian.PutUint64(extra[0:8], newOff)
			return reply(0, extra), nil
		}
		if f.Strict {
			return fsReply{}, fmt.Errorf("virtio-fs missing lseek backend for LSEEK node=%d", nodeID)
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseStatfs:
		blocks, bfree, bavail, files, ffree, bsize, frsize, namelen, errno := s.backend.StatFS(nodeID)
		if errno != 0 {
			return reply(errno, nil), nil
		}
		extra := make([]byte, fuseStatfsOutSize)
		binary.LittleEndian.PutUint64(extra[0:8], blocks)
		binary.LittleEndian.PutUint64(extra[8:16], bfree)
		binary.LittleEndian.PutUint64(extra[16:24], bavail)
		binary.LittleEndian.PutUint64(extra[24:32], files)
		binary.LittleEndian.PutUint64(extra[32:40], ffree)
		binary.LittleEndian.PutUint32(extra[40:44], uint32(bsize))
		binary.LittleEndian.PutUint32(extra[44:48], uint32(namelen))
		binary.LittleEndian.PutUint32(extra[48:52], uint32(frsize))
		return reply(0, extra), nil
	case fuseStatx:
		if err := request.requireBody(24, "STATX"); err != nil {
			return fsReply{}, err
		}
		if logEnabled {
			s.logPathf("statx", nodeID, fmt.Sprintf(" mask=%#x", binary.LittleEndian.Uint32(req[60:64])))
		}
		attr, errno := s.backend.GetAttr(nodeID)
		if errno != 0 {
			return reply(errno, nil), nil
		}
		extra := make([]byte, fuseStatxOutSize)
		encodeFuseAttrTTL(extra[0:16], s.cachePolicy(nodeID).AttrTTL)
		encodeFuseStatx(extra[32:], attr)
		return reply(0, extra), nil
	case fuseSyncFS:
		if be, ok := s.backend.(fsSyncFSBackend); ok {
			return reply(be.SyncFS(), nil), nil
		}
		return reply(0, nil), nil
	case fuseTmpfile:
		if logEnabled {
			s.logPathf("tmpfile", nodeID, "")
		}
		return reply(-linuxENOSYS, nil), nil
	case fuseDestroy:
		s.locks.close()
		return reply(0, nil), nil
	case fuseIoctl:
		if logEnabled {
			s.logPathf("ioctl", nodeID, "")
		}
		return reply(-linuxENOTTY, nil), nil
	case fuseCreate:
		if err := request.requireBody(16, "CREATE"); err != nil {
			return fsReply{}, err
		}
		if be, ok := s.backend.(fsCreateBackend); ok {
			flags := binary.LittleEndian.Uint32(req[40:44])
			mode := binary.LittleEndian.Uint32(req[44:48])
			name := readCStringName(req[fuseInHeaderSize+16:])
			var childID uint64
			var fh uint64
			var attr FuseAttr
			var errno int32
			if callerBE, ok := s.backend.(fsCreateCallerBackend); ok {
				childID, fh, attr, errno = callerBE.CreateForCaller(nodeID, path.Clean(name), flags, mode, callerUID, callerGID)
			} else {
				childID, fh, attr, errno = be.Create(nodeID, path.Clean(name), flags, mode, callerUID, callerGID)
			}
			if errno != 0 {
				return reply(errno, nil), nil
			}
			extra := make([]byte, fuseEntryOutSize+fuseOpenOutSize)
			s.encodeFuseEntryOut(extra[:fuseEntryOutSize], childID)
			encodeFuseAttr(extra[40:], attr)
			binary.LittleEndian.PutUint64(extra[fuseEntryOutSize:fuseEntryOutSize+8], fh)
			binary.LittleEndian.PutUint32(extra[fuseEntryOutSize+8:fuseEntryOutSize+12], s.openResponseFlags(childID, flags, false))
			return reply(0, extra), nil
		}
		return reply(-linuxENOSYS, nil), nil
	default:
		return reply(-linuxENOSYS, nil), nil
	}
}
