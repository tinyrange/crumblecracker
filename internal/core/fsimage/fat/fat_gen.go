package fat

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/fsimage/vm"
)

const (
	SECTOR_SIZE           = 512
	FAT12_FREE     uint32 = uint32(0)
	FAT12_EOC      uint32 = uint32(4088)
	FAT12_BAD      uint32 = uint32(4087)
	FAT16_FREE     uint32 = uint32(0)
	FAT16_EOC      uint32 = uint32(65528)
	FAT16_BAD      uint32 = uint32(65527)
	FAT32_FREE     uint32 = uint32(0)
	FAT32_EOC      uint32 = uint32(268435448)
	FAT32_BAD      uint32 = uint32(268435447)
	ATTR_READ_ONLY        = 1
	ATTR_HIDDEN           = 2
	ATTR_SYSTEM           = 4
	ATTR_VOLUME_ID        = 8
	ATTR_DIRECTORY        = 16
	ATTR_ARCHIVE          = 32
	ATTR_LFN              = 15
)

type FATFilesystem [512]byte

func (s *FATFilesystem) TagGenerated() {}

func (s *FATFilesystem) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= s.Size() {
		return 0, io.EOF
	}
	n = copy(p, (*s)[off:])
	return n, nil
}
func (s *FATFilesystem) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if off >= s.Size() {
		return 0, io.ErrUnexpectedEOF
	}
	n = copy((*s)[off:], p)
	return n, nil
}
func (s *FATFilesystem) Size() int64 {
	return 512
}
func (s *FATFilesystem) BootJump() [3]byte {
	var result [3]byte
	copy(result[:], (*s)[0:3])
	return result
}
func (s *FATFilesystem) SetBootJump(value [3]byte) {
	copy((*s)[0:3], value[:])
}
func (s *FATFilesystem) OemIdentifier() [8]byte {
	var result [8]byte
	copy(result[:], (*s)[3:11])
	return result
}
func (s *FATFilesystem) SetOemIdentifier(value [8]byte) {
	copy((*s)[3:11], value[:])
}
func (s *FATFilesystem) BytesPerSector() uint16 {
	return binary.LittleEndian.Uint16((*s)[11:])
}
func (s *FATFilesystem) SetBytesPerSector(value uint16) {
	binary.LittleEndian.PutUint16((*s)[11:], value)
}
func (s *FATFilesystem) SectorsPerCluster() uint8 {
	return uint8((*s)[13])
}
func (s *FATFilesystem) SetSectorsPerCluster(value uint8) {
	(*s)[13] = byte(value)
}
func (s *FATFilesystem) ReservedSectors() uint16 {
	return binary.LittleEndian.Uint16((*s)[14:])
}
func (s *FATFilesystem) SetReservedSectors(value uint16) {
	binary.LittleEndian.PutUint16((*s)[14:], value)
}
func (s *FATFilesystem) FileAllocationTables() uint8 {
	return uint8((*s)[16])
}
func (s *FATFilesystem) SetFileAllocationTables(value uint8) {
	(*s)[16] = byte(value)
}
func (s *FATFilesystem) RootDirectoryEntries() uint16 {
	return binary.LittleEndian.Uint16((*s)[17:])
}
func (s *FATFilesystem) SetRootDirectoryEntries(value uint16) {
	binary.LittleEndian.PutUint16((*s)[17:], value)
}
func (s *FATFilesystem) TotalSectors() uint16 {
	return binary.LittleEndian.Uint16((*s)[19:])
}
func (s *FATFilesystem) SetTotalSectors(value uint16) {
	binary.LittleEndian.PutUint16((*s)[19:], value)
}
func (s *FATFilesystem) MediaDescriptorType() uint8 {
	return uint8((*s)[21])
}
func (s *FATFilesystem) SetMediaDescriptorType(value uint8) {
	(*s)[21] = byte(value)
}
func (s *FATFilesystem) SectorsPerFat16() uint16 {
	return binary.LittleEndian.Uint16((*s)[22:])
}
func (s *FATFilesystem) SetSectorsPerFat16(value uint16) {
	binary.LittleEndian.PutUint16((*s)[22:], value)
}
func (s *FATFilesystem) SectorsPerTrack() uint16 {
	return binary.LittleEndian.Uint16((*s)[24:])
}
func (s *FATFilesystem) SetSectorsPerTrack(value uint16) {
	binary.LittleEndian.PutUint16((*s)[24:], value)
}
func (s *FATFilesystem) SectorsPerHead() uint16 {
	return binary.LittleEndian.Uint16((*s)[26:])
}
func (s *FATFilesystem) SetSectorsPerHead(value uint16) {
	binary.LittleEndian.PutUint16((*s)[26:], value)
}
func (s *FATFilesystem) HiddenSectors() uint32 {
	return binary.LittleEndian.Uint32((*s)[28:])
}
func (s *FATFilesystem) SetHiddenSectors(value uint32) {
	binary.LittleEndian.PutUint32((*s)[28:], value)
}
func (s *FATFilesystem) LargeSectorCount() uint32 {
	return binary.LittleEndian.Uint32((*s)[32:])
}
func (s *FATFilesystem) SetLargeSectorCount(value uint32) {
	binary.LittleEndian.PutUint32((*s)[32:], value)
}
func (s *FATFilesystem) TotalSectorsExtended() uint32 {
	return func() uint32 {
		if s.TotalSectors() != 0 {
			return uint32(s.TotalSectors())
		} else {
			return s.LargeSectorCount()
		}
	}()
}
func (s *FATFilesystem) RootDirectorySectors() uint32 {
	return uint32(uint32((uint32((uint32(s.RootDirectoryEntries()) * uint32(32))) + uint32((uint32(s.BytesPerSector()) - uint32(1))))) / uint32(s.BytesPerSector()))
}
func (s *FATFilesystem) FatSizeInSectors() uint32 {
	return uint32(uint32(s.FileAllocationTables()) * uint32(s.SectorsPerFat()))
}
func (s *FATFilesystem) DataSectors() uint32 {
	return uint32(uint32(s.TotalSectorsExtended()) - uint32((uint32(uint32(s.ReservedSectors())+uint32(s.FatSizeInSectors())) + uint32(s.RootDirectorySectors()))))
}
func (s *FATFilesystem) TotalDataClusters() uint32 {
	return uint32(uint32(s.DataSectors()) / uint32(s.SectorsPerCluster()))
}
func (s *FATFilesystem) RootDirectoryOffset() uint32 {
	return uint32(uint32(int64(s.ReservedSectors())*int64(s.BytesPerSector())) + uint32(uint32(uint32(s.FileAllocationTables())*uint32(s.SectorsPerFat()))*uint32(s.BytesPerSector())))
}
func (s *FATFilesystem) IsFAT32() bool {
	return (s.SectorsPerFat16() == 0)
}
func (s *FATFilesystem) BackupBootSector() uint16 {
	if s.IsFAT32() {
		return binary.LittleEndian.Uint16((*s)[50:])
	}
	return 0
}
func (s *FATFilesystem) SetBackupBootSector(value uint16) {
	if s.IsFAT32() {
		binary.LittleEndian.PutUint16((*s)[50:], value)
	}
}
func (s *FATFilesystem) BootCode() []byte {
	if s.IsFAT32() {
		return (*s)[90:510]
	} else if !s.IsFAT32() {
		return (*s)[62:510]
	}
	return nil
}
func (s *FATFilesystem) SetBootCode(value [420]byte) {
	if s.IsFAT32() {
		copy((*s)[90:], value[0:])
	}
	if !s.IsFAT32() {
		copy((*s)[62:], value[0:])
	}
}
func (s *FATFilesystem) BootSignature() uint16 {
	if s.IsFAT32() {
		return binary.LittleEndian.Uint16((*s)[510:])
	} else if !s.IsFAT32() {
		return binary.LittleEndian.Uint16((*s)[510:])
	}
	return 0
}
func (s *FATFilesystem) SetBootSignature(value uint16) {
	if s.IsFAT32() {
		binary.LittleEndian.PutUint16((*s)[510:], value)
	}
	if !s.IsFAT32() {
		binary.LittleEndian.PutUint16((*s)[510:], value)
	}
}
func (s *FATFilesystem) DriveNumber() uint8 {
	if s.IsFAT32() {
		return uint8((*s)[64])
	} else if !s.IsFAT32() {
		return uint8((*s)[36])
	}
	return 0
}
func (s *FATFilesystem) SetDriveNumber(value uint8) {
	if s.IsFAT32() {
		(*s)[64] = byte(value)
	}
	if !s.IsFAT32() {
		(*s)[36] = byte(value)
	}
}
func (s *FATFilesystem) EndOfChian() uint32 {
	if s.IsFAT32() {
		return FAT32_EOC
	} else if (!s.IsFAT32()) && (s.TotalDataClusters() < 4085) {
		return FAT12_EOC
	} else if (!s.IsFAT32()) && (!(s.TotalDataClusters() < 4085)) {
		return FAT16_EOC
	}
	return 0
}
func (s *FATFilesystem) FatType() string {
	if s.IsFAT32() {
		return "FAT32"
	} else if (!s.IsFAT32()) && (s.TotalDataClusters() < 4085) {
		return "FAT12"
	} else if (!s.IsFAT32()) && (!(s.TotalDataClusters() < 4085)) {
		return "FAT16"
	}
	return ""
}
func (s *FATFilesystem) FatVersion() uint16 {
	if s.IsFAT32() {
		return binary.LittleEndian.Uint16((*s)[42:])
	}
	return 0
}
func (s *FATFilesystem) SetFatVersion(value uint16) {
	if s.IsFAT32() {
		binary.LittleEndian.PutUint16((*s)[42:], value)
	}
}
func (s *FATFilesystem) Flags() uint16 {
	if s.IsFAT32() {
		return binary.LittleEndian.Uint16((*s)[40:])
	}
	return 0
}
func (s *FATFilesystem) SetFlags(value uint16) {
	if s.IsFAT32() {
		binary.LittleEndian.PutUint16((*s)[40:], value)
	}
}
func (s *FATFilesystem) FsInfoSector() uint16 {
	if s.IsFAT32() {
		return binary.LittleEndian.Uint16((*s)[48:])
	}
	return 0
}
func (s *FATFilesystem) SetFsInfoSector(value uint16) {
	if s.IsFAT32() {
		binary.LittleEndian.PutUint16((*s)[48:], value)
	}
}
func (s *FATFilesystem) NtFlags() uint8 {
	if s.IsFAT32() {
		return uint8((*s)[65])
	} else if !s.IsFAT32() {
		return uint8((*s)[37])
	}
	return 0
}
func (s *FATFilesystem) SetNtFlags(value uint8) {
	if s.IsFAT32() {
		(*s)[65] = byte(value)
	}
	if !s.IsFAT32() {
		(*s)[37] = byte(value)
	}
}
func (s *FATFilesystem) RootDirectoryCluster() uint32 {
	if s.IsFAT32() {
		return binary.LittleEndian.Uint32((*s)[44:])
	}
	return 0
}
func (s *FATFilesystem) SetRootDirectoryCluster(value uint32) {
	if s.IsFAT32() {
		binary.LittleEndian.PutUint32((*s)[44:], value)
	}
}
func (s *FATFilesystem) SectorsPerFat() uint32 {
	if s.IsFAT32() {
		return s.SectorsPerFat32()
	} else if !s.IsFAT32() {
		return uint32(s.SectorsPerFat16())
	}
	return 0
}
func (s *FATFilesystem) SectorsPerFat32() uint32 {
	if s.IsFAT32() {
		return binary.LittleEndian.Uint32((*s)[36:])
	}
	return 0
}
func (s *FATFilesystem) SetSectorsPerFat32(value uint32) {
	if s.IsFAT32() {
		binary.LittleEndian.PutUint32((*s)[36:], value)
	}
}
func (s *FATFilesystem) Signature() uint8 {
	if s.IsFAT32() {
		return uint8((*s)[66])
	} else if !s.IsFAT32() {
		return uint8((*s)[38])
	}
	return 0
}
func (s *FATFilesystem) SetSignature(value uint8) {
	if s.IsFAT32() {
		(*s)[66] = byte(value)
	}
	if !s.IsFAT32() {
		(*s)[38] = byte(value)
	}
}
func (s *FATFilesystem) SystemIdentifier() string {
	if s.IsFAT32() {
		return strings.TrimRight(string((*s)[82:90]), "\x00 ")
	} else if !s.IsFAT32() {
		return strings.TrimRight(string((*s)[54:62]), "\x00 ")
	}
	return ""
}
func (s *FATFilesystem) SetSystemIdentifier(value string) {
	if s.IsFAT32() {
		copy((*s)[82:], []byte(value)[:])
	}
	if !s.IsFAT32() {
		copy((*s)[54:], []byte(value)[:])
	}
}
func (s *FATFilesystem) VolumeId() uint32 {
	if s.IsFAT32() {
		return binary.LittleEndian.Uint32((*s)[67:])
	} else if !s.IsFAT32() {
		return binary.LittleEndian.Uint32((*s)[39:])
	}
	return 0
}
func (s *FATFilesystem) SetVolumeId(value uint32) {
	if s.IsFAT32() {
		binary.LittleEndian.PutUint32((*s)[67:], value)
	}
	if !s.IsFAT32() {
		binary.LittleEndian.PutUint32((*s)[39:], value)
	}
}
func (s *FATFilesystem) VolumeLabel() string {
	if s.IsFAT32() {
		return strings.TrimRight(string((*s)[71:82]), "\x00 ")
	} else if !s.IsFAT32() {
		return strings.TrimRight(string((*s)[43:54]), "\x00 ")
	}
	return ""
}
func (s *FATFilesystem) SetVolumeLabel(value string) {
	if s.IsFAT32() {
		copy((*s)[71:], []byte(value)[:])
	}
	if !s.IsFAT32() {
		copy((*s)[43:], []byte(value)[:])
	}
}
func (s *FATFilesystem) Validate() error {
	if !((s.Signature() == 40) || (s.Signature() == 41)) {
		return fmt.Errorf("validation failed: check condition not met")
	}
	if !(s.BootSignature() == 43605) {
		return fmt.Errorf("validation failed: check condition not met")
	}
	if !(s.BootJump() == [3]byte{235, 60, 144}) {
		return fmt.Errorf("validation failed: check condition not met")
	}
	return nil
}
func NewFATFilesystem() *FATFilesystem {
	var s FATFilesystem
	return &s
}

type FSInfo [512]byte

func (s *FSInfo) TagGenerated() {}

func (s *FSInfo) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= s.Size() {
		return 0, io.EOF
	}
	n = copy(p, (*s)[off:])
	return n, nil
}
func (s *FSInfo) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if off >= s.Size() {
		return 0, io.ErrUnexpectedEOF
	}
	n = copy((*s)[off:], p)
	return n, nil
}
func (s *FSInfo) Size() int64 {
	return 512
}
func (s *FSInfo) LeadSignature() uint32 {
	return binary.LittleEndian.Uint32((*s)[0:])
}
func (s *FSInfo) SetLeadSignature(value uint32) {
	binary.LittleEndian.PutUint32((*s)[0:], value)
}
func (s *FSInfo) Signature() uint32 {
	return binary.LittleEndian.Uint32((*s)[484:])
}
func (s *FSInfo) SetSignature(value uint32) {
	binary.LittleEndian.PutUint32((*s)[484:], value)
}
func (s *FSInfo) LastFreeClusterCount() uint32 {
	return binary.LittleEndian.Uint32((*s)[488:])
}
func (s *FSInfo) SetLastFreeClusterCount(value uint32) {
	binary.LittleEndian.PutUint32((*s)[488:], value)
}
func (s *FSInfo) AvailableClusterStart() uint32 {
	return binary.LittleEndian.Uint32((*s)[492:])
}
func (s *FSInfo) SetAvailableClusterStart(value uint32) {
	binary.LittleEndian.PutUint32((*s)[492:], value)
}
func (s *FSInfo) TrailSignature() uint32 {
	return binary.LittleEndian.Uint32((*s)[508:])
}
func (s *FSInfo) SetTrailSignature(value uint32) {
	binary.LittleEndian.PutUint32((*s)[508:], value)
}
func (s *FSInfo) Validate() error {
	if s.LeadSignature() != 1096897106 {
		return fmt.Errorf("field leadSignature has invalid value: got %v, expected %v", s.LeadSignature(), 1096897106)
	}
	if s.Signature() != 1631679090 {
		return fmt.Errorf("field signature has invalid value: got %v, expected %v", s.Signature(), 1631679090)
	}
	if s.TrailSignature() != 2857697280 {
		return fmt.Errorf("field trailSignature has invalid value: got %v, expected %v", s.TrailSignature(), 2857697280)
	}
	return nil
}
func NewFSInfo() *FSInfo {
	var s FSInfo
	s.SetLeadSignature(1096897106)
	s.SetSignature(1631679090)
	s.SetTrailSignature(2857697280)
	return &s
}

type DirectoryEntry [32]byte

func (s *DirectoryEntry) TagGenerated() {}

func (s *DirectoryEntry) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= s.Size() {
		return 0, io.EOF
	}
	n = copy(p, (*s)[off:])
	return n, nil
}
func (s *DirectoryEntry) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if off >= s.Size() {
		return 0, io.ErrUnexpectedEOF
	}
	n = copy((*s)[off:], p)
	return n, nil
}
func (s *DirectoryEntry) Size() int64 {
	return 32
}
func (s *DirectoryEntry) Name() [8]byte {
	var result [8]byte
	copy(result[:], (*s)[0:8])
	return result
}
func (s *DirectoryEntry) SetName(value [8]byte) {
	copy((*s)[0:8], value[:])
}
func (s *DirectoryEntry) Extension() [3]byte {
	var result [3]byte
	copy(result[:], (*s)[8:11])
	return result
}
func (s *DirectoryEntry) SetExtension(value [3]byte) {
	copy((*s)[8:11], value[:])
}
func (s *DirectoryEntry) Attributes() uint8 {
	return uint8((*s)[11])
}
func (s *DirectoryEntry) SetAttributes(value uint8) {
	(*s)[11] = byte(value)
}
func (s *DirectoryEntry) NtReserved() uint8 {
	return uint8((*s)[12])
}
func (s *DirectoryEntry) SetNtReserved(value uint8) {
	(*s)[12] = byte(value)
}
func (s *DirectoryEntry) CreationTimeHundredths() uint8 {
	return uint8((*s)[13])
}
func (s *DirectoryEntry) SetCreationTimeHundredths(value uint8) {
	(*s)[13] = byte(value)
}
func (s *DirectoryEntry) CreationTime() uint16 {
	return binary.LittleEndian.Uint16((*s)[14:])
}
func (s *DirectoryEntry) SetCreationTime(value uint16) {
	binary.LittleEndian.PutUint16((*s)[14:], value)
}
func (s *DirectoryEntry) CreationDate() uint16 {
	return binary.LittleEndian.Uint16((*s)[16:])
}
func (s *DirectoryEntry) SetCreationDate(value uint16) {
	binary.LittleEndian.PutUint16((*s)[16:], value)
}
func (s *DirectoryEntry) LastAccessDate() uint16 {
	return binary.LittleEndian.Uint16((*s)[18:])
}
func (s *DirectoryEntry) SetLastAccessDate(value uint16) {
	binary.LittleEndian.PutUint16((*s)[18:], value)
}
func (s *DirectoryEntry) FirstClusterHigh() uint16 {
	return binary.LittleEndian.Uint16((*s)[20:])
}
func (s *DirectoryEntry) SetFirstClusterHigh(value uint16) {
	binary.LittleEndian.PutUint16((*s)[20:], value)
}
func (s *DirectoryEntry) LastModificationTime() uint16 {
	return binary.LittleEndian.Uint16((*s)[22:])
}
func (s *DirectoryEntry) SetLastModificationTime(value uint16) {
	binary.LittleEndian.PutUint16((*s)[22:], value)
}
func (s *DirectoryEntry) LastModificationDate() uint16 {
	return binary.LittleEndian.Uint16((*s)[24:])
}
func (s *DirectoryEntry) SetLastModificationDate(value uint16) {
	binary.LittleEndian.PutUint16((*s)[24:], value)
}
func (s *DirectoryEntry) FirstClusterLow() uint16 {
	return binary.LittleEndian.Uint16((*s)[26:])
}
func (s *DirectoryEntry) SetFirstClusterLow(value uint16) {
	binary.LittleEndian.PutUint16((*s)[26:], value)
}
func (s *DirectoryEntry) FileSize() uint32 {
	return binary.LittleEndian.Uint32((*s)[28:])
}
func (s *DirectoryEntry) SetFileSize(value uint32) {
	binary.LittleEndian.PutUint32((*s)[28:], value)
}
func (s *DirectoryEntry) FirstCluster() uint32 {
	return uint32(uint64(s.FirstClusterLow()) | uint64(s.FirstClusterHigh())<<16)
}
func NewDirectoryEntry() *DirectoryEntry {
	var s DirectoryEntry
	return &s
}

type LongFileNameEntry [32]byte

func (s *LongFileNameEntry) TagGenerated() {}

func (s *LongFileNameEntry) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= s.Size() {
		return 0, io.EOF
	}
	n = copy(p, (*s)[off:])
	return n, nil
}
func (s *LongFileNameEntry) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if off >= s.Size() {
		return 0, io.ErrUnexpectedEOF
	}
	n = copy((*s)[off:], p)
	return n, nil
}
func (s *LongFileNameEntry) Size() int64 {
	return 32
}
func (s *LongFileNameEntry) Order() uint8 {
	return uint8((*s)[0])
}
func (s *LongFileNameEntry) SetOrder(value uint8) {
	(*s)[0] = byte(value)
}
func (s *LongFileNameEntry) Name1() [10]byte {
	var result [10]byte
	copy(result[:], (*s)[1:11])
	return result
}
func (s *LongFileNameEntry) SetName1(value [10]byte) {
	copy((*s)[1:11], value[:])
}
func (s *LongFileNameEntry) Attributes() uint8 {
	return uint8((*s)[11])
}
func (s *LongFileNameEntry) SetAttributes(value uint8) {
	(*s)[11] = byte(value)
}
func (s *LongFileNameEntry) EntryType() uint8 {
	return uint8((*s)[12])
}
func (s *LongFileNameEntry) SetEntryType(value uint8) {
	(*s)[12] = byte(value)
}
func (s *LongFileNameEntry) Checksum() uint8 {
	return uint8((*s)[13])
}
func (s *LongFileNameEntry) SetChecksum(value uint8) {
	(*s)[13] = byte(value)
}
func (s *LongFileNameEntry) Name2() [12]byte {
	var result [12]byte
	copy(result[:], (*s)[14:26])
	return result
}
func (s *LongFileNameEntry) SetName2(value [12]byte) {
	copy((*s)[14:26], value[:])
}
func (s *LongFileNameEntry) FirstCluster() uint16 {
	return binary.LittleEndian.Uint16((*s)[26:])
}
func (s *LongFileNameEntry) SetFirstCluster(value uint16) {
	binary.LittleEndian.PutUint16((*s)[26:], value)
}
func (s *LongFileNameEntry) Name3() [4]byte {
	var result [4]byte
	copy(result[:], (*s)[28:32])
	return result
}
func (s *LongFileNameEntry) SetName3(value [4]byte) {
	copy((*s)[28:32], value[:])
}
func (s *LongFileNameEntry) Validate() error {
	if s.Attributes() != ATTR_LFN {
		return fmt.Errorf("field attributes has invalid value: got %v, expected %v", s.Attributes(), ATTR_LFN)
	}
	if s.EntryType() != 0 {
		return fmt.Errorf("field entryType has invalid value: got %v, expected %v", s.EntryType(), 0)
	}
	if s.FirstCluster() != 0 {
		return fmt.Errorf("field firstCluster has invalid value: got %v, expected %v", s.FirstCluster(), 0)
	}
	return nil
}
func NewLongFileNameEntry() *LongFileNameEntry {
	var s LongFileNameEntry
	s.SetAttributes(ATTR_LFN)
	s.SetEntryType(0)
	s.SetFirstCluster(0)
	return &s
}

type FATTime [2]byte

func (s *FATTime) TagGenerated() {}

func (s *FATTime) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= s.Size() {
		return 0, io.EOF
	}
	n = copy(p, (*s)[off:])
	return n, nil
}
func (s *FATTime) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if off >= s.Size() {
		return 0, io.ErrUnexpectedEOF
	}
	n = copy((*s)[off:], p)
	return n, nil
}
func (s *FATTime) Size() int64 {
	return 2
}
func (s *FATTime) Seconds() uint16 {
	return uint16((binary.LittleEndian.Uint16((*s)[0:]) >> 0) & 0x1F)
}
func (s *FATTime) SetSeconds(value uint16) {
	current := binary.LittleEndian.Uint16((*s)[0:])
	newValue := (current & 0xFFE0) | ((value & 0x1F) << 0)
	binary.LittleEndian.PutUint16((*s)[0:], newValue)
}
func (s *FATTime) Minutes() uint16 {
	return uint16((binary.LittleEndian.Uint16((*s)[0:]) >> 5) & 0x3F)
}
func (s *FATTime) SetMinutes(value uint16) {
	current := binary.LittleEndian.Uint16((*s)[0:])
	newValue := (current & 0xF81F) | ((value & 0x3F) << 5)
	binary.LittleEndian.PutUint16((*s)[0:], newValue)
}
func (s *FATTime) Hours() uint16 {
	return uint16((binary.LittleEndian.Uint16((*s)[0:]) >> 11) & 0x1F)
}
func (s *FATTime) SetHours(value uint16) {
	current := binary.LittleEndian.Uint16((*s)[0:])
	newValue := (current & 0x7FF) | ((value & 0x1F) << 11)
	binary.LittleEndian.PutUint16((*s)[0:], newValue)
}
func NewFATTime() *FATTime {
	var s FATTime
	return &s
}

type FATDate [2]byte

func (s *FATDate) TagGenerated() {}

func (s *FATDate) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= s.Size() {
		return 0, io.EOF
	}
	n = copy(p, (*s)[off:])
	return n, nil
}
func (s *FATDate) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if off >= s.Size() {
		return 0, io.ErrUnexpectedEOF
	}
	n = copy((*s)[off:], p)
	return n, nil
}
func (s *FATDate) Size() int64 {
	return 2
}
func (s *FATDate) Day() uint16 {
	return uint16((binary.LittleEndian.Uint16((*s)[0:]) >> 0) & 0x1F)
}
func (s *FATDate) SetDay(value uint16) {
	current := binary.LittleEndian.Uint16((*s)[0:])
	newValue := (current & 0xFFE0) | ((value & 0x1F) << 0)
	binary.LittleEndian.PutUint16((*s)[0:], newValue)
}
func (s *FATDate) Month() uint16 {
	return uint16((binary.LittleEndian.Uint16((*s)[0:]) >> 5) & 0xF)
}
func (s *FATDate) SetMonth(value uint16) {
	current := binary.LittleEndian.Uint16((*s)[0:])
	newValue := (current & 0xFE1F) | ((value & 0xF) << 5)
	binary.LittleEndian.PutUint16((*s)[0:], newValue)
}
func (s *FATDate) Year() uint16 {
	return uint16((binary.LittleEndian.Uint16((*s)[0:]) >> 9) & 0x7F)
}
func (s *FATDate) SetYear(value uint16) {
	current := binary.LittleEndian.Uint16((*s)[0:])
	newValue := (current & 0x1FF) | ((value & 0x7F) << 9)
	binary.LittleEndian.PutUint16((*s)[0:], newValue)
}
func NewFATDate() *FATDate {
	var s FATDate
	return &s
}

type FATLayout struct {
	storage vm.VirtualStorage
}

func NewFATLayout(storage vm.VirtualStorage) *FATLayout {
	return &FATLayout{storage: storage}
}
func (l *FATLayout) Fs() *FATFilesystem {
	var r FATFilesystem
	if err := l.storage.Reinterpret(&r, 0); err != nil {
		return nil
	}
	return &r
}
func (l *FATLayout) FatTableSize() uint64 {
	fs := l.Fs()
	return uint64(uint32(uint32(uint32(uint32(fs.FileAllocationTables())*uint32(fs.SectorsPerFat()))) * uint32(fs.BytesPerSector())))
}
func (l *FATLayout) FatTables() io.ReaderAt {
	fs := l.Fs()
	return vm.NewTruncatedRegion(vm.NewOffsetRegion(l.storage, int64(uint32(uint32(fs.ReservedSectors())*uint32(fs.BytesPerSector())))), int64(l.FatTableSize()))
}
func (l *FATLayout) FirstFAT() io.ReaderAt {
	fs := l.Fs()
	return vm.NewTruncatedRegion(vm.NewOffsetRegion(l.storage, int64(uint32(uint32(fs.ReservedSectors())*uint32(fs.BytesPerSector())))), int64(uint32(uint32(fs.SectorsPerFat())*uint32(fs.BytesPerSector()))))
}
func (l *FATLayout) DataStart() uint32 {
	fs := l.Fs()
	return uint32(uint32(uint32(uint32(uint32(uint32(uint32(fs.ReservedSectors())*uint32(fs.BytesPerSector())))+uint32(uint32(uint32(uint32(uint32(fs.FileAllocationTables())*uint32(fs.SectorsPerFat())))*uint32(fs.BytesPerSector()))))) + uint32((func() uint32 {
		if fs.FatType() == "FAT12" || fs.FatType() == "FAT16" {
			return uint32(uint32(uint32(fs.RootDirectoryEntries()) * uint32(32)))
		} else {
			return uint32(0)
		}
	}()))))
}
func (l *FATLayout) DataArea() io.ReaderAt {
	return vm.NewOffsetRegion(l.storage, int64(l.DataStart()))
}
func (l *FATLayout) FsInfo() *FSInfo {
	fs := l.Fs()
	var r FSInfo
	if err := l.storage.Reinterpret(&r, int64(uint32(uint32(fs.FsInfoSector())*uint32(fs.BytesPerSector())))); err != nil {
		return nil
	}
	return &r
}
func (l *FATLayout) ClusterSize() uint64 {
	fs := l.Fs()
	return uint64(uint32(uint32(fs.SectorsPerCluster()) * uint32(fs.BytesPerSector())))
}
func (l *FATLayout) Cluster(n uint32) uint64 {
	return uint64(uint32(uint32(l.DataStart()) + uint32(uint32(uint32((uint32(uint32(n)-uint32(2))))*uint32(l.ClusterSize())))))
}
func (l *FATLayout) ClusterData(n uint32) io.ReaderAt {
	return vm.NewTruncatedRegion(vm.NewOffsetRegion(l.storage, int64(l.Cluster(n))), int64(l.ClusterSize()))
}
func (l *FATLayout) FatEntryOffset(n uint32) uint64 {
	fs := l.Fs()
	return uint64(uint32(uint32(uint32(uint32(fs.ReservedSectors())*uint32(fs.BytesPerSector()))) + uint32((func() uint32 {
		if fs.FatType() == "FAT12" {
			return uint32(uint32(uint32((uint32(uint32(n) * uint32(3)))) / uint32(2)))
		} else {
			return uint32(func() uint32 {
				if fs.FatType() == "FAT16" {
					return uint32(uint32(uint32(n) * uint32(2)))
				} else {
					return uint32(func() uint32 {
						if fs.FatType() == "FAT32" {
							return uint32(uint32(uint32(n) * uint32(4)))
						} else {
							return uint32(0)
						}
					}())
				}
			}())
		}
	}()))))
}
func (l *FATLayout) FatEntry(n uint32) uint32 {
	fs := l.Fs()
	if fs.FatType() == "FAT12" {
		return uint32(func() uint32 {
			if uint32(uint32(n)%uint32(2)) == 0 {
				return uint32((l.Fat12Bytes(n) & 0xFFF))
			} else {
				return uint32((l.Fat12Bytes(n) >> 4))
			}
		}())
	}
	if fs.FatType() == "FAT16" {
		var buf [4]byte
		if _, err := l.storage.ReadAt(buf[:], int64(l.FatEntryOffset(n))); err != nil {
			return 0
		}
		return uint32(binary.LittleEndian.Uint32(buf[:]))
	}
	if fs.FatType() == "FAT32" {
		var buf [4]byte
		if _, err := l.storage.ReadAt(buf[:], int64(l.FatEntryOffset(n))); err != nil {
			return 0
		}
		return uint32(binary.LittleEndian.Uint32(buf[:]))
	}
	return uint32(0)
}
func (l *FATLayout) Fat12Bytes(n uint32) uint32 {
	var buf [4]byte
	if _, err := l.storage.ReadAt(buf[:], int64(l.FatEntryOffset(n))); err != nil {
		return uint32(0)
	}
	return binary.LittleEndian.Uint32(buf[:])
}
