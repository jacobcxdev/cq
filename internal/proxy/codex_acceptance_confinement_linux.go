//go:build linux

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type linuxCodexAcceptanceConfinement struct{}

var openLinuxAcceptanceHelper = func() (*os.File, error) {
	return os.Open("/proc/self/exe")
}

var startLinuxAcceptanceCommand = func(command *exec.Cmd) error { return command.Start() }

func defaultCodexAcceptanceConfinement() codexAcceptanceConfinement {
	return linuxCodexAcceptanceConfinement{}
}

func (linuxCodexAcceptanceConfinement) Execute(ctx context.Context, execution codexAcceptanceExecution) ([]byte, error) {
	if ctx == nil || execution.executable == "" {
		return nil, errors.New("Codex acceptance confinement unavailable")
	}
	config, err := newLinuxNamespaceConfig(execution)
	if err != nil {
		return nil, errors.New("Codex acceptance confinement unavailable")
	}
	client, err := openLinuxAcceptanceClient(execution)
	if err != nil {
		return nil, errors.New("Codex acceptance executable unavailable")
	}
	defer client.Close()
	helper, err := openLinuxAcceptanceHelper()
	if err != nil {
		return nil, errors.New("Codex acceptance helper unavailable")
	}
	defer helper.Close()
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("Codex acceptance descriptor confinement unavailable")
	}
	parent := os.NewFile(uintptr(sockets[0]), "linux-acceptance-parent")
	child := os.NewFile(uintptr(sockets[1]), "linux-acceptance-child")
	if parent == nil || child == nil {
		if parent != nil {
			_ = parent.Close()
		} else {
			_ = unix.Close(sockets[0])
		}
		if child != nil {
			_ = child.Close()
		} else {
			_ = unix.Close(sockets[1])
		}
		return nil, errors.New("Codex acceptance descriptor confinement unavailable")
	}
	defer parent.Close()
	defer child.Close()
	command := exec.Command("/proc/self/fd/3", "proxy", "__linux-acceptance-helper")
	command.ExtraFiles = []*os.File{helper, child, client}
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdin = nil
	command.Stderr = io.Discard
	var output codexAcceptanceLimitedBuffer
	output.limit = codexAcceptanceOutputLimit
	if execution.command.captureOutput {
		command.Stdout = &output
	} else {
		command.Stdout = io.Discard
	}
	command.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
	if err := startLinuxAcceptanceCommand(command); err != nil {
		return nil, fmt.Errorf("Codex acceptance process confinement unavailable: %w", err)
	}
	defer killLinuxAcceptanceGroup(command.Process.Pid)
	_ = child.Close()
	_ = helper.Close()
	encoded, err := json.Marshal(config)
	if err != nil || len(encoded) == 0 || len(encoded) > linuxNamespaceConfigMaxBytes {
		killLinuxAcceptanceGroup(command.Process.Pid)
		_ = command.Wait()
		return nil, errors.New("Codex acceptance confinement unavailable")
	}
	written, sendErr := unix.SendmsgN(int(parent.Fd()), encoded, nil, nil, unix.MSG_NOSIGNAL)
	if sendErr != nil || written != len(encoded) {
		killLinuxAcceptanceGroup(command.Process.Pid)
		_ = command.Wait()
		return nil, errors.New("Codex acceptance helper unavailable")
	}
	ready := make(chan error, 1)
	failures := make(chan error, 1)
	bridges := newLinuxRelayBridges()
	receiverDone := make(chan struct{})
	defer func() {
		_ = parent.Close()
		<-receiverDone
		bridges.closeAndWait()
	}()
	go runLinuxRelayReceiver(int(parent.Fd()), config.Relays, ready, failures, bridges, receiverDone)
	wait := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				wait <- errors.New("Linux acceptance helper panic")
			}
		}()
		wait <- command.Wait()
	}()
	readyTimer := time.NewTimer(3 * time.Second)
	defer readyTimer.Stop()
	select {
	case err := <-ready:
		if err != nil {
			killLinuxAcceptanceGroup(command.Process.Pid)
			<-wait
			return nil, errors.New("Codex acceptance confinement unavailable")
		}
	case err := <-failures:
		_ = err
		killLinuxAcceptanceGroup(command.Process.Pid)
		<-wait
		return nil, errors.New("Codex acceptance relay confinement unavailable")
	case <-readyTimer.C:
		killLinuxAcceptanceGroup(command.Process.Pid)
		<-wait
		return nil, errors.New("Codex acceptance confinement unavailable")
	case <-ctx.Done():
		killLinuxAcceptanceGroup(command.Process.Pid)
		<-wait
		return nil, errors.New("Codex acceptance command timed out")
	case <-wait:
		return nil, errors.New("Codex acceptance confinement unavailable")
	}
	select {
	case err := <-wait:
		_ = parent.Close()
		if err != nil {
			if ctx.Err() != nil {
				return nil, errors.New("Codex acceptance command timed out")
			}
			return nil, errors.New("Codex acceptance command failed")
		}
		return output.Bytes(), nil
	case <-failures:
		killLinuxAcceptanceGroup(command.Process.Pid)
		<-wait
		return nil, errors.New("Codex acceptance relay confinement unavailable")
	case <-ctx.Done():
		killLinuxAcceptanceGroup(command.Process.Pid)
		<-wait
		return nil, errors.New("Codex acceptance command timed out")
	}
}

func newLinuxNamespaceConfig(execution codexAcceptanceExecution) (linuxNamespaceConfig, error) {
	config := linuxNamespaceConfig{
		Version:    linuxNamespaceProtocolVersion,
		Executable: execution.executable,
		Args:       append([]string(nil), execution.args...),
		Env:        append([]string(nil), execution.command.env...),
		Dir:        execution.command.dir,
		WriteRoot:  execution.command.sandboxWriteRoot,
	}
	urls := []struct {
		id  linuxRelayID
		raw string
	}{{linuxRelayProxy, execution.command.endpoint}, {linuxRelayEgress, execution.command.egressProxyURL}}
	for _, item := range urls {
		if item.raw == "" {
			continue
		}
		parsed, err := url.Parse(item.raw)
		if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
			return linuxNamespaceConfig{}, errors.New("invalid Linux relay URL")
		}
		port, err := strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65_535 {
			return linuxNamespaceConfig{}, errors.New("invalid Linux relay URL")
		}
		config.Relays = append(config.Relays, linuxRelayDefinition{ID: item.id, Port: port, Target: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))})
	}
	if err := config.validate(); err != nil {
		return linuxNamespaceConfig{}, err
	}
	return config, nil
}

func openLinuxAcceptanceClient(execution codexAcceptanceExecution) (*os.File, error) {
	if execution.proof.valid() && execution.proof.path == execution.executable {
		return openCodexInstalledAcceptanceExecutable(execution.proof)
	}
	path, err := exec.LookPath(execution.executable)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
	}
	return os.Open(path)
}

func killLinuxAcceptanceGroup(pid int) {
	if pid > 1 {
		_ = unix.Kill(-pid, unix.SIGKILL)
	}
}

func killLinuxAcceptanceProcess(pid int) {
	if pid > 1 {
		_ = unix.Kill(pid, unix.SIGKILL)
	}
}
