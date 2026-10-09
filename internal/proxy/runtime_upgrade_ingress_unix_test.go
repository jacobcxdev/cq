//go:build darwin || linux

package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRuntimeUpgradeListenerBacklogSaturation(t *testing.T) {
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	listener := NewRuntimeUpgradeListener(tcp)
	defer listener.Close()
	raw, err := tcp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var listenErr error
	if err := raw.Control(func(fd uintptr) { listenErr = unix.Listen(int(fd), 1) }); err != nil || listenErr != nil {
		t.Fatalf("bounded backlog: %v, %v", err, listenErr)
	}
	if err := listener.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	connected, timedOut, reset := 0, 0, 0
	for range 12 {
		conn, err := net.DialTimeout("tcp", listener.Addr().String(), 30*time.Millisecond)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				timedOut++
			} else if errors.Is(err, unix.ECONNRESET) {
				reset++
			} else {
				t.Fatalf("unexpected saturation result: %v", err)
			}
			continue
		}
		defer conn.Close()
		connected++
	}
	if connected == 0 || timedOut+reset == 0 {
		t.Fatalf("expected retained connections and bounded saturation: connected=%d timed_out=%d reset=%d", connected, timedOut, reset)
	}
	t.Logf("backlog=1: connected=%d timed_out=%d reset=%d; capacity and overflow behaviour depend on kernel", connected, timedOut, reset)
}
