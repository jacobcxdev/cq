//go:build windows

package dots

import (
	"runtime"
	"slices"
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
	executable, root := `C:\Program Files\CQ\cq.exe`, `C:\CQ Data\dots`
	values := windowsDotsRelaunchProperties(executable, root, `C:\CQ Data\dots\ChatGPT.ico`)
	args, err := windows.DecomposeCommandLine(values[0])
	if err != nil || !slices.Equal(args, []string{executable, "codex", "dots", "launch", "--root", root}) {
		t.Fatalf("taskbar relaunch arguments = %q, %v", args, err)
	}
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
