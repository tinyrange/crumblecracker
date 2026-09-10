package amd64vm

import "github.com/tinyrange/crumblecracker/internal/core/vmruntime"

const (
	RootFSBase = 0xd0004000
	RootFSSize = 0x1000
	RootFSIRQ  = 5

	VsockBase = 0xd0005000
	VsockSize = 0x1000
	VsockIRQ  = 6

	RNGBase = 0xd0006000
	RNGSize = 0x1000
	RNGIRQ  = 7

	NetBase = 0xd0007000
	NetSize = 0x1000
	NetIRQ  = 8

	ShareFSBase = 0xd0008000
	ShareFSIRQ  = 9
	FSStride    = 0x1000

	SnapshotBase = 0xd0009000
	SnapshotSize = 0x1000

	BalloonBase = 0xd000a000
	BalloonSize = 0x1000
	BalloonIRQ  = 10

	GPUBase = 0xd000b000
	GPUSize = 0x1000
	GPUIRQ  = 11

	KeyboardBase = 0xd000c000
	KeyboardSize = 0x1000
	KeyboardIRQ  = 12

	RelativePointerBase = 0xd000e000
	RelativePointerSize = 0x1000
	RelativePointerIRQ  = 14

	PointerBase = 0xd000d000
	PointerSize = 0x1000
	PointerIRQ  = 13

	RootFSTag   = vmruntime.RootFSTag
	EmulatorTag = vmruntime.EmulatorTag
	GuestCID    = vmruntime.GuestCID
	ControlPort = vmruntime.ControlPort
)
