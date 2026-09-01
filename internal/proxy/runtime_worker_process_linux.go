//go:build linux

package proxy

import (
	"os/exec"
	"syscall"
)

func configureRuntimeWorkerCommand(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
