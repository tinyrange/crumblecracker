package common

import (
	"io"
	"io/fs"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

type FileHandle interface {
	io.ReaderAt // Read from the file at a specific offset
	io.Closer   // Close the file handle
}

type Node interface {
	Name() string // The base name of the file or directory
	Size() int64  // The size of the file in bytes
	IsDir() bool  // True if this is a directory, false if it's a file
}

type UnixNode interface {
	Node
	Mode() fs.FileMode     // Get the file mode (permissions)
	Owner() (uid, gid int) // Get the owner and group IDs
	ModTime() time.Time    // Get the last modification time of the file
}

type FileSystemReader interface {
	IterateNodes(callback func(node Node) error) error             // Iterate through all nodes in the file system
	RootDirectory() (Node, error)                                  // Get the root directory node
	ReadContents(node Node) (FileHandle, error)                    // Read the contents of a file node
	ReadDirectory(node Node, callback func(node Node) error) error // Read the contents of a directory node
}

type UnixFileSystemReader interface {
	FileSystemReader
	ReadSymbolicLink(node Node) (string, error) // Read the target of a symbolic link
}

type WritableNode interface {
	Node
	SetName(name string) error // Set the name of the node
}

type WritableUnixNode interface {
	WritableNode
	UnixNode
	SetPermissions(permissions fs.FileMode) error // Set the permissions of the node
	SetOwner(uid, gid int) error                  // Set the owner and group of the node
	SetModTime(modTime time.Time) error           // Set the last modification time of the node
}

type FileSystemWriter interface {
	AllocateNode() (WritableNode, error)                             // Allocate a new node for writing
	WriteContents(node WritableNode, data vm.MemoryRegion) error     // Write data to the node
	WriteDirectory(node WritableNode, children []WritableNode) error // Write directory contents
	WritableRootDirectory() (WritableNode, error)                    // Get the writable root directory node
	Finalize() error                                                 // Finalize the file system metadata
}

type UnixFileSystemWriter interface {
	FileSystemWriter
	WriteSymbolicLink(node WritableNode, target string) error     // Write a symbolic link node
	WriteHardLink(source WritableNode, target WritableNode) error // Write a hard link node
}
