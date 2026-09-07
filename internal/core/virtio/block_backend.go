package virtio

import "io"

type BlockBackend interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
}
