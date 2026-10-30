package proxy

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
	wgconn "golang.zx2c4.com/wireguard/conn"
)

func newInterfaceWireGuardBind(name string) (wgconn.Bind, error) {
	return newInterfaceWireGuardBindWithDependencies(name, interfaceBindDependencies{
		lookup: net.InterfaceByName,
		listen: listenInterfacePacket,
		control: func(index int) interfaceSocketControl {
			return darwinInterfaceSocketControl(index, unix.SetsockoptInt, unix.GetsockoptInt)
		},
	})
}

func darwinInterfaceSocketControl(index int, set func(int, int, int, int) error, get func(int, int, int) (int, error)) interfaceSocketControl {
	return func(network, _ string, raw syscall.RawConn) error {
		if index <= 0 {
			// Zero removes Darwin's interface binding.
			return errors.New("codex_egress_interface: invalid interface index")
		}
		level, option := unix.IPPROTO_IP, unix.IP_BOUND_IF
		switch network {
		case "udp4":
		case "udp6":
			level, option = unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF
		default:
			return errors.New("codex_egress_interface: unsupported socket family")
		}
		var socketErr error
		err := raw.Control(func(fd uintptr) {
			if err := set(int(fd), level, option, index); err != nil {
				socketErr = err
				return
			}
			actual, err := get(int(fd), level, option)
			if err != nil || actual != index {
				socketErr = errors.New("interface socket binding was not retained")
			}
		})
		if err != nil || socketErr != nil {
			return errors.New("codex_egress_interface: cannot bind WireGuard UDP socket to the selected interface")
		}
		return nil
	}
}
