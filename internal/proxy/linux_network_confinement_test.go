//go:build linux

package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxAcceptanceNetworkAllowsOnlyTCPSockets(t *testing.T) {
	tests := []struct {
		name     string
		domain   int
		typeFlag int
		protocol int
		want     bool
	}{
		{name: "IPv4 TCP", domain: unix.AF_INET, typeFlag: unix.SOCK_STREAM, protocol: unix.IPPROTO_TCP, want: true},
		{name: "IPv6 default protocol", domain: unix.AF_INET6, typeFlag: unix.SOCK_STREAM | unix.SOCK_CLOEXEC, want: true},
		{name: "IPv4 UDP", domain: unix.AF_INET, typeFlag: unix.SOCK_DGRAM, protocol: unix.IPPROTO_UDP},
		{name: "Unix stream", domain: unix.AF_UNIX, typeFlag: unix.SOCK_STREAM},
		{name: "IPv4 raw", domain: unix.AF_INET, typeFlag: unix.SOCK_RAW},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			notification := linuxSeccompNotification{Data: linuxSeccompData{
				Number: int32(unix.SYS_SOCKET),
				Arguments: [6]uint64{
					uint64(test.domain),
					uint64(test.typeFlag),
					uint64(test.protocol),
				},
			}}
			response, err := handleLinuxAcceptanceNetworkNotification(-1, notification, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := response.Error == 0 && response.Flags == unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
			if got != test.want {
				t.Fatalf("socket allowed = %v, want %v; response = %#v", got, test.want, response)
			}
			if !test.want && response.Error != -int32(syscall.EPERM) {
				t.Fatalf("socket errno = %d, want EPERM", response.Error)
			}
		})
	}
}

func TestLinuxAcceptanceNetworkPreventsFilterReplacement(t *testing.T) {
	tests := []struct {
		name     string
		syscall  int32
		argument uint64
		want     bool
	}{
		{name: "seccomp syscall", syscall: int32(unix.SYS_SECCOMP)},
		{name: "prctl seccomp", syscall: int32(unix.SYS_PRCTL), argument: unix.PR_SET_SECCOMP},
		{name: "unrelated prctl", syscall: int32(unix.SYS_PRCTL), argument: unix.PR_GET_DUMPABLE, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			notification := linuxSeccompNotification{Data: linuxSeccompData{
				Number:    test.syscall,
				Arguments: [6]uint64{test.argument},
			}}
			response, err := handleLinuxAcceptanceNetworkNotification(-1, notification, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := response.Error == 0 && response.Flags == unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
			if got != test.want {
				t.Fatalf("operation allowed = %v, want %v; response = %#v", got, test.want, response)
			}
		})
	}
}

func TestLinuxAcceptanceNetworkPreventsProcessGroupEscape(t *testing.T) {
	for _, number := range []int32{int32(unix.SYS_SETPGID), int32(unix.SYS_SETSID)} {
		response, err := handleLinuxAcceptanceNetworkNotification(-1, linuxSeccompNotification{
			Data: linuxSeccompData{Number: number},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if response.Error != -int32(syscall.EPERM) || response.Flags != 0 {
			t.Fatalf("syscall %d response = %#v, want EPERM", number, response)
		}
	}
}

func TestLinuxAcceptanceNetworkRestrictsFastOpen(t *testing.T) {
	tests := []struct {
		name      string
		syscall   int32
		flagsSlot int
		flags     uint64
		want      bool
	}{
		{name: "sendto fast open", syscall: int32(unix.SYS_SENDTO), flagsSlot: 3, flags: unix.MSG_FASTOPEN},
		{name: "sendto ordinary", syscall: int32(unix.SYS_SENDTO), flagsSlot: 3, want: true},
		{name: "sendmsg fast open", syscall: int32(unix.SYS_SENDMSG), flagsSlot: 2, flags: unix.MSG_FASTOPEN},
		{name: "sendmsg ordinary", syscall: int32(unix.SYS_SENDMSG), flagsSlot: 2, want: true},
		{name: "sendmmsg fast open", syscall: int32(unix.SYS_SENDMMSG), flagsSlot: 3, flags: unix.MSG_FASTOPEN},
		{name: "sendmmsg ordinary", syscall: int32(unix.SYS_SENDMMSG), flagsSlot: 3, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var arguments [6]uint64
			arguments[test.flagsSlot] = test.flags
			response, err := handleLinuxAcceptanceNetworkNotification(-1, linuxSeccompNotification{
				Data: linuxSeccompData{Number: test.syscall, Arguments: arguments},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := response.Error == 0 && response.Flags == unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
			if got != test.want {
				t.Fatalf("operation allowed = %v, want %v; response = %#v", got, test.want, response)
			}
			if !test.want && response.Error != -int32(syscall.EPERM) {
				t.Fatalf("response = %#v, want EPERM", response)
			}
		})
	}
}

func TestLinuxAcceptanceNetworkSupervisorPanicFailsClosed(t *testing.T) {
	err := runLinuxAcceptanceNetworkSupervisorSafely(func() error {
		panic("external supervisor panic")
	})
	if err == nil || err.Error() != "Linux acceptance network supervisor panic" {
		t.Fatalf("supervisor panic error = %v", err)
	}
}

func TestLinuxAcceptanceNetworkSupervisorCancelledStartupCleansUp(t *testing.T) {
	target := make(chan *os.File)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, ok := <-target
		if ok {
			done <- errors.New("unexpected listener")
			return
		}
		done <- nil
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitLinuxAcceptanceNetworkSupervisorReady(ctx, target, ready, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("startup error = %v, want context cancellation", err)
	}
}

func TestLinuxAcceptanceConnectTargetAllowsConfiguredLoopbackOnly(t *testing.T) {
	allowed := map[int]bool{19431: true}
	tests := []struct {
		name    string
		address [16]byte
		wantErr bool
	}{
		{name: "configured loopback", address: linuxSockaddrInet4(19431, [4]byte{127, 0, 0, 1})},
		{name: "external same port", address: linuxSockaddrInet4(19431, [4]byte{192, 0, 2, 1}), wantErr: true},
		{name: "unconfigured loopback", address: linuxSockaddrInet4(19432, [4]byte{127, 0, 0, 1}), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, err := linuxAcceptanceConnectTarget(test.address[:], allowed)
			if (err != nil) != test.wantErr {
				t.Fatalf("target error = %v, want error %v", err, test.wantErr)
			}
			if !test.wantErr {
				address, ok := target.(*unix.SockaddrInet4)
				if !ok || address.Port != 19431 || address.Addr != [4]byte{127, 0, 0, 1} {
					t.Fatalf("target = %#v", target)
				}
			}
		})
	}
}

func linuxSockaddrInet4(port int, address [4]byte) [16]byte {
	var encoded [16]byte
	binary.NativeEndian.PutUint16(encoded[0:2], unix.AF_INET)
	binary.BigEndian.PutUint16(encoded[2:4], uint16(port))
	copy(encoded[4:8], address[:])
	return encoded
}
