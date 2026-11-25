//go:build windows

package dots

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsDotsAppID = "JacobCX.CQ.ChatGPT"

var (
	dotsUser32                   = windows.NewLazySystemDLL("user32.dll")
	dotsEnumWindows              = dotsUser32.NewProc("EnumWindows")
	dotsGetWindow                = dotsUser32.NewProc("GetWindow")
	dotsGetWindowThreadProcessID = dotsUser32.NewProc("GetWindowThreadProcessId")
	dotsIsWindowVisible          = dotsUser32.NewProc("IsWindowVisible")
	dotsGetClassName             = dotsUser32.NewProc("GetClassNameW")
	dotsWindowPropertyStore      = windows.NewLazySystemDLL("shell32.dll").NewProc("SHGetPropertyStoreForWindow")
	dotsOle32                    = windows.NewLazySystemDLL("ole32.dll")
	dotsCoInitializeEx           = dotsOle32.NewProc("CoInitializeEx")
	dotsCoUninitialize           = dotsOle32.NewProc("CoUninitialize")
	dotsPropVariantClear         = dotsOle32.NewProc("PropVariantClear")
	dotsPropertyStoreIID         = windows.GUID{Data1: 0x886d8eeb, Data2: 0x8cf2, Data3: 0x4446, Data4: [8]byte{0x8d, 0x02, 0xcd, 0xba, 0x1d, 0xbd, 0xcf, 0x99}}
	dotsAppModelPropertyFormat   = windows.GUID{Data1: 0x9f4c2855, Data2: 0x9f79, Data3: 0x4b39, Data4: [8]byte{0xa8, 0xd0, 0xe1, 0xd4, 0x2d, 0xe1, 0xd5, 0xf3}}
)

type dotsPropertyKey struct {
	format windows.GUID
	id     uint32
}

type dotsPropVariant struct {
	typeID uint16
	_      [3]uint16
	text   *uint16
	_      uintptr
}

type dotsPropertyStore struct {
	vtable *struct {
		queryInterface, addRef, release, getCount, getAt, getValue, setValue, commit uintptr
	}
}

func (store *dotsPropertyStore) value(id uint32) (string, bool) {
	key := dotsPropertyKey{format: dotsAppModelPropertyFormat, id: id}
	var value dotsPropVariant
	result, _, _ := syscall.SyscallN(store.vtable.getValue, uintptr(unsafe.Pointer(store)), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value)))
	defer func() { dotsPropVariantClear.Call(uintptr(unsafe.Pointer(&value))) }()
	if result != 0 {
		return "", false
	}
	if value.typeID == 0 { // VT_EMPTY
		return "", true
	}
	if value.typeID != 31 || value.text == nil { // VT_LPWSTR
		return "", false
	}
	return windows.UTF16PtrToString(value.text), true
}

func (store *dotsPropertyStore) set(id uint32, text string) bool {
	key := dotsPropertyKey{format: dotsAppModelPropertyFormat, id: id}
	var value dotsPropVariant
	if text != "" {
		pointer, err := windows.UTF16PtrFromString(text)
		if err != nil {
			return false
		}
		value.typeID, value.text = 31, pointer
	}
	result, _, _ := syscall.SyscallN(store.vtable.setValue, uintptr(unsafe.Pointer(store)), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value)))
	runtime.KeepAlive(value)
	return result == 0
}

// Keep the CQ relaunch command with the icon so the taskbar cannot relaunch
// the stock app when the managed window is pinned. Set the grouping ID last.
func windowsDotsWindowProperties(window uintptr, values [4]string, clear bool) bool {
	var store *dotsPropertyStore
	result, _, _ := dotsWindowPropertyStore.Call(window, uintptr(unsafe.Pointer(&dotsPropertyStoreIID)), uintptr(unsafe.Pointer(&store)))
	if result != 0 || store == nil {
		return false
	}
	defer syscall.SyscallN(store.vtable.release, uintptr(unsafe.Pointer(store)))
	var prior [4]string
	for i, expected := range values {
		previous, ok := store.value(uint32(i + 2))
		if !ok || (clear && previous != expected) || (!clear && previous != "" && previous != expected) {
			return false
		}
		prior[i] = previous
	}
	if clear {
		values = [4]string{}
	}
	for i, value := range values {
		if !store.set(uint32(i+2), value) {
			for j := i - 1; j >= 0; j-- {
				store.set(uint32(j+2), prior[j])
			}
			return false
		}
	}
	return true
}

func windowsDotsWindowPID(window uintptr) uint32 {
	var pid uint32
	dotsGetWindowThreadProcessID.Call(window, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func nativeAppPresentation(s *Settings, pid int, env []string) func() {
	executable := environmentValue(env, "CQ_DOTS_CQ_PATH")
	root := filepath.Dir(s.RuntimeRoot)
	if !cleanAbsolute(executable) || !strings.EqualFold(filepath.Ext(executable), ".exe") {
		return func() {}
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		icon, err := installWindowsDotsIcon(root, s.AppPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cq: Windows Dots taskbar icon unavailable: %v\n", err)
			return
		}
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		status, _, _ := dotsCoInitializeEx.Call(0, 2) // COINIT_APARTMENTTHREADED
		if int32(status) < 0 {
			return
		}
		defer dotsCoUninitialize.Call()
		command := syscall.EscapeArg(executable) + " codex dots start --root " + syscall.EscapeArg(root)
		values := [4]string{command, icon + ",0", "ChatGPT via CQ", windowsDotsAppID}
		applied := map[uintptr]bool{}
		releaseWindow := func(window uintptr) {
			if windowsDotsWindowPID(window) == uint32(pid) {
				windowsDotsWindowProperties(window, values, true)
			}
			delete(applied, window)
		}
		defer func() {
			for window := range applied {
				releaseWindow(window)
			}
		}()
		seen := map[uintptr]bool{}
		visit := windows.NewCallback(func(window, _ uintptr) uintptr {
			if windowsDotsWindowPID(window) != uint32(pid) {
				return 1
			}
			visible, _, _ := dotsIsWindowVisible.Call(window)
			owner, _, _ := dotsGetWindow.Call(window, 4) // GW_OWNER
			if visible == 0 || owner != 0 {
				return 1
			}
			var class [64]uint16
			dotsGetClassName.Call(window, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
			if windows.UTF16ToString(class[:]) == "Chrome_WidgetWin_1" {
				seen[window] = true
				if !applied[window] && windowsDotsWindowPID(window) == uint32(pid) {
					applied[window] = windowsDotsWindowProperties(window, values, false)
				}
			}
			return 1
		})
		interval := time.NewTicker(time.Second)
		defer interval.Stop()
		for ctx.Err() == nil {
			dotsEnumWindows.Call(visit, 0)
			for window := range applied {
				if !seen[window] {
					releaseWindow(window)
				}
			}
			clear(seen)
			select {
			case <-ctx.Done():
			case <-interval.C:
			}
		}
	}()
	return func() { stop(); <-done }
}
