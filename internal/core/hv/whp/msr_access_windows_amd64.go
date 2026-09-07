//go:build windows && amd64

package whp

import "time"

var processStart = time.Now()

func handleMSRAccess(vm *VM, vpIndex uint32, exit Exit, raw *runVPExitContext) error {
	msr := raw.msrAccess()
	nextRIP := exit.RIP + uint64(raw.instructionLength())
	if raw.instructionLength() == 0 {
		nextRIP = exit.RIP + 2
	}
	if msr.AccessInfo.isWrite() {
		return vm.SetVCPURIP(vpIndex, nextRIP)
	}
	value := readMSR(msr.MSRNumber)
	return vm.SetVCPURegisters(vpIndex, map[registerName]uint64{
		registerRax: value & 0xffffffff,
		registerRdx: value >> 32,
		registerRip: nextRIP,
	})
}

func readMSR(msr uint32) uint64 {
	switch msr {
	case 0x10:
		return uint64(time.Since(processStart).Nanoseconds())
	case 0xce:
		return 0
	case 0xe7, 0xe8:
		return 0
	case 0x1a0:
		return 0
	default:
		return 0
	}
}
