//go:build linux

package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	linuxNamespaceProtocolVersion uint8 = 1
	linuxNamespaceConfigMaxBytes        = 64 << 10
	linuxNamespaceMaxArguments          = 128
	linuxNamespaceMaxEnvironment        = 128
	linuxNamespaceControlFD             = 4
	linuxNamespaceClientFD              = 5
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
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return errors.New("Linux acceptance mount confinement unavailable")
	}
	if err := bringLinuxLoopbackUp(); err != nil {
		return errors.New("Linux acceptance network confinement unavailable")
	}
	listeners, err := openLinuxRelayListeners(config.Relays)
	if err != nil {
		return errors.New("Linux acceptance relay confinement unavailable")
	}
	defer closeLinuxRelayListeners(listeners)
	if err := applyLinuxLandlockWriteConfinement(config.WriteRoot); err != nil {
		return errors.New("Linux acceptance filesystem confinement unavailable")
	}
	if err := verifyLinuxNamespaceDescriptors(map[int]bool{controlFD: true, clientFD: true}); err != nil {
		return errors.New("Linux acceptance descriptor confinement unavailable")
	}
	sender := &linuxRelaySender{fd: controlFD}
	acceptErrors := make(chan error, 1)
	for index, listener := range listeners {
		definition := config.Relays[index]
		go runLinuxRelayAcceptor(ctx, listener, definition.ID, sender, acceptErrors)
	}
	if err := sender.ready(); err != nil {
		return errors.New("Linux acceptance helper unavailable")
	}
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
		},
	}
	if err := command.Start(); err != nil {
		return errors.New("Linux acceptance command failed")
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
		if err != nil {
			return errors.New("Linux acceptance command failed")
		}
		return nil
	case <-acceptErrors:
		_ = command.Process.Kill()
		<-wait
		return errors.New("Linux acceptance relay failed")
	case <-ctx.Done():
		_ = command.Process.Kill()
		<-wait
		return errors.New("Linux acceptance command timed out")
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

func openLinuxRelayListeners(definitions []linuxRelayDefinition) ([]*net.TCPListener, error) {
	listeners := make([]*net.TCPListener, 0, len(definitions))
	for _, definition := range definitions {
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: definition.Port})
		if err != nil {
			closeLinuxRelayListeners(listeners)
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

func closeLinuxRelayListeners(listeners []*net.TCPListener) {
	for _, listener := range listeners {
		_ = listener.Close()
	}
}

func runLinuxRelayAcceptor(ctx context.Context, listener *net.TCPListener, id linuxRelayID, sender *linuxRelaySender, failures chan<- error) {
	defer func() {
		if recover() != nil {
			select {
			case failures <- errors.New("Linux relay panic"):
			default:
			}
		}
	}()
	for {
		connection, err := listener.AcceptTCP()
		if err != nil {
			if ctx.Err() == nil {
				select {
				case failures <- err:
				default:
				}
			}
			return
		}
		file, err := connection.File()
		_ = connection.Close()
		if err == nil {
			err = sender.connection(id, int(file.Fd()))
			_ = file.Close()
		}
		if err != nil {
			select {
			case failures <- err:
			default:
			}
			return
		}
	}
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
		if errno != 0 || flags&unix.FD_CLOEXEC == 0 {
			return errors.New("unexpected inherited Linux descriptor")
		}
	}
	return nil
}

func bringLinuxLoopbackUp() error {
	link, err := net.InterfaceByName("lo")
	if err != nil || link.Index <= 0 {
		return errors.New("resolve loopback")
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	sequence := uint32(time.Now().UnixNano())
	message := make([]byte, unix.NLMSG_HDRLEN+unix.SizeofIfInfomsg)
	native := binary.NativeEndian
	native.PutUint32(message[0:4], uint32(len(message)))
	native.PutUint16(message[4:6], unix.RTM_NEWLINK)
	native.PutUint16(message[6:8], unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	native.PutUint32(message[8:12], sequence)
	message[unix.NLMSG_HDRLEN] = unix.AF_UNSPEC
	native.PutUint32(message[unix.NLMSG_HDRLEN+4:unix.NLMSG_HDRLEN+8], uint32(link.Index))
	native.PutUint32(message[unix.NLMSG_HDRLEN+8:unix.NLMSG_HDRLEN+12], unix.IFF_UP)
	native.PutUint32(message[unix.NLMSG_HDRLEN+12:unix.NLMSG_HDRLEN+16], unix.IFF_UP)
	if err := unix.Sendto(fd, message, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	response := make([]byte, 4096)
	count, _, err := unix.Recvfrom(fd, response, 0)
	if err != nil || count < unix.NLMSG_HDRLEN+4 || native.Uint32(response[8:12]) != sequence || native.Uint16(response[4:6]) != unix.NLMSG_ERROR {
		return errors.New("configure loopback")
	}
	if errno := int32(native.Uint32(response[unix.NLMSG_HDRLEN : unix.NLMSG_HDRLEN+4])); errno != 0 {
		return syscall.Errno(-errno)
	}
	return nil
}
