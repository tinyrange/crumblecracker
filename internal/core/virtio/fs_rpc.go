package virtio

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

type FSRPCConn interface {
	io.Reader
	io.Writer
	io.Closer
}

type fsRPCRequest struct {
	ID        uint64    `json:"id"`
	Op        string    `json:"op"`
	NodeID    uint64    `json:"node_id,omitempty"`
	Parent    uint64    `json:"parent,omitempty"`
	NewParent uint64    `json:"new_parent,omitempty"`
	FH        uint64    `json:"fh,omitempty"`
	Offset    uint64    `json:"offset,omitempty"`
	Size      uint64    `json:"size,omitempty"`
	Flags     uint32    `json:"flags,omitempty"`
	Mode      uint32    `json:"mode,omitempty"`
	RDev      uint32    `json:"rdev,omitempty"`
	UID       uint32    `json:"uid,omitempty"`
	GID       uint32    `json:"gid,omitempty"`
	Valid     uint32    `json:"valid,omitempty"`
	LockOwner uint64    `json:"lock_owner,omitempty"`
	Whence    uint32    `json:"whence,omitempty"`
	Name      string    `json:"name,omitempty"`
	NewName   string    `json:"new_name,omitempty"`
	Target    string    `json:"target,omitempty"`
	Data      []byte    `json:"data,omitempty"`
	ATime     time.Time `json:"atime,omitempty"`
	MTime     time.Time `json:"mtime,omitempty"`
}

type fsRPCResponse struct {
	ID        uint64        `json:"id"`
	Error     string        `json:"error,omitempty"`
	MaxWrite  uint32        `json:"max_write,omitempty"`
	Flags     uint32        `json:"flags,omitempty"`
	NodeID    uint64        `json:"node_id,omitempty"`
	NewNodeID uint64        `json:"new_node_id,omitempty"`
	FH        uint64        `json:"fh,omitempty"`
	Attr      FuseAttr      `json:"attr,omitempty"`
	Errno     int32         `json:"errno,omitempty"`
	Bytes     []byte        `json:"bytes,omitempty"`
	Text      string        `json:"text,omitempty"`
	Count     uint32        `json:"count,omitempty"`
	Offset    uint64        `json:"offset,omitempty"`
	Blocks    uint64        `json:"blocks,omitempty"`
	BFree     uint64        `json:"bfree,omitempty"`
	BAvail    uint64        `json:"bavail,omitempty"`
	Files     uint64        `json:"files,omitempty"`
	FFree     uint64        `json:"ffree,omitempty"`
	BSize     uint64        `json:"bsize,omitempty"`
	FRSize    uint64        `json:"frsize,omitempty"`
	NameLen   uint64        `json:"namelen,omitempty"`
	Policy    FSCachePolicy `json:"policy,omitempty"`
}

type FSRemoteBackend struct {
	conn FSRPCConn
	enc  *json.Encoder
	dec  *json.Decoder
	mu   sync.Mutex
	next uint64
}

func NewFSRemoteBackend(conn FSRPCConn) *FSRemoteBackend {
	return &FSRemoteBackend{
		conn: conn,
		enc:  json.NewEncoder(conn),
		dec:  json.NewDecoder(conn),
	}
}

func (b *FSRemoteBackend) Close() error {
	return b.conn.Close()
}

func (b *FSRemoteBackend) call(req fsRPCRequest) (fsRPCResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	req.ID = b.next
	if err := b.enc.Encode(req); err != nil {
		return fsRPCResponse{}, err
	}
	var resp fsRPCResponse
	if err := b.dec.Decode(&resp); err != nil {
		return fsRPCResponse{}, err
	}
	if resp.ID != req.ID {
		return fsRPCResponse{}, fmt.Errorf("virtiofs rpc response id %d does not match request id %d", resp.ID, req.ID)
	}
	if resp.Error != "" {
		return fsRPCResponse{}, fmt.Errorf("%s", resp.Error)
	}
	return resp, nil
}

func (b *FSRemoteBackend) Init() (uint32, uint32) {
	resp, err := b.call(fsRPCRequest{Op: "init"})
	if err != nil {
		return 0, 0
	}
	return resp.MaxWrite, resp.Flags
}

func (b *FSRemoteBackend) GetAttr(nodeID uint64) (FuseAttr, int32) {
	resp, err := b.call(fsRPCRequest{Op: "getattr", NodeID: nodeID})
	if err != nil {
		return FuseAttr{}, -linuxEIO
	}
	return resp.Attr, resp.Errno
}

func (b *FSRemoteBackend) Lookup(parent uint64, name string) (uint64, FuseAttr, int32) {
	resp, err := b.call(fsRPCRequest{Op: "lookup", Parent: parent, Name: name})
	if err != nil {
		return 0, FuseAttr{}, -linuxEIO
	}
	return resp.NodeID, resp.Attr, resp.Errno
}

func (b *FSRemoteBackend) Open(nodeID uint64, flags uint32) (uint64, int32) {
	resp, err := b.call(fsRPCRequest{Op: "open", NodeID: nodeID, Flags: flags})
	if err != nil {
		return 0, -linuxEIO
	}
	return resp.FH, resp.Errno
}

func (b *FSRemoteBackend) Release(nodeID uint64, fh uint64) {
	_, _ = b.call(fsRPCRequest{Op: "release", NodeID: nodeID, FH: fh})
}

func (b *FSRemoteBackend) Read(nodeID uint64, fh uint64, off uint64, size uint32) ([]byte, int32) {
	resp, err := b.call(fsRPCRequest{Op: "read", NodeID: nodeID, FH: fh, Offset: off, Size: uint64(size)})
	if err != nil {
		return nil, -linuxEIO
	}
	return resp.Bytes, resp.Errno
}

func (b *FSRemoteBackend) OpenDir(nodeID uint64, flags uint32) (uint64, int32) {
	resp, err := b.call(fsRPCRequest{Op: "opendir", NodeID: nodeID, Flags: flags})
	if err != nil {
		return 0, -linuxEIO
	}
	return resp.FH, resp.Errno
}

func (b *FSRemoteBackend) ReadDir(nodeID uint64, fh uint64, off uint64, maxBytes uint32) ([]byte, int32) {
	resp, err := b.call(fsRPCRequest{Op: "readdir", NodeID: nodeID, FH: fh, Offset: off, Size: uint64(maxBytes)})
	if err != nil {
		return nil, -linuxEIO
	}
	return resp.Bytes, resp.Errno
}

func (b *FSRemoteBackend) ReleaseDir(nodeID uint64, fh uint64) {
	_, _ = b.call(fsRPCRequest{Op: "releasedir", NodeID: nodeID, FH: fh})
}

func (b *FSRemoteBackend) Readlink(nodeID uint64) (string, int32) {
	resp, err := b.call(fsRPCRequest{Op: "readlink", NodeID: nodeID})
	if err != nil {
		return "", -linuxEIO
	}
	return resp.Text, resp.Errno
}

func (b *FSRemoteBackend) StatFS(nodeID uint64) (uint64, uint64, uint64, uint64, uint64, uint64, uint64, uint64, int32) {
	resp, err := b.call(fsRPCRequest{Op: "statfs", NodeID: nodeID})
	if err != nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, -linuxEIO
	}
	return resp.Blocks, resp.BFree, resp.BAvail, resp.Files, resp.FFree, resp.BSize, resp.FRSize, resp.NameLen, resp.Errno
}

func (b *FSRemoteBackend) GetXattr(nodeID uint64, name string) ([]byte, int32) {
	resp, err := b.call(fsRPCRequest{Op: "getxattr", NodeID: nodeID, Name: name})
	if err != nil {
		return nil, -linuxEIO
	}
	return resp.Bytes, resp.Errno
}

func (b *FSRemoteBackend) ListXattr(nodeID uint64) ([]byte, int32) {
	resp, err := b.call(fsRPCRequest{Op: "listxattr", NodeID: nodeID})
	if err != nil {
		return nil, -linuxEIO
	}
	return resp.Bytes, resp.Errno
}

func (b *FSRemoteBackend) SetXattr(nodeID uint64, name string, value []byte, flags uint32) int32 {
	resp, err := b.call(fsRPCRequest{Op: "setxattr", NodeID: nodeID, Name: name, Data: value, Flags: flags})
	if err != nil {
		return -linuxEIO
	}
	return resp.Errno
}

func (b *FSRemoteBackend) RemoveXattr(nodeID uint64, name string) int32 {
	resp, err := b.call(fsRPCRequest{Op: "removexattr", NodeID: nodeID, Name: name})
	if err != nil {
		return -linuxEIO
	}
	return resp.Errno
}

func (b *FSRemoteBackend) Flush(nodeID uint64, fh uint64, lockOwner uint64) int32 {
	resp, err := b.call(fsRPCRequest{Op: "flush", NodeID: nodeID, FH: fh, LockOwner: lockOwner})
	if err != nil {
		return -linuxEIO
	}
	return resp.Errno
}

func (b *FSRemoteBackend) Fsync(nodeID uint64, fh uint64, flags uint32) int32 {
	resp, err := b.call(fsRPCRequest{Op: "fsync", NodeID: nodeID, FH: fh, Flags: flags})
	if err != nil {
		return -linuxEIO
	}
	return resp.Errno
}

func (b *FSRemoteBackend) FsyncDir(nodeID uint64, fh uint64, flags uint32) int32 {
	resp, err := b.call(fsRPCRequest{Op: "fsyncdir", NodeID: nodeID, FH: fh, Flags: flags})
	if err != nil {
		return -linuxEIO
	}
	return resp.Errno
}

func (b *FSRemoteBackend) Lseek(nodeID uint64, fh uint64, offset uint64, whence uint32) (uint64, int32) {
	resp, err := b.call(fsRPCRequest{Op: "lseek", NodeID: nodeID, FH: fh, Offset: offset, Whence: whence})
	if err != nil {
		return 0, -linuxEIO
	}
	return resp.Offset, resp.Errno
}

func (b *FSRemoteBackend) Mkdir(parent uint64, name string, mode uint32, uid uint32, gid uint32) (uint64, FuseAttr, int32) {
	resp, err := b.call(fsRPCRequest{Op: "mkdir", Parent: parent, Name: name, Mode: mode, UID: uid, GID: gid})
	if err != nil {
		return 0, FuseAttr{}, -linuxEIO
	}
	return resp.NodeID, resp.Attr, resp.Errno
}

func (b *FSRemoteBackend) Mknod(parent uint64, name string, mode uint32, rdev uint32, uid uint32, gid uint32) (uint64, FuseAttr, int32) {
	resp, err := b.call(fsRPCRequest{Op: "mknod", Parent: parent, Name: name, Mode: mode, RDev: rdev, UID: uid, GID: gid})
	if err != nil {
		return 0, FuseAttr{}, -linuxEIO
	}
	return resp.NodeID, resp.Attr, resp.Errno
}

func (b *FSRemoteBackend) Symlink(parent uint64, name string, target string, uid uint32, gid uint32) (uint64, FuseAttr, int32) {
	resp, err := b.call(fsRPCRequest{Op: "symlink", Parent: parent, Name: name, Target: target, UID: uid, GID: gid})
	if err != nil {
		return 0, FuseAttr{}, -linuxEIO
	}
	return resp.NodeID, resp.Attr, resp.Errno
}

func (b *FSRemoteBackend) Link(nodeID uint64, newParent uint64, newName string) (uint64, FuseAttr, int32) {
	resp, err := b.call(fsRPCRequest{Op: "link", NodeID: nodeID, NewParent: newParent, NewName: newName})
	if err != nil {
		return 0, FuseAttr{}, -linuxEIO
	}
	return resp.NewNodeID, resp.Attr, resp.Errno
}

func (b *FSRemoteBackend) RmDir(parent uint64, name string) int32 {
	resp, err := b.call(fsRPCRequest{Op: "rmdir", Parent: parent, Name: name})
	if err != nil {
		return -linuxEIO
	}
	return resp.Errno
}

func (b *FSRemoteBackend) Create(parent uint64, name string, flags uint32, mode uint32, uid uint32, gid uint32) (uint64, uint64, FuseAttr, int32) {
	resp, err := b.call(fsRPCRequest{Op: "create", Parent: parent, Name: name, Flags: flags, Mode: mode, UID: uid, GID: gid})
	if err != nil {
		return 0, 0, FuseAttr{}, -linuxEIO
	}
	return resp.NodeID, resp.FH, resp.Attr, resp.Errno
}

func (b *FSRemoteBackend) Write(nodeID uint64, fh uint64, off uint64, data []byte, flags uint32) (uint32, int32) {
	resp, err := b.call(fsRPCRequest{Op: "write", NodeID: nodeID, FH: fh, Offset: off, Data: data, Flags: flags})
	if err != nil {
		return 0, -linuxEIO
	}
	return resp.Count, resp.Errno
}

func (b *FSRemoteBackend) SetAttr(nodeID uint64, valid uint32, fh uint64, size uint64, mode uint32, uid uint32, gid uint32, atime time.Time, mtime time.Time) (FuseAttr, int32) {
	resp, err := b.call(fsRPCRequest{Op: "setattr", NodeID: nodeID, Valid: valid, FH: fh, Size: size, Mode: mode, UID: uid, GID: gid, ATime: atime, MTime: mtime})
	if err != nil {
		return FuseAttr{}, -linuxEIO
	}
	return resp.Attr, resp.Errno
}

func (b *FSRemoteBackend) Unlink(parent uint64, name string) int32 {
	resp, err := b.call(fsRPCRequest{Op: "unlink", Parent: parent, Name: name})
	if err != nil {
		return -linuxEIO
	}
	return resp.Errno
}

func (b *FSRemoteBackend) Rename(parent uint64, name string, newParent uint64, newName string, flags uint32) int32 {
	resp, err := b.call(fsRPCRequest{Op: "rename", Parent: parent, Name: name, NewParent: newParent, NewName: newName, Flags: flags})
	if err != nil {
		return -linuxEIO
	}
	return resp.Errno
}

func (b *FSRemoteBackend) CachePolicy(nodeID uint64) FSCachePolicy {
	resp, err := b.call(fsRPCRequest{Op: "cachepolicy", NodeID: nodeID})
	if err != nil {
		return FSCachePolicy{}
	}
	return resp.Policy
}

func ServeFSBackend(conn FSRPCConn, backend FSBackend) error {
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	for {
		var req fsRPCRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		resp := handleFSRPCRequest(backend, req)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

func handleFSRPCRequest(backend FSBackend, req fsRPCRequest) fsRPCResponse {
	resp := fsRPCResponse{ID: req.ID}
	if backend == nil {
		resp.Errno = -linuxEIO
		return resp
	}
	switch req.Op {
	case "init":
		resp.MaxWrite, resp.Flags = backend.Init()
	case "getattr":
		resp.Attr, resp.Errno = backend.GetAttr(req.NodeID)
	case "lookup":
		resp.NodeID, resp.Attr, resp.Errno = backend.Lookup(req.Parent, req.Name)
	case "open":
		resp.FH, resp.Errno = backend.Open(req.NodeID, req.Flags)
	case "release":
		backend.Release(req.NodeID, req.FH)
	case "read":
		resp.Bytes, resp.Errno = backend.Read(req.NodeID, req.FH, req.Offset, uint32(req.Size))
	case "opendir":
		resp.FH, resp.Errno = backend.OpenDir(req.NodeID, req.Flags)
	case "readdir":
		resp.Bytes, resp.Errno = backend.ReadDir(req.NodeID, req.FH, req.Offset, uint32(req.Size))
	case "releasedir":
		backend.ReleaseDir(req.NodeID, req.FH)
	case "readlink":
		resp.Text, resp.Errno = backend.Readlink(req.NodeID)
	case "statfs":
		resp.Blocks, resp.BFree, resp.BAvail, resp.Files, resp.FFree, resp.BSize, resp.FRSize, resp.NameLen, resp.Errno = backend.StatFS(req.NodeID)
	case "getxattr":
		be, ok := backend.(fsXattrBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Bytes, resp.Errno = be.GetXattr(req.NodeID, req.Name)
	case "listxattr":
		be, ok := backend.(fsXattrBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Bytes, resp.Errno = be.ListXattr(req.NodeID)
	case "setxattr":
		be, ok := backend.(fsXattrMutationBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Errno = be.SetXattr(req.NodeID, req.Name, req.Data, req.Flags)
	case "removexattr":
		be, ok := backend.(fsXattrMutationBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Errno = be.RemoveXattr(req.NodeID, req.Name)
	case "flush":
		be, ok := backend.(fsFlushBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Errno = be.Flush(req.NodeID, req.FH, req.LockOwner)
	case "fsync":
		be, ok := backend.(fsFsyncBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Errno = be.Fsync(req.NodeID, req.FH, req.Flags)
	case "fsyncdir":
		be, ok := backend.(fsFsyncDirBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Errno = be.FsyncDir(req.NodeID, req.FH, req.Flags)
	case "lseek":
		be, ok := backend.(fsLseekBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Offset, resp.Errno = be.Lseek(req.NodeID, req.FH, req.Offset, req.Whence)
	case "mkdir":
		be, ok := backend.(fsMkdirBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.NodeID, resp.Attr, resp.Errno = be.Mkdir(req.Parent, req.Name, req.Mode, req.UID, req.GID)
	case "mknod":
		be, ok := backend.(fsMknodBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.NodeID, resp.Attr, resp.Errno = be.Mknod(req.Parent, req.Name, req.Mode, req.RDev, req.UID, req.GID)
	case "symlink":
		be, ok := backend.(fsSymlinkBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.NodeID, resp.Attr, resp.Errno = be.Symlink(req.Parent, req.Name, req.Target, req.UID, req.GID)
	case "link":
		be, ok := backend.(fsLinkBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.NewNodeID, resp.Attr, resp.Errno = be.Link(req.NodeID, req.NewParent, req.NewName)
	case "rmdir":
		be, ok := backend.(fsRmDirBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Errno = be.RmDir(req.Parent, req.Name)
	case "create":
		be, ok := backend.(fsCreateBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.NodeID, resp.FH, resp.Attr, resp.Errno = be.Create(req.Parent, req.Name, req.Flags, req.Mode, req.UID, req.GID)
	case "write":
		be, ok := backend.(fsWriteBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Count, resp.Errno = be.Write(req.NodeID, req.FH, req.Offset, req.Data, req.Flags)
	case "setattr":
		be, ok := backend.(fsSetAttrBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Attr, resp.Errno = be.SetAttr(req.NodeID, req.Valid, req.FH, req.Size, req.Mode, req.UID, req.GID, req.ATime, req.MTime)
	case "unlink":
		be, ok := backend.(fsUnlinkBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Errno = be.Unlink(req.Parent, req.Name)
	case "rename":
		be, ok := backend.(fsRenameBackend)
		if !ok {
			resp.Errno = -linuxENOSYS
			break
		}
		resp.Errno = be.Rename(req.Parent, req.Name, req.NewParent, req.NewName, req.Flags)
	case "cachepolicy":
		be, ok := backend.(fsCachePolicyBackend)
		if ok {
			resp.Policy = be.CachePolicy(req.NodeID)
		}
	default:
		resp.Errno = -linuxENOSYS
	}
	return resp
}
