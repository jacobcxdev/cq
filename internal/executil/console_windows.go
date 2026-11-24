package executil

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// SuppressConsoleWindow allows a pipe-only child to run without opening a console.
func SuppressConsoleWindow(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= windows.DETACHED_PROCESS
}
