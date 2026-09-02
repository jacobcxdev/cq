//go:build linux

package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	linuxBPFLoadWordAbsolute              = 0x20
	linuxBPFJumpEqualConstant             = 0x15
	linuxBPFReturnConstant                = 0x06
	linuxSeccompDataSyscallOffset         = 0
	linuxSeccompDataArchitectureOffset    = 4
	linuxSocketTypeMask                   = 0xf
	linuxAcceptanceSockaddrInet4Size      = 16
	linuxAcceptanceNotificationPollMillis = 100
)

type linuxSeccompData struct {
	Number             int32
	Architecture       uint32
	InstructionPointer uint64
	Arguments          [6]uint64
}

type linuxSeccompNotification struct {
	ID    uint64
	PID   uint32
	Flags uint32
	Data  linuxSeccompData
}

type linuxSeccompNotificationResponse struct {
	ID    uint64
	Value int64
	Error int32
	Flags uint32
}

type linuxAcceptanceIovec struct {
	Base uintptr
	Len  uintptr
}

func installLinuxAcceptanceNetworkFilter() (*os.File, error) {
	architecture := linuxSeccompAuditArchitecture()
	if architecture == 0 {
		return nil, errors.New("unsupported Linux seccomp architecture")
	}
	filtered := []uint32{
		uint32(unix.SYS_SOCKET),
		uint32(unix.SYS_CONNECT),
		uint32(unix.SYS_SENDTO),
		uint32(unix.SYS_SENDMSG),
		uint32(unix.SYS_SENDMMSG),
		uint32(unix.SYS_BIND),
		uint32(unix.SYS_LISTEN),
		uint32(unix.SYS_ACCEPT),
		uint32(unix.SYS_ACCEPT4),
		uint32(unix.SYS_IO_URING_SETUP),
		uint32(unix.SYS_PIDFD_GETFD),
		uint32(unix.SYS_PTRACE),
		uint32(unix.SYS_PROCESS_VM_READV),
		uint32(unix.SYS_PROCESS_VM_WRITEV),
		uint32(unix.SYS_BPF),
		uint32(unix.SYS_SECCOMP),
		uint32(unix.SYS_PRCTL),
		uint32(unix.SYS_SETPGID),
		uint32(unix.SYS_SETSID),
	}
	program := make([]syscall.SockFilter, 0, 5+len(filtered)*4)
	program = append(program,
		syscall.SockFilter{Code: linuxBPFLoadWordAbsolute, K: linuxSeccompDataArchitectureOffset},
		syscall.SockFilter{Code: linuxBPFJumpEqualConstant, Jt: 1, K: architecture},
		syscall.SockFilter{Code: linuxBPFReturnConstant, K: unix.SECCOMP_RET_KILL_PROCESS},
		syscall.SockFilter{Code: linuxBPFLoadWordAbsolute, K: linuxSeccompDataSyscallOffset},
	)
	for _, number := range filtered {
		variants := []uint32{number}
		if x32 := linuxSeccompX32SyscallBit(); x32 != 0 {
			variants = append(variants, number|x32)
		}
		for _, variant := range variants {
			program = append(program,
				syscall.SockFilter{Code: linuxBPFJumpEqualConstant, Jf: 1, K: variant},
				syscall.SockFilter{Code: linuxBPFReturnConstant, K: unix.SECCOMP_RET_USER_NOTIF},
			)
		}
	}
	program = append(program, syscall.SockFilter{Code: linuxBPFReturnConstant, K: unix.SECCOMP_RET_ALLOW})
	filter := syscall.SockFprog{Len: uint16(len(program)), Filter: &program[0]}
	fd, _, errno := unix.Syscall(
		unix.SYS_SECCOMP,
		unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_NEW_LISTENER,
		uintptr(unsafe.Pointer(&filter)),
	)
	runtime.KeepAlive(program)
	if errno != 0 {
		return nil, errno
	}
	listener := os.NewFile(fd, "linux-acceptance-seccomp")
	if listener == nil {
		_ = unix.Close(int(fd))
		return nil, errors.New("open Linux seccomp listener")
	}
	return listener, nil
}

func runLinuxAcceptanceNetworkSupervisor(listener *os.File, definitions []linuxRelayDefinition, stop <-chan struct{}) error {
	if listener == nil {
		return errors.New("Linux acceptance network confinement unavailable")
	}
	allowed := make(map[int]bool, len(definitions))
	for _, definition := range definitions {
		allowed[definition.Port] = true
	}
	fd := int(listener.Fd())
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		count, err := unix.Poll(poll, linuxAcceptanceNotificationPollMillis)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if count == 0 {
			continue
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			select {
			case <-stop:
				return nil
			default:
				return errors.New("Linux seccomp listener failed")
			}
		}
		var notification linuxSeccompNotification
		if err := linuxSeccompIoctl(fd, unix.SECCOMP_IOCTL_NOTIF_RECV, unsafe.Pointer(&notification)); err != nil {
			if err == unix.EINTR || err == unix.ENOENT {
				continue
			}
			return err
		}
		if err := validateLinuxSeccompNotification(fd, notification.ID); err != nil {
			if err == unix.ENOENT {
				continue
			}
			return err
		}
		response, fatal := handleLinuxAcceptanceNetworkNotification(fd, notification, allowed)
		if fatal == unix.ENOENT {
			continue
		}
		if err := linuxSeccompIoctl(fd, unix.SECCOMP_IOCTL_NOTIF_SEND, unsafe.Pointer(&response)); err != nil && err != unix.ENOENT {
			return err
		}
		if fatal != nil {
			return fatal
		}
	}
}

func handleLinuxAcceptanceNetworkNotification(listenerFD int, notification linuxSeccompNotification, allowed map[int]bool) (linuxSeccompNotificationResponse, error) {
	response := linuxSeccompNotificationResponse{ID: notification.ID, Error: -int32(unix.EPERM)}
	switch uintptr(notification.Data.Number) {
	case unix.SYS_SOCKET:
		domain := int(notification.Data.Arguments[0])
		typeAndFlags := int(notification.Data.Arguments[1])
		protocol := int(notification.Data.Arguments[2])
		if (domain == unix.AF_INET || domain == unix.AF_INET6) && typeAndFlags&linuxSocketTypeMask == unix.SOCK_STREAM && (protocol == 0 || protocol == unix.IPPROTO_TCP) {
			response.Error = 0
			response.Flags = unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
		}
		return response, nil
	case unix.SYS_CONNECT:
		return brokerLinuxAcceptanceConnect(listenerFD, notification, allowed)
	case unix.SYS_SENDTO:
		if notification.Data.Arguments[3]&unix.MSG_FASTOPEN == 0 {
			response.Error = 0
			response.Flags = unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
		}
		return response, nil
	case unix.SYS_SENDMSG:
		if notification.Data.Arguments[2]&unix.MSG_FASTOPEN == 0 {
			response.Error = 0
			response.Flags = unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
		}
		return response, nil
	case unix.SYS_SENDMMSG:
		if notification.Data.Arguments[3]&unix.MSG_FASTOPEN == 0 {
			response.Error = 0
			response.Flags = unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
		}
		return response, nil
	case unix.SYS_PRCTL:
		if notification.Data.Arguments[0] != unix.PR_SET_SECCOMP {
			response.Error = 0
			response.Flags = unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
		}
		return response, nil
	default:
		return response, nil
	}
}

func brokerLinuxAcceptanceConnect(listenerFD int, notification linuxSeccompNotification, allowed map[int]bool) (linuxSeccompNotificationResponse, error) {
	response := linuxSeccompNotificationResponse{ID: notification.ID, Error: -int32(unix.EPERM)}
	length := notification.Data.Arguments[2]
	if length < linuxAcceptanceSockaddrInet4Size || length > 128 {
		return response, nil
	}
	encoded, err := readLinuxAcceptanceMemory(int(notification.PID), notification.Data.Arguments[1], int(length))
	if err != nil {
		return response, err
	}
	target, err := linuxAcceptanceConnectTarget(encoded, allowed)
	if err != nil {
		return response, nil
	}
	if err := validateLinuxSeccompNotification(listenerFD, notification.ID); err != nil {
		return response, err
	}
	duplicate, err := duplicateLinuxAcceptanceDescriptor(listenerFD, notification.ID, int(notification.PID), int(notification.Data.Arguments[0]))
	if err != nil {
		return response, err
	}
	defer unix.Close(duplicate)
	typeValue, err := unix.GetsockoptInt(duplicate, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || typeValue != unix.SOCK_STREAM {
		return response, errors.New("invalid Linux acceptance socket")
	}
	if err := validateLinuxSeccompNotification(listenerFD, notification.ID); err != nil {
		return response, err
	}
	if err := unix.Connect(duplicate, target); err != nil {
		if errno, ok := err.(syscall.Errno); ok {
			response.Error = -int32(errno)
			return response, nil
		}
		return response, err
	}
	response.Error = 0
	return response, nil
}

func linuxAcceptanceConnectTarget(encoded []byte, allowed map[int]bool) (unix.Sockaddr, error) {
	if len(encoded) < linuxAcceptanceSockaddrInet4Size || binary.NativeEndian.Uint16(encoded[0:2]) != unix.AF_INET {
		return nil, errors.New("Linux acceptance target unavailable")
	}
	port := int(binary.BigEndian.Uint16(encoded[2:4]))
	address := [4]byte{encoded[4], encoded[5], encoded[6], encoded[7]}
	if address != [4]byte{127, 0, 0, 1} || !allowed[port] {
		return nil, errors.New("Linux acceptance target unavailable")
	}
	return &unix.SockaddrInet4{Port: port, Addr: address}, nil
}

func readLinuxAcceptanceMemory(pid int, address uint64, length int) ([]byte, error) {
	if pid <= 1 || address == 0 || length < 1 || length > 128 {
		return nil, errors.New("invalid Linux acceptance memory request")
	}
	buffer := make([]byte, length)
	local := linuxAcceptanceIovec{Base: uintptr(unsafe.Pointer(&buffer[0])), Len: uintptr(length)}
	remote := linuxAcceptanceIovec{Base: uintptr(address), Len: uintptr(length)}
	count, _, errno := unix.Syscall6(
		unix.SYS_PROCESS_VM_READV,
		uintptr(pid),
		uintptr(unsafe.Pointer(&local)),
		1,
		uintptr(unsafe.Pointer(&remote)),
		1,
		0,
	)
	runtime.KeepAlive(buffer)
	if errno != 0 || int(count) != length {
		return nil, errors.New("read Linux acceptance network target")
	}
	return buffer, nil
}

func duplicateLinuxAcceptanceDescriptor(listenerFD int, notificationID uint64, pid, fd int) (int, error) {
	threadGroup, err := linuxAcceptanceThreadGroupID(pid)
	if err != nil {
		return -1, err
	}
	if err := validateLinuxSeccompNotification(listenerFD, notificationID); err != nil {
		return -1, err
	}
	pidfd, _, errno := unix.Syscall(unix.SYS_PIDFD_OPEN, uintptr(threadGroup), 0, 0)
	if errno != 0 {
		return -1, errors.New("open Linux acceptance process")
	}
	defer unix.Close(int(pidfd))
	if err := validateLinuxSeccompNotification(listenerFD, notificationID); err != nil {
		return -1, err
	}
	duplicate, _, errno := unix.Syscall(unix.SYS_PIDFD_GETFD, pidfd, uintptr(fd), 0)
	if errno != 0 {
		return -1, errors.New("duplicate Linux acceptance descriptor")
	}
	if err := validateLinuxSeccompNotification(listenerFD, notificationID); err != nil {
		_ = unix.Close(int(duplicate))
		return -1, err
	}
	return int(duplicate), nil
}

func linuxAcceptanceThreadGroupID(pid int) (int, error) {
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, errors.New("inspect Linux acceptance process")
	}
	for _, line := range strings.Split(string(content), "\n") {
		if !strings.HasPrefix(line, "Tgid:") {
			continue
		}
		value, parseErr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Tgid:")))
		if parseErr != nil || value <= 1 {
			break
		}
		return value, nil
	}
	return 0, errors.New("inspect Linux acceptance process")
}

func linuxSeccompIoctl(fd int, request uintptr, argument unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(argument))
	if errno != 0 {
		return errno
	}
	return nil
}

func validateLinuxSeccompNotification(fd int, id uint64) error {
	return linuxSeccompIoctl(fd, unix.SECCOMP_IOCTL_NOTIF_ID_VALID, unsafe.Pointer(&id))
}
