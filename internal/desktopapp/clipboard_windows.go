//go:build windows

package desktopapp

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var clipboardUser32 = windows.NewLazySystemDLL("user32.dll")
var clipboardKernel32 = windows.NewLazySystemDLL("kernel32.dll")
var clipboardOpen = clipboardUser32.NewProc("OpenClipboard")
var clipboardClose = clipboardUser32.NewProc("CloseClipboard")
var clipboardEmpty = clipboardUser32.NewProc("EmptyClipboard")
var clipboardGet = clipboardUser32.NewProc("GetClipboardData")
var clipboardSet = clipboardUser32.NewProc("SetClipboardData")
var clipboardAvailable = clipboardUser32.NewProc("IsClipboardFormatAvailable")
var clipboardCreateWindow = clipboardUser32.NewProc("CreateWindowExW")
var clipboardDestroyWindow = clipboardUser32.NewProc("DestroyWindow")
var clipboardAlloc = clipboardKernel32.NewProc("GlobalAlloc")
var clipboardFree = clipboardKernel32.NewProc("GlobalFree")
var clipboardLock = clipboardKernel32.NewProc("GlobalLock")
var clipboardUnlock = clipboardKernel32.NewProc("GlobalUnlock")
var clipboardSize = clipboardKernel32.NewProc("GlobalSize")

const clipboardUnicodeText = 13
const clipboardMaxBytes = 64 << 20

type platformClipboard struct{ owner uintptr }

func clipboardError(operation string, err error) error {
	if err == nil || err == syscall.Errno(0) {
		return fmt.Errorf("%s failed", operation)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
func newHostClipboard() (hostClipboard, error) {
	// A message-only window gives EmptyClipboard/SetClipboardData a real owner
	// without creating another visible window or relying on keyboard focus.
	class, _ := windows.UTF16PtrFromString("STATIC")
	owner, _, err := clipboardCreateWindow.Call(0, uintptr(unsafe.Pointer(class)), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, 0, 0)
	if owner == 0 {
		return nil, clipboardError("create clipboard owner", err)
	}
	return &platformClipboard{owner: owner}, nil
}
func (c *platformClipboard) Close() {
	if c.owner != 0 {
		clipboardDestroyWindow.Call(c.owner)
		c.owner = 0
	}
}
func (c *platformClipboard) open() error {
	ok, _, err := clipboardOpen.Call(c.owner)
	if ok == 0 {
		return clipboardError("open clipboard", err)
	}
	return nil
}
func (c *platformClipboard) ReadText() (string, error) {
	if err := c.open(); err != nil {
		return "", err
	}
	defer clipboardClose.Call()
	if available, _, _ := clipboardAvailable.Call(clipboardUnicodeText); available == 0 {
		return "", nil
	}
	handle, _, err := clipboardGet.Call(clipboardUnicodeText)
	if handle == 0 {
		return "", clipboardError("read clipboard", err)
	}
	size, _, err := clipboardSize.Call(handle)
	if size < 2 || size > clipboardMaxBytes || size%2 != 0 {
		return "", fmt.Errorf("invalid clipboard allocation size %d", size)
	}
	ptr, _, err := clipboardLock.Call(handle)
	if ptr == 0 {
		return "", clipboardError("lock clipboard text", err)
	}
	defer clipboardUnlock.Call(handle)
	data := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), int(size/2))
	for i, value := range data {
		if value == 0 {
			return windows.UTF16ToString(data[:i]), nil
		}
	}
	return "", fmt.Errorf("clipboard text has no terminator")
}
func (c *platformClipboard) SetText(text string) error {
	data, err := windows.UTF16FromString(text)
	if err != nil {
		return err
	}
	if len(data) > clipboardMaxBytes/2 {
		return fmt.Errorf("clipboard text exceeds %d bytes", clipboardMaxBytes)
	}
	handle, _, err := clipboardAlloc.Call(2, uintptr(len(data)*2)) // GMEM_MOVEABLE
	if handle == 0 {
		return clipboardError("allocate clipboard text", err)
	}
	transferred := false
	defer func() {
		if !transferred {
			clipboardFree.Call(handle)
		}
	}()
	ptr, _, err := clipboardLock.Call(handle)
	if ptr == 0 {
		return clipboardError("lock clipboard allocation", err)
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(data)), data)
	clipboardUnlock.Call(handle)
	if err := c.open(); err != nil {
		return err
	}
	defer clipboardClose.Call()
	if ok, _, err := clipboardEmpty.Call(); ok == 0 {
		return clipboardError("empty clipboard", err)
	}
	if ok, _, err := clipboardSet.Call(clipboardUnicodeText, handle); ok == 0 {
		return clipboardError("write clipboard text", err)
	}
	transferred = true // Windows owns and frees the allocation after successful SetClipboardData.
	return nil
}
