package linuxabi

const (
	SIFMT    = 0o170000
	SIFSOCK  = 0o140000
	SIFLNK   = 0o120000
	SIFREG   = 0o100000
	SIFBLK   = 0o060000
	SIFDIR   = 0o040000
	SIFCHR   = 0o020000
	SIFIFO   = 0o010000
	PermMask = 0o7777
)

const (
	EPERM      int32 = 1
	ENOENT     int32 = 2
	EINTR      int32 = 4
	EIO        int32 = 5
	ENXIO      int32 = 6
	EBADF      int32 = 9
	EAGAIN     int32 = 11
	EACCES     int32 = 13
	EBUSY      int32 = 16
	EEXIST     int32 = 17
	EXDEV      int32 = 18
	ENOTDIR    int32 = 20
	EISDIR     int32 = 21
	EINVAL     int32 = 22
	ENOTTY     int32 = 25
	EFBIG      int32 = 27
	ENOSPC     int32 = 28
	EROFS      int32 = 30
	EPIPE      int32 = 32
	ERANGE     int32 = 34
	ENOSYS     int32 = 38
	ENOTEMPTY  int32 = 39
	ENODATA    int32 = 61
	EOPNOTSUPP int32 = 95
	ETIMEDOUT  int32 = 110
)
