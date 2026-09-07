package shmem

import (
	"errors"
	"testing"
)

type recordingMapper struct {
	gpa uint64
	mem []byte
	err error
}

func (m *recordingMapper) MapSharedMemory(mem []byte, guestPhysAddr uint64) error {
	m.gpa = guestPhysAddr
	m.mem = mem
	return m.err
}

func TestDomainRegionIsSharedBetweenAttachedVMs(t *testing.T) {
	registry := NewRegistry()
	firstAttachment, err := registry.Attach(Config{Domain: "packets", PhysAddr: 0xd1000000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstAttachment.Release() })
	secondAttachment, err := registry.Attach(Config{Domain: "packets", PhysAddr: 0xd2000000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondAttachment.Release() })

	firstMapper := &recordingMapper{}
	first, err := NewDevice(0xd1000000, firstAttachment, firstMapper)
	if err != nil {
		t.Fatal(err)
	}
	secondMapper := &recordingMapper{}
	second, err := NewDevice(0xd2000000, secondAttachment, secondMapper)
	if err != nil {
		t.Fatal(err)
	}

	requestRegion(t, first, 0, 7, PageSize, 0x100000000)
	requestRegion(t, second, 0, 7, PageSize, 0x200000000)

	if firstMapper.gpa != 0x100000000 || secondMapper.gpa != 0x200000000 {
		t.Fatalf("mapped GPAs = %#x and %#x", firstMapper.gpa, secondMapper.gpa)
	}
	firstMapper.mem[123] = 0x5a
	if got := secondMapper.mem[123]; got != 0x5a {
		t.Fatalf("shared byte = %#x, want 0x5a", got)
	}
}

func TestRegionSizeConflictHasStructuredStatus(t *testing.T) {
	registry := NewRegistry()
	firstAttachment, err := registry.Attach(Config{Domain: "packets", PhysAddr: 0xd1000000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstAttachment.Release() })
	secondAttachment, err := registry.Attach(Config{Domain: "packets", PhysAddr: 0xd2000000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondAttachment.Release() })
	first, _ := NewDevice(0xd1000000, firstAttachment, &recordingMapper{})
	second, _ := NewDevice(0xd2000000, secondAttachment, &recordingMapper{})

	requestRegion(t, first, 0, 9, PageSize, 0x100000000)
	requestRegion(t, second, 0, 9, 2*PageSize, 0x200000000)

	status, err := second.Read(second.Base+descriptorBase+4, 4)
	if err != nil {
		t.Fatal(err)
	}
	code, err := second.Read(second.Base+descriptorBase+24, 4)
	if err != nil {
		t.Fatal(err)
	}
	if status != uint64(StatusError) || code != uint64(ErrorSizeConflict) {
		t.Fatalf("descriptor result = status %d error %d", status, code)
	}
}

func TestMappingFailureDoesNotReportSuccess(t *testing.T) {
	registry := NewRegistry()
	attachment, err := registry.Attach(Config{Domain: "packets", PhysAddr: 0xd1000000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = attachment.Release() })
	mapper := &recordingMapper{err: errors.New("overlap")}
	device, _ := NewDevice(0xd1000000, attachment, mapper)

	requestRegion(t, device, 0, 4, PageSize, 0x100000000)
	status, _ := device.Read(device.Base+descriptorBase+4, 4)
	code, _ := device.Read(device.Base+descriptorBase+24, 4)
	if status != uint64(StatusError) || code != uint64(ErrorMapping) {
		t.Fatalf("descriptor result = status %d error %d", status, code)
	}
}

func requestRegion(t *testing.T, device *Device, descriptor int, id uint32, size, gpa uint64) {
	t.Helper()
	base := device.Base + descriptorBase + uint64(descriptor)*descriptorSize
	if err := device.Write(base, 4, uint64(id)); err != nil {
		t.Fatal(err)
	}
	if err := device.Write(base+8, 8, size); err != nil {
		t.Fatal(err)
	}
	if err := device.Write(base+16, 8, gpa); err != nil {
		t.Fatal(err)
	}
	if err := device.Write(base+4, 4, uint64(StatusRequested)); err != nil {
		t.Fatal(err)
	}
}
