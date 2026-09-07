package amd64

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	headerMagicOffset  = 0x202
	headerMagic        = "HdrS"
	headerLengthOffset = 0x201
)

type SetupHeader struct {
	ProtocolVersion   uint16
	SetupSectors      uint8
	LoadFlags         uint8
	InitrdAddrMax     uint32
	KernelAlignment   uint32
	RelocatableKernel uint8
	MinAlignment      uint8
	XLoadFlags        uint16
	CmdlineSize       uint32
	PrefAddress       uint64
	InitSize          uint32
}

type KernelImage struct {
	Data          []byte
	Header        SetupHeader
	HeaderBytes   []byte
	PayloadOffset int
}

func LoadBzImage(kernel io.ReaderAt, kernelSize int64) (*KernelImage, error) {
	if kernel == nil {
		return nil, errors.New("amd64 kernel reader is nil")
	}
	if kernelSize <= 0 {
		return nil, errors.New("amd64 kernel size must be positive")
	}
	data, err := io.ReadAll(io.NewSectionReader(kernel, 0, kernelSize))
	if err != nil {
		return nil, fmt.Errorf("read bzImage kernel: %w", err)
	}
	img := &KernelImage{Data: data}
	if err := img.parseHeader(); err != nil {
		return nil, err
	}
	return img, nil
}

func (k *KernelImage) parseHeader() error {
	data := k.Data
	if len(data) < headerMagicOffset+4 {
		return errors.New("kernel image too small")
	}
	if !bytes.Equal(data[headerMagicOffset:headerMagicOffset+4], []byte(headerMagic)) {
		return errors.New("missing HdrS signature; not a Linux bzImage")
	}

	headerLength := int(data[headerLengthOffset])
	headerEnd := headerMagicOffset + headerLength
	if headerEnd > len(data) {
		return errors.New("setup header extends past end of image")
	}
	if headerEnd <= setupHeaderOffset {
		return errors.New("invalid setup header length")
	}
	k.HeaderBytes = append([]byte(nil), data[setupHeaderOffset:headerEnd]...)

	hdr := SetupHeader{
		SetupSectors:      data[setupHeaderOffset],
		ProtocolVersion:   binary.LittleEndian.Uint16(data[protocolVersionOffset:]),
		LoadFlags:         data[loadFlagsOffset],
		InitrdAddrMax:     binary.LittleEndian.Uint32(data[initrdAddrMaxOffset:]),
		KernelAlignment:   binary.LittleEndian.Uint32(data[kernelAlignmentOffset:]),
		RelocatableKernel: data[relocatableKernelOffset],
		MinAlignment:      data[minAlignmentOffset],
		XLoadFlags:        binary.LittleEndian.Uint16(data[xloadflagsOffset:]),
		CmdlineSize:       binary.LittleEndian.Uint32(data[cmdlineSizeOffset:]),
		PrefAddress:       binary.LittleEndian.Uint64(data[prefAddressOffset:]),
		InitSize:          binary.LittleEndian.Uint32(data[initSizeOffset:]),
	}
	if hdr.SetupSectors == 0 {
		hdr.SetupSectors = 4
	}
	if hdr.XLoadFlags&0x1 == 0 {
		return errors.New("kernel does not advertise 64-bit entry")
	}
	k.Header = hdr

	payloadOffset := 512 * (1 + int(hdr.SetupSectors))
	if payloadOffset > len(data) {
		return fmt.Errorf("payload offset %d exceeds image size %d", payloadOffset, len(data))
	}
	k.PayloadOffset = payloadOffset
	return nil
}

func (k *KernelImage) Payload() []byte {
	if k == nil || k.PayloadOffset > len(k.Data) {
		return nil
	}
	return k.Data[k.PayloadOffset:]
}

func (k *KernelImage) DefaultLoadAddress() uint64 {
	if k.Header.PrefAddress != 0 {
		return k.Header.PrefAddress
	}
	if k.Header.LoadFlags&0x1 != 0 {
		return 0x00100000
	}
	return 0x00010000
}

func (k *KernelImage) EntryPoint(loadAddr uint64) uint64 {
	return loadAddr + 0x200
}
