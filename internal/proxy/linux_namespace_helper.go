//go:build linux

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	linuxNamespaceProtocolVersion       uint8 = 1
	linuxNamespaceConfigMaxBytes              = 64 << 10
	linuxNamespaceMaxArguments                = 128
	linuxNamespaceMaxEnvironment              = 128
	linuxNamespaceControlFD                   = 4
	linuxNamespaceClientFD                    = 5
	linuxAcceptanceNetworkThreadTimeout       = 3 * time.Second
)

type linuxNamespaceConfig struct {
	Version    uint8                  `json:"version"`
	Executable string                 `json:"executable"`
	Args       []string               `json:"args"`
	Env        []string               `json:"env"`
	Dir        string                 `json:"dir,omitempty"`
	WriteRoot  string                 `json:"write_root,omitempty"`
	Relays     []linuxRelayDefinition `json:"relays,omitempty"`
}

func (config linuxNamespaceConfig) validate() error {
	if config.Version != linuxNamespaceProtocolVersion || config.Executable == "" || len(config.Args) > linuxNamespaceMaxArguments || len(config.Env) > linuxNamespaceMaxEnvironment || len(config.Relays) > 2 {
		return errors.New("invalid Linux namespace config")
	}
	for _, value := range []string{config.Executable, config.Dir, config.WriteRoot} {
		if strings.IndexByte(value, 0) >= 0 {
			return errors.New("invalid Linux namespace config")
		}
	}
	total := len(config.Executable) + len(config.Dir) + len(config.WriteRoot)
	for _, values := range [][]string{config.Args, config.Env} {
		for _, value := range values {
			if strings.IndexByte(value, 0) >= 0 {
				return errors.New("invalid Linux namespace config")
			}
			total += len(value)
		}
	}
	if total > linuxNamespaceConfigMaxBytes/2 {
		return errors.New("invalid Linux namespace config")
	}
	if config.WriteRoot != "" {
		root := filepath.Clean(config.WriteRoot)
		if !filepath.IsAbs(root) || root == string(filepath.Separator) || filepath.Dir(root) == root || root != config.WriteRoot {
			return errors.New("invalid Linux namespace write root")
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil || resolved != root {
			return errors.New("invalid Linux namespace write root")
		}
	}
	if config.Dir != "" {
		resolved, err := filepath.EvalSymlinks(config.Dir)
		if err != nil || resolved != config.Dir || config.WriteRoot == "" || !linuxPathWithin(config.WriteRoot, config.Dir) {
			return errors.New("invalid Linux namespace directory")
		}
	}
	seenIDs := make(map[linuxRelayID]bool, len(config.Relays))
	seenPorts := make(map[int]bool, len(config.Relays))
	for _, relay := range config.Relays {
		if relay.ID != linuxRelayProxy && relay.ID != linuxRelayEgress || seenIDs[relay.ID] || seenPorts[relay.Port] || relay.Port < 1 || relay.Port > 65_535 {
			return errors.New("invalid Linux relay definition")
		}
		host, port, err := net.SplitHostPort(relay.Target)
		parsedPort, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || host != "127.0.0.1" || parsedPort != relay.Port {
			return errors.New("invalid Linux relay target")
		}
		seenIDs[relay.ID] = true
		seenPorts[relay.Port] = true
	}
	return nil
}

func linuxPathWithin(root, path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// RunLinuxAcceptanceNamespaceHelper runs hidden Linux helper role.
func RunLinuxAcceptanceNamespaceHelper(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Linux acceptance helper unavailable")
	}
	_ = unix.Close(3)
	controlFD := linuxNamespaceControlFD
	clientFD := linuxNamespaceClientFD
	unix.CloseOnExec(controlFD)
	unix.CloseOnExec(clientFD)
	config, err := receiveLinuxNamespaceConfig(controlFD)
	if err != nil {
		return errors.New("Linux acceptance helper unavailable")
	}
	sender := &linuxRelaySender{fd: controlFD}
	client := os.NewFile(uintptr(clientFD), "linux-acceptance-client")
	if client == nil {
		return errors.New("Linux acceptance executable unavailable")
	}
	defer client.Close()
	stdin, err := os.Open("/dev/null")
	if err != nil {
		return errors.New("Linux acceptance input unavailable")
	}
	defer stdin.Close()
	command := &exec.Cmd{
		Path:       "/proc/self/fd/3",
		Args:       append([]string{config.Executable}, config.Args...),
		Env:        append([]string(nil), config.Env...),
		Dir:        config.Dir,
		Stdin:      stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		ExtraFiles: []*os.File{client},
		SysProcAttr: &syscall.SysProcAttr{
			Pdeathsig: syscall.SIGKILL,
			Setpgid:   true,
		},
	}
	networkListenerTarget := make(chan *os.File)
	networkReady := make(chan struct{})
	stopNetwork := make(chan struct{})
	networkDone := make(chan error, 1)
	go func() {
		networkDone <- runLinuxAcceptanceNetworkSupervisorSafely(func() error {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			close(networkReady)
			listener, ok := <-networkListenerTarget
			if !ok {
				return errors.New("Linux acceptance network supervisor unavailable")
			}
			return runLinuxAcceptanceNetworkSupervisor(listener, config.Relays, stopNetwork)
		})
	}()
	if err := waitLinuxAcceptanceNetworkSupervisorReady(ctx, networkListenerTarget, networkReady, networkDone); err != nil {
		return errors.New("Linux acceptance network confinement unavailable")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := applyLinuxLandlockWriteConfinement(config.WriteRoot); err != nil {
		stopLinuxAcceptanceNetworkSupervisorBeforeAttach(networkListenerTarget, networkDone)
		return errors.New("Linux acceptance filesystem confinement unavailable")
	}
	if err := verifyLinuxNamespaceDescriptors(map[int]bool{controlFD: true, clientFD: true}); err != nil {
		stopLinuxAcceptanceNetworkSupervisorBeforeAttach(networkListenerTarget, networkDone)
		return errors.New("Linux acceptance descriptor confinement unavailable")
	}
	networkListener, err := installLinuxAcceptanceNetworkFilter()
	if err != nil {
		stopLinuxAcceptanceNetworkSupervisorBeforeAttach(networkListenerTarget, networkDone)
		return errors.New("Linux acceptance network confinement unavailable")
	}
	if err := attachLinuxAcceptanceNetworkSupervisor(ctx, networkListenerTarget, networkListener, networkDone); err != nil {
		_ = networkListener.Close()
		return errors.New("Linux acceptance network confinement unavailable")
	}
	if err := command.Start(); err != nil {
		_ = stopLinuxAcceptanceNetworkSupervisor(networkListener, stopNetwork, networkDone)
		return errors.New("Linux acceptance command failed")
	}
	if err := sender.ready(); err != nil {
		killLinuxAcceptanceGroup(command.Process.Pid)
		_ = command.Wait()
		_ = stopLinuxAcceptanceNetworkSupervisor(networkListener, stopNetwork, networkDone)
		return errors.New("Linux acceptance helper unavailable")
	}
	wait := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				wait <- errors.New("Linux acceptance client panic")
			}
		}()
		wait <- command.Wait()
	}()
	select {
	case err := <-wait:
		killLinuxAcceptanceGroup(command.Process.Pid)
		networkErr := stopLinuxAcceptanceNetworkSupervisor(networkListener, stopNetwork, networkDone)
		if networkErr != nil {
			return errors.New("Linux acceptance network confinement unavailable")
		}
		if err != nil {
			return errors.New("Linux acceptance command failed")
		}
		return nil
	case <-networkDone:
		killLinuxAcceptanceGroup(command.Process.Pid)
		<-wait
		close(stopNetwork)
		_ = networkListener.Close()
		return errors.New("Linux acceptance network confinement unavailable")
	case <-ctx.Done():
		killLinuxAcceptanceGroup(command.Process.Pid)
		<-wait
		_ = stopLinuxAcceptanceNetworkSupervisor(networkListener, stopNetwork, networkDone)
		return errors.New("Linux acceptance command timed out")
	}
}

func runLinuxAcceptanceNetworkSupervisorSafely(run func() error) (returnErr error) {
	defer func() {
		if recover() != nil {
			returnErr = errors.New("Linux acceptance network supervisor panic")
		}
	}()
	if run == nil {
		return errors.New("Linux acceptance network supervisor unavailable")
	}
	return run()
}

func waitLinuxAcceptanceNetworkSupervisorReady(ctx context.Context, target chan *os.File, ready <-chan struct{}, done <-chan error) error {
	timer := time.NewTimer(linuxAcceptanceNetworkThreadTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		return nil
	case err := <-done:
		return err
	case <-ctx.Done():
		stopLinuxAcceptanceNetworkSupervisorBeforeAttach(target, done)
		return ctx.Err()
	case <-timer.C:
		stopLinuxAcceptanceNetworkSupervisorBeforeAttach(target, done)
		return errors.New("Linux acceptance network supervisor startup timed out")
	}
}

func attachLinuxAcceptanceNetworkSupervisor(ctx context.Context, target chan<- *os.File, listener *os.File, done <-chan error) error {
	timer := time.NewTimer(linuxAcceptanceNetworkThreadTimeout)
	defer timer.Stop()
	select {
	case target <- listener:
		return nil
	case err := <-done:
		return err
	case <-ctx.Done():
		close(target)
		_ = awaitLinuxAcceptanceNetworkSupervisor(done)
		return ctx.Err()
	case <-timer.C:
		close(target)
		_ = awaitLinuxAcceptanceNetworkSupervisor(done)
		return errors.New("Linux acceptance network supervisor attach timed out")
	}
}

func stopLinuxAcceptanceNetworkSupervisorBeforeAttach(target chan *os.File, done <-chan error) {
	close(target)
	_ = awaitLinuxAcceptanceNetworkSupervisor(done)
}

func stopLinuxAcceptanceNetworkSupervisor(listener *os.File, stop chan struct{}, done <-chan error) error {
	close(stop)
	_ = listener.Close()
	return awaitLinuxAcceptanceNetworkSupervisor(done)
}

func awaitLinuxAcceptanceNetworkSupervisor(done <-chan error) error {
	timer := time.NewTimer(linuxAcceptanceNetworkThreadTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errors.New("Linux acceptance network supervisor cleanup timed out")
	}
}

func receiveLinuxNamespaceConfig(controlFD int) (linuxNamespaceConfig, error) {
	encoded := make([]byte, linuxNamespaceConfigMaxBytes+1)
	oob := make([]byte, 1)
	count, oobCount, flags, _, err := unix.Recvmsg(controlFD, encoded, oob, 0)
	if err != nil || count == 0 || count > linuxNamespaceConfigMaxBytes || oobCount != 0 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		return linuxNamespaceConfig{}, errors.New("invalid Linux namespace config")
	}
	var config linuxNamespaceConfig
	decoder := json.NewDecoder(strings.NewReader(string(encoded[:count])))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil || decoder.Decode(&struct{}{}) != io.EOF || config.validate() != nil {
		return linuxNamespaceConfig{}, errors.New("invalid Linux namespace config")
	}
	return config, nil
}

func verifyLinuxNamespaceDescriptors(explicit map[int]bool) error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil || len(entries) > 256 {
		return errors.New("inspect Linux descriptors")
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd <= 2 || explicit[fd] {
			continue
		}
		flags, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_GETFD, 0)
		if errno == unix.EBADF {
			continue
		}
		if errno != 0 || closeLinuxNamespaceDescriptorOnExec(fd, flags) != nil {
			return errors.New("unexpected inherited Linux descriptor")
		}
	}
	return nil
}

func closeLinuxNamespaceDescriptorOnExec(fd int, flags uintptr) error {
	if flags&unix.FD_CLOEXEC == 0 {
		_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC)
		if errno == unix.EBADF {
			return nil
		}
		if errno != 0 {
			return errno
		}
	}
	verified, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_GETFD, 0)
	if errno == unix.EBADF {
		return nil
	}
	if errno != 0 {
		return errno
	}
	if verified&unix.FD_CLOEXEC == 0 {
		return errors.New("Linux descriptor remained inheritable")
	}
	return nil
}
