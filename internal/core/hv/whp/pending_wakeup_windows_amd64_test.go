//go:build windows && amd64

package whp

import (
	"os"
	"testing"
	"time"
)

// Exercise the actual WHP cancellation boundary without a guest image. A queued
// device completion must wake a CPU that starts running after its initial kick.
func TestQueuedDeviceInterruptWakesLateStartingCPU(t *testing.T) {
	if os.Getenv("CC_TEST_WINDOWS_WHP_WAKEUP") != "1" {
		t.Skip("set CC_TEST_WINDOWS_WHP_WAKEUP=1 on a disposable WHP host")
	}
	vm, err := NewVM(16 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	const entry = 0x100000
	copy(vm.Memory()[entry:], []byte{0xfa, 0xeb, 0xfe}) // cli; jmp .
	if err := vm.SetLongMode(entry, 0x90000, 0x80000, 0x1000); err != nil {
		t.Fatal(err)
	}
	platform := &bootPlatform{vm: vm}
	platform.queuePendingIRQ(bootIOAPICRoute{line: 5, vector: 0x21}, interruptTriggerLevel, true, true)
	vm.kickVCPUIfRunning(0) // The CPU has not entered WHP yet; this kick is lost.
	stopWakeups := startPendingIRQWakeups(vm, platform)
	defer stopWakeups()

	type result struct {
		exit Exit
		err  error
	}
	done := make(chan result, 1)
	go func() {
		exit, err := vm.Run()
		done <- result{exit, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.exit.Reason != runVPExitReasonCanceled {
			t.Fatalf("queued device completion did not wake CPU: %+v, %v", got.exit, got.err)
		}
	case <-time.After(5 * time.Second):
		_ = vm.CancelRun()
		<-done
		t.Fatal("CPU stayed blocked after a queued device completion")
	}
	if platform.pendingIRQCount() != 1 {
		t.Fatal("wakeup consumed the interrupt before the run loop could deliver it")
	}
}
