package dots

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// Keep Chromium's private fd 3/4 transport, but give the signed app its own
// macOS privacy responsibility. LaunchServices cannot pass these descriptors.
// This is the same spawn attribute used by Chromium and LLDB; do not fall back
// to attributing the app's permissions to CQ if the system API is unavailable.
type nativeSpawnAPI struct {
	attrInit    func(*uintptr) int32
	attrDestroy func(*uintptr) int32
	flags       func(*uintptr, int16) int32
	signalMask  func(*uintptr, *uint32) int32
	disclaim    func(*uintptr, bool) int32
	actionsInit func(*uintptr) int32
	actionsFree func(*uintptr) int32
	dup2        func(*uintptr, int32, int32) int32
	closeFD     func(*uintptr, int32) int32
	spawn       func(*int32, *byte, *uintptr, *uintptr, **byte, **byte) int32
}

var nativeSpawnFunctions = sync.OnceValues(func() (*nativeSpawnAPI, error) {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("native app launch API unavailable: %w", err)
	}
	api := &nativeSpawnAPI{}
	for _, symbol := range []struct {
		name string
		fn   any
	}{
		{"posix_spawnattr_init", &api.attrInit},
		{"posix_spawnattr_destroy", &api.attrDestroy},
		{"posix_spawnattr_setflags", &api.flags},
		{"posix_spawnattr_setsigmask", &api.signalMask},
		{"responsibility_spawnattrs_setdisclaim", &api.disclaim},
		{"posix_spawn_file_actions_init", &api.actionsInit},
		{"posix_spawn_file_actions_destroy", &api.actionsFree},
		{"posix_spawn_file_actions_adddup2", &api.dup2},
		{"posix_spawn_file_actions_addclose", &api.closeFD},
		{"posix_spawn", &api.spawn},
	} {
		address, err := purego.Dlsym(lib, symbol.name)
		if err != nil {
			_ = purego.Dlclose(lib)
			return nil, fmt.Errorf("native app launch API unavailable: %s", symbol.name)
		}
		purego.RegisterFunc(symbol.fn, address)
	}
	// libSystem and these function pointers remain loaded for this process.
	return api, nil
})

func startNativeApp(executable string, args, env []string, input, output *os.File) (*os.Process, error) {
	path, err := syscall.BytePtrFromString(executable)
	if err != nil {
		return nil, err
	}
	argv, err := syscall.SlicePtrFromStrings(append([]string{executable}, args...))
	if err != nil {
		return nil, err
	}
	envp, err := syscall.SlicePtrFromStrings(env)
	if err != nil {
		return nil, err
	}
	api, err := nativeSpawnFunctions()
	if err != nil {
		return nil, err
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	var attr, actions uintptr
	if code := api.attrInit(&attr); code != 0 {
		return nil, syscall.Errno(code)
	}
	defer api.attrDestroy(&attr)
	if code := api.actionsInit(&actions); code != 0 {
		return nil, syscall.Errno(code)
	}
	defer api.actionsFree(&actions)
	// POSIX_SPAWN_CLOEXEC_DEFAULT | POSIX_SPAWN_SETSIGMASK. Do not inherit a
	// Go runtime thread's blocked signals or any unrelated broker descriptors.
	if code := api.flags(&attr, 0x4000|0x0008); code != 0 {
		return nil, syscall.Errno(code)
	}
	var mask uint32
	if code := api.signalMask(&attr, &mask); code != 0 {
		return nil, syscall.Errno(code)
	}
	if code := api.disclaim(&attr, true); code != 0 {
		return nil, syscall.Errno(code)
	}
	for target, file := range []*os.File{null, os.Stdout, os.Stderr, input, output} {
		if file == nil {
			return nil, syscall.EBADF
		}
		// Duplicate above all destination slots before adding child actions,
		// so remapping fd 3/4 cannot overwrite a later action's source.
		fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 5)
		if err != nil {
			return nil, err
		}
		defer unix.Close(fd)
		if code := api.dup2(&actions, int32(fd), int32(target)); code != 0 {
			return nil, syscall.Errno(code)
		}
		if code := api.closeFD(&actions, int32(fd)); code != 0 {
			return nil, syscall.Errno(code)
		}
	}
	var pid int32
	code := api.spawn(&pid, path, &actions, &attr, &argv[0], &envp[0])
	// posix_spawn copies these synchronous arguments; it retains no Go pointers.
	runtime.KeepAlive(path)
	runtime.KeepAlive(argv)
	runtime.KeepAlive(envp)
	runtime.KeepAlive(input)
	runtime.KeepAlive(output)
	if code != 0 {
		return nil, &os.PathError{Op: "spawn", Path: executable, Err: syscall.Errno(code)}
	}
	return os.FindProcess(int(pid))
}
