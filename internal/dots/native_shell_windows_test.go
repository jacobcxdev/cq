//go:build windows

package dots

import (
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsDotsTaskbarPropertiesUseCQIconAndRelaunch(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	status, _, _ := dotsCoInitializeEx.Call(0, 2)
	if int32(status) < 0 {
		t.Fatalf("initialise the test window COM apartment: %#x", uint32(status))
	}
	defer dotsCoUninitialize.Call()
	create := dotsUser32.NewProc("CreateWindowExW")
	destroy := dotsUser32.NewProc("DestroyWindow")
	class, err := windows.UTF16PtrFromString("STATIC")
	if err != nil {
		t.Fatal(err)
	}
	window, _, err := create.Call(0, uintptr(unsafe.Pointer(class)), 0, 0, 0, 0, 8, 8, 0, 0, 0, 0)
	if window == 0 {
		t.Fatalf("create the test window: %v", err)
	}
	defer destroy.Call(window)
	values := [4]string{`"C:\Program Files\CQ\cq.exe" codex dots start`, `C:\CQ\ChatGPT.ico,0`, "ChatGPT via CQ", windowsDotsAppID}
	if !windowsDotsWindowProperties(window, values, false) || !windowsDotsWindowProperties(window, values, false) {
		t.Fatal("failed to apply or refresh the CQ taskbar properties")
	}
	other := values
	other[3] = "Example.Other.App"
	if windowsDotsWindowProperties(window, other, false) || windowsDotsWindowProperties(window, other, true) {
		t.Fatal("an unrelated group was able to overwrite or clear the CQ properties")
	}
	if !windowsDotsWindowProperties(window, values, true) || !windowsDotsWindowProperties(window, other, false) || !windowsDotsWindowProperties(window, other, true) {
		t.Fatal("taskbar properties were not cleared before relaunch")
	}
}
