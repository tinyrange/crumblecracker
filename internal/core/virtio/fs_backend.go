package virtio

import "time"

type PersistentFSStatus struct {
	Name               string
	Mount              string
	FormatVersion      uint32
	LowerID            string
	PreviousLowerID    string
	Sequence           uint64
	DurableSequence    uint64
	UpperLogicalBytes  uint64
	UpperDataBytes     uint64
	UpperPhysicalBytes uint64
	WALBytes           uint64
	StagingBytes       uint64
	TrashBytes         uint64
	RecoveryStatus     string
	QuarantinePath     string
	DiscardedBytes     uint64
	LastCheckpoint     time.Time
	LastError          string
	HostFreeBytes      uint64
}

// FSBackend owns the filesystem namespace, inode identities, and open handles.
// The FUSE dispatcher owns protocol decoding; the virtqueue driver never calls
// a backend directly.
//
// Node ID 1 is the root. Because this server deliberately does not forward
// FUSE_FORGET, every node ID returned by Lookup or a mutation must remain valid
// for the backend lifetime. A successful Open handle remains valid until its
// matching Release, including after unlink or rename; directory handles follow
// the same rule through ReleaseDir. Methods may be called concurrently, so the
// backend owns synchronization for namespace and handle state.
//
// Optional BeginClose implementations are called first to interrupt backend
// work. Close is called only after queue workers have stopped, and an incomplete
// Close may be retried.
type FSBackend interface {
	Init() (maxWrite uint32, flags uint32)
	GetAttr(nodeID uint64) (FuseAttr, int32)
	Lookup(parent uint64, name string) (nodeID uint64, attr FuseAttr, errno int32)
	Open(nodeID uint64, flags uint32) (fh uint64, errno int32)
	Release(nodeID uint64, fh uint64)
	Read(nodeID uint64, fh uint64, off uint64, size uint32) ([]byte, int32)
	OpenDir(nodeID uint64, flags uint32) (fh uint64, errno int32)
	ReadDir(nodeID uint64, fh uint64, off uint64, maxBytes uint32) ([]byte, int32)
	ReleaseDir(nodeID uint64, fh uint64)
	Readlink(nodeID uint64) (string, int32)
	StatFS(nodeID uint64) (blocks, bfree, bavail, files, ffree, bsize, frsize, namelen uint64, errno int32)
}

type fsXattrBackend interface {
	GetXattr(nodeID uint64, name string) ([]byte, int32)
	ListXattr(nodeID uint64) ([]byte, int32)
}

type fsXattrMutationBackend interface {
	SetXattr(nodeID uint64, name string, value []byte, flags uint32) int32
	RemoveXattr(nodeID uint64, name string) int32
}

type fsFlushBackend interface {
	Flush(nodeID uint64, fh uint64, lockOwner uint64) int32
}

type fsFsyncBackend interface {
	Fsync(nodeID uint64, fh uint64, flags uint32) int32
}

type fsFsyncDirBackend interface {
	FsyncDir(nodeID uint64, fh uint64, flags uint32) int32
}

type fsSyncFSBackend interface {
	SyncFS() int32
}

type fsLseekBackend interface {
	Lseek(nodeID uint64, fh uint64, offset uint64, whence uint32) (uint64, int32)
}

type fsMkdirBackend interface {
	Mkdir(parent uint64, name string, mode uint32, uid uint32, gid uint32) (nodeID uint64, attr FuseAttr, errno int32)
}

type fsMknodBackend interface {
	Mknod(parent uint64, name string, mode uint32, rdev uint32, uid uint32, gid uint32) (nodeID uint64, attr FuseAttr, errno int32)
}

type fsSymlinkBackend interface {
	Symlink(parent uint64, name string, target string, uid uint32, gid uint32) (nodeID uint64, attr FuseAttr, errno int32)
}

type fsLinkBackend interface {
	Link(nodeID uint64, newParent uint64, newName string) (newNodeID uint64, attr FuseAttr, errno int32)
}

type fsLinkCallerBackend interface {
	LinkForCaller(nodeID uint64, newParent uint64, newName string, uid uint32, gid uint32) (newNodeID uint64, attr FuseAttr, errno int32)
}

type fsRmDirBackend interface {
	RmDir(parent uint64, name string) int32
}

type fsRmDirCallerBackend interface {
	RmDirForCaller(parent uint64, name string, uid uint32, gid uint32) int32
}

type fsCreateBackend interface {
	Create(parent uint64, name string, flags uint32, mode uint32, uid uint32, gid uint32) (nodeID uint64, fh uint64, attr FuseAttr, errno int32)
}

type fsOpenCallerBackend interface {
	OpenForCaller(nodeID uint64, flags uint32, uid uint32, gid uint32) (uint64, int32)
}

type fsCreateCallerBackend interface {
	CreateForCaller(parent uint64, name string, flags uint32, mode uint32, uid uint32, gid uint32) (nodeID uint64, fh uint64, attr FuseAttr, errno int32)
}

type fsWriteBackend interface {
	Write(nodeID uint64, fh uint64, off uint64, data []byte, flags uint32) (uint32, int32)
}

type fsWriteCallerBackend interface {
	WriteForCaller(nodeID uint64, fh uint64, off uint64, data []byte, flags uint32, uid uint32, gid uint32) (uint32, int32)
}

type fsSetAttrBackend interface {
	SetAttr(nodeID uint64, valid uint32, fh uint64, size uint64, mode uint32, uid uint32, gid uint32, atime time.Time, mtime time.Time) (FuseAttr, int32)
}

type fsSetAttrCallerBackend interface {
	SetAttrForCaller(nodeID uint64, valid uint32, fh uint64, size uint64, mode uint32, uid uint32, gid uint32, atime time.Time, mtime time.Time, callerUID uint32, callerGID uint32) (FuseAttr, int32)
}

type fsUnlinkBackend interface {
	Unlink(parent uint64, name string) int32
}

type fsUnlinkCallerBackend interface {
	UnlinkForCaller(parent uint64, name string, uid uint32, gid uint32) int32
}

type fsRenameBackend interface {
	Rename(parent uint64, name string, newParent uint64, newName string, flags uint32) int32
}

type fsRenameCallerBackend interface {
	RenameForCaller(parent uint64, name string, newParent uint64, newName string, flags uint32, uid uint32, gid uint32) int32
}

type fsWritebackCacheBackend interface {
	SetWritebackCache(enabled bool)
}

type fsCachePolicyBackend interface {
	CachePolicy(nodeID uint64) FSCachePolicy
}

type FSCachePolicy struct {
	Mode     string
	EntryTTL time.Duration
	AttrTTL  time.Duration
}

type FuseAttr struct {
	Ino       uint64
	Size      uint64
	Blocks    uint64
	ATimeSec  uint64
	MTimeSec  uint64
	CTimeSec  uint64
	ATimeNsec uint32
	MTimeNsec uint32
	CTimeNsec uint32
	Mode      uint32
	NLink     uint32
	UID       uint32
	GID       uint32
	RDev      uint32
	BlkSize   uint32
	Flags     uint32
}
