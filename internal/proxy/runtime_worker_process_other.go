//go:build !linux

package proxy

import "os/exec"

func configureRuntimeWorkerCommand(*exec.Cmd) {}
