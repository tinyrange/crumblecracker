package desktopapp

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const glassWindowFocusSupported = true

var (
	glassCurrentDC        = windows.NewLazySystemDLL("opengl32.dll").NewProc("wglGetCurrentDC")
	glassModule           = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
	glassUser32           = windows.NewLazySystemDLL("user32.dll")
	glassWindowFromDC     = glassUser32.NewProc("WindowFromDC")
	glassWindowClass      = glassUser32.NewProc("GetClassNameW")
	glassUnregisterClass  = glassUser32.NewProc("UnregisterClassW")
	glassSetWindowPos     = glassUser32.NewProc("SetWindowPos")
	glassForegroundWindow = glassUser32.NewProc("SetForegroundWindow")
	glassIsIconic         = glassUser32.NewProc("IsIconic")
	glassRestoreWindow    = glassUser32.NewProc("ShowWindow")
	glassComctl32         = windows.NewLazySystemDLL("comctl32.dll")
	glassSetSubclass      = glassComctl32.NewProc("SetWindowSubclass")
	glassRemoveSubclass   = glassComctl32.NewProc("RemoveWindowSubclass")
	glassDefaultSubclass  = glassComctl32.NewProc("DefSubclassProc")
	glassDPIWindowProc    uintptr
)

func init() { glassDPIWindowProc = syscall.NewCallback(glassDPIChanged) }

func glassDPIChanged(hwnd, msg, wParam, lParam, id, _ uintptr) uintptr {
	switch msg {
	case 0x02e0: // WM_DPICHANGED
		var r struct{ left, top, right, bottom int32 }
		// Win32 supplies the suggested RECT by address. Copy it while the
		// message owns it and keep that address outside Go's pointer space.
		if err := windows.ReadProcessMemory(windows.CurrentProcess(), lParam, (*byte)(unsafe.Pointer(&r)), unsafe.Sizeof(r), nil); err == nil {
			const noZOrderOrActivate = 0x0004 | 0x0010
			glassSetWindowPos.Call(hwnd, 0, uintptr(r.left), uintptr(r.top),
				uintptr(r.right-r.left), uintptr(r.bottom-r.top), noZOrderOrActivate)
			return 0
		}
	case 0x0082: // WM_NCDESTROY
		glassRemoveSubclass.Call(hwnd, glassDPIWindowProc, id)
	}
	result, _, _ := glassDefaultSubclass.Call(hwnd, msg, wParam, lParam)
	return result
}

func prepareGlassWindow() (show, release func()) {
	// The viewer thread's current WGL context identifies its own window.
	noop := func() {}
	dc, _, _ := glassCurrentDC.Call()
	if dc == 0 {
		return noop, noop
	}
	hwnd, _, _ := glassWindowFromDC.Call(dc)
	if hwnd == 0 {
		return noop, noop
	}
	var class [256]uint16
	classLength, _, _ := glassWindowClass.Call(hwnd, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
	module, _, _ := glassModule.Call(0)
	glassSetSubclass.Call(hwnd, glassDPIWindowProc, 1, 0)
	return func() {
			if minimized, _, _ := glassIsIconic.Call(hwnd); minimized != 0 {
				const swRestore = 9
				glassRestoreWindow.Call(hwnd, swRestore)
			}
			// The first ShowWindow can inherit SW_HIDE from the hidden backend launch.
			// Put Glass above its launcher even when Windows reserves focus for Electron.
			// Returning it to the normal window stack lets other apps cover it as usual.
			const topmost, notTopmost = ^uintptr(0), ^uintptr(1)
			const swpNoSize, swpNoMove, swpNoActivate, swpShowWindow = 0x0001, 0x0002, 0x0010, 0x0040
			const positionOnly = swpNoSize | swpNoMove | swpNoActivate
			glassSetWindowPos.Call(hwnd, topmost, 0, 0, 0, 0, positionOnly|swpShowWindow)
			glassSetWindowPos.Call(hwnd, notTopmost, 0, 0, 0, 0, positionOnly)
			glassForegroundWindow.Call(hwnd)
		}, func() {
			// gowin registers its class on every window creation. Release it after
			// the old window is destroyed so a second Glass window can open.
			if classLength != 0 && module != 0 {
				glassUnregisterClass.Call(uintptr(unsafe.Pointer(&class[0])), module)
			}
		}
}
