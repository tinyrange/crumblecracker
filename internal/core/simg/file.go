package simg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const (
	sifHeaderSize     = 4096
	sifDescriptorSize = 585
	squashFSMagic     = "hsqs"
)

type SIFHeader struct {
	Magic             string
	VersionMajor      string
	VersionMinor      string
	Arch              string
	CreatedAt         int64
	ModifiedAt        int64
	DescriptorsFree   uint64
	DescriptorsTotal  uint64
	DescriptorsOffset uint64
	DescriptorsSize   uint64
	DataOffset        uint64
	DataSize          uint64
}

type SIFDescriptor struct {
	Index           int
	DataTypeRaw     uint32
	Offset          uint64
	Size            uint64
	SizeWithPadding uint64
	Name            string
}

type File struct {
	Path               string
	F                  *os.File
	Size               uint64
	SIF                *SIFHeader
	Descriptors        []SIFDescriptor
	SquashFSOffset     int64
	SquashFSDescriptor *SIFDescriptor
}

func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	img := &File{Path: path, F: f, Size: uint64(st.Size())}
	head, err := readAtMost(f, 0, sifHeaderSize)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if len(head) >= 42 && string(head[32:42]) == "SIF_MAGIC\x00" {
		hdr, err := parseSIFHeader(head)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		img.SIF = &hdr
		desc, err := parseSIFDescriptors(f, img.Size, hdr)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		img.Descriptors = desc
	}
	off, desc, err := findSquashFSOffset(img)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	img.SquashFSOffset = off
	img.SquashFSDescriptor = desc
	return img, nil
}

func (f *File) Close() error {
	if f == nil || f.F == nil {
		return nil
	}
	return f.F.Close()
}

func parseSIFHeader(b []byte) (SIFHeader, error) {
	if len(b) < 128 {
		return SIFHeader{}, errors.New("short SIF header")
	}
	h := SIFHeader{
		Magic:             trimCString(b[32:42]),
		VersionMajor:      trimCString(b[42:45]),
		VersionMinor:      "",
		Arch:              decodeSIFArch(b[45:48]),
		CreatedAt:         int64(binary.LittleEndian.Uint64(b[64:72])),
		ModifiedAt:        int64(binary.LittleEndian.Uint64(b[72:80])),
		DescriptorsFree:   binary.LittleEndian.Uint64(b[80:88]),
		DescriptorsTotal:  binary.LittleEndian.Uint64(b[88:96]),
		DescriptorsOffset: binary.LittleEndian.Uint64(b[96:104]),
		DescriptorsSize:   binary.LittleEndian.Uint64(b[104:112]),
		DataOffset:        binary.LittleEndian.Uint64(b[112:120]),
		DataSize:          binary.LittleEndian.Uint64(b[120:128]),
	}
	if h.Magic != "SIF_MAGIC" {
		return SIFHeader{}, fmt.Errorf("invalid SIF magic %q", h.Magic)
	}
	return h, nil
}

func parseSIFDescriptors(f *os.File, fileSize uint64, h SIFHeader) ([]SIFDescriptor, error) {
	if h.DescriptorsTotal == 0 {
		return nil, nil
	}
	maxByTable := int(h.DescriptorsTotal)
	bytesNeeded := uint64(maxByTable) * sifDescriptorSize
	if h.DescriptorsOffset+bytesNeeded > fileSize {
		maxByTable = int((fileSize - h.DescriptorsOffset) / sifDescriptorSize)
		if maxByTable < 0 {
			maxByTable = 0
		}
	}
	if maxByTable == 0 {
		return nil, nil
	}
	table, err := readAtMost(f, int64(h.DescriptorsOffset), maxByTable*sifDescriptorSize)
	if err != nil {
		return nil, err
	}
	out := make([]SIFDescriptor, 0, maxByTable)
	for i := 0; i < maxByTable; i++ {
		raw := table[i*sifDescriptorSize : (i+1)*sifDescriptorSize]
		d := inferDescriptor(i+1, raw, fileSize, h.DataOffset)
		if d != nil {
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out, nil
}

func inferDescriptor(idx int, raw []byte, fileSize, dataOffset uint64) *SIFDescriptor {
	if len(raw) < sifDescriptorSize {
		return nil
	}
	dtype := binary.LittleEndian.Uint32(raw[0:4])
	if dtype == 0 {
		return nil
	}
	bestPos := -1
	var bestOff, bestSize, bestPad uint64
	for p := 8; p+24 <= len(raw); p++ {
		off := binary.LittleEndian.Uint64(raw[p : p+8])
		sz := binary.LittleEndian.Uint64(raw[p+8 : p+16])
		pad := binary.LittleEndian.Uint64(raw[p+16 : p+24])
		if off == 0 || sz == 0 || off+sz > fileSize || pad < sz || off < dataOffset {
			continue
		}
		if bestPos == -1 || off < bestOff {
			bestPos = p
			bestOff = off
			bestSize = sz
			bestPad = pad
		}
	}
	if bestPos == -1 {
		return nil
	}
	return &SIFDescriptor{
		Index:           idx,
		DataTypeRaw:     dtype,
		Offset:          bestOff,
		Size:            bestSize,
		SizeWithPadding: bestPad,
		Name:            guessName(raw[64:220]),
	}
}

func findSquashFSOffset(img *File) (int64, *SIFDescriptor, error) {
	if img.SIF != nil {
		for i := range img.Descriptors {
			d := &img.Descriptors[i]
			ok, err := hasMagicAt(img.F, int64(d.Offset), squashFSMagic)
			if err != nil {
				return 0, nil, err
			}
			if ok {
				return int64(d.Offset), d, nil
			}
		}
		start := int64(img.SIF.DataOffset)
		off, err := scanForMagic(img.F, start, squashFSMagic)
		if err == nil {
			return off, nil, nil
		}
	}
	if ok, err := hasMagicAt(img.F, 0, squashFSMagic); err == nil && ok {
		return 0, nil, nil
	}
	off, err := scanForMagic(img.F, 0, squashFSMagic)
	if err != nil {
		return 0, nil, errors.New("could not locate embedded squashfs")
	}
	return off, nil, nil
}

func readAtMost(f *os.File, off int64, n int) ([]byte, error) {
	if n < 0 {
		return nil, errors.New("negative read length")
	}
	buf := make([]byte, n)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func hasMagicAt(f *os.File, off int64, magic string) (bool, error) {
	b, err := readAtMost(f, off, len(magic))
	if err != nil {
		return false, err
	}
	return string(b) == magic, nil
}

func scanForMagic(f *os.File, start int64, magic string) (int64, error) {
	const chunkSize = 1 << 20
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, err
	}
	pat := []byte(magic)
	buf := make([]byte, chunkSize+len(pat)-1)
	var offset int64 = start
	var carry int
	for {
		n, err := f.Read(buf[carry : carry+chunkSize])
		if n > 0 {
			search := buf[:carry+n]
			if idx := strings.Index(string(search), magic); idx >= 0 {
				return offset - int64(carry) + int64(idx), nil
			}
			if len(pat)-1 < len(search) {
				carry = copy(buf[:len(pat)-1], search[len(search)-(len(pat)-1):])
			} else {
				carry = copy(buf[:], search)
			}
			offset += int64(n)
		}
		if err == io.EOF {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
	}
}

func trimCString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

func guessName(b []byte) string {
	s := trimCString(b)
	if s == "" || strings.ContainsRune(s, '\uFFFD') {
		return ""
	}
	return s
}

func decodeSIFArch(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	switch string(b[:2]) {
	case "01":
		return "386"
	case "02":
		return "amd64"
	case "03":
		return "arm"
	case "04":
		return "arm64"
	case "05":
		return "ppc64"
	case "06":
		return "ppc64le"
	case "07":
		return "mips"
	case "08":
		return "mipsle"
	case "09":
		return "mips64"
	case "10":
		return "mips64le"
	case "11":
		return "s390x"
	case "12":
		return "riscv64"
	default:
		return ""
	}
}
