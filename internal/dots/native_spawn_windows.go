//go:build windows

package dots

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func nativeAppExecutable(s *Settings) string { return filepath.Join(s.AppPath, "app", "ChatGPT.exe") }
func nativeAppCheck(s *Settings) error       { return CheckWindowsApp(s) }
func nativeAppArchive(s *Settings) string {
	return filepath.Join(s.AppPath, "app", "resources", "app.asar")
}
func nativePipeLaunchArgs(args []string) []string {
	return append([]string(nil), args...)
}
func nativeDebugFlagsMatch(flags []string) bool {
	if len(flags) != 2 || flags[0] != "--remote-debugging-pipe" || !strings.HasPrefix(flags[1], "--remote-debugging-io-pipes=") {
		return false
	}
	values := strings.Split(strings.TrimPrefix(flags[1], "--remote-debugging-io-pipes="), ",")
	if len(values) != 2 || values[0] == values[1] {
		return false
	}
	for _, value := range values {
		number, err := strconv.ParseUint(value, 10, 64)
		if err != nil || number == 0 {
			return false
		}
	}
	return true
}

// Chromium on Windows receives native HANDLE values in this switch. Its
// --remote-debugging-pipe descriptor 3/4 contract applies only to POSIX.
func startNativeApp(executable string, args, env []string, input, output *os.File) (*os.Process, error) {
	if input == nil || output == nil {
		return nil, errors.New("native debugger pipe unavailable")
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--remote-debugging-") {
			return nil, errors.New("native debugger flags are broker-owned")
		}
	}
	process := windows.CurrentProcess()
	var childInput, childOutput windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(input.Fd()), process, &childInput, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	defer windows.CloseHandle(childInput)
	if err := windows.DuplicateHandle(process, windows.Handle(output.Fd()), process, &childOutput, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	defer windows.CloseHandle(childOutput)
	flag := "--remote-debugging-io-pipes=" + strconv.FormatUint(uint64(childInput), 10) + "," + strconv.FormatUint(uint64(childOutput), 10)
	arguments := append(append([]string{executable}, args...), "--remote-debugging-pipe", flag)
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	return os.StartProcess(executable, arguments, &os.ProcAttr{
		Env:   env,
		Files: []*os.File{null, os.Stdout, os.Stderr},
		Sys:   &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(childInput), syscall.Handle(childOutput)}},
	})
}
