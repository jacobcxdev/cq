//go:build windows

package dots

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestWindowsGatewayTCPPeerRequiresExactConnection(t *testing.T) {
	row := make([]byte, windowsGatewayTCPRowSize)
	binary.LittleEndian.PutUint32(row[0:4], 5)
	copy(row[4:8], []byte{127, 0, 0, 1})
	binary.BigEndian.PutUint16(row[8:10], 50123)
	copy(row[12:16], []byte{127, 0, 0, 1})
	binary.BigEndian.PutUint16(row[16:18], 8000)
	binary.LittleEndian.PutUint32(row[20:24], 42)
	table := append([]byte{1, 0, 0, 0}, row...)
	if pid, ok := windowsGatewayPeerPID("127.0.0.1:50123", table); !ok || pid != 42 {
		t.Fatalf("matching Windows TCP owner = %d, %v", pid, ok)
	}
	for _, test := range []struct {
		name   string
		remote string
		table  []byte
	}{
		{"different-port", "127.0.0.1:50124", table},
		{"different-address", "127.0.0.2:50123", table},
		{"truncated-row", "127.0.0.1:50123", table[:len(table)-1]},
		{"ambiguous-pid", "127.0.0.1:50123", append(append([]byte{2, 0, 0, 0}, row...), func() []byte {
			other := append([]byte(nil), row...)
			binary.LittleEndian.PutUint32(other[20:24], 43)
			return other
		}()...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if pid, ok := windowsGatewayPeerPID(test.remote, test.table); ok || pid != 0 {
				t.Fatalf("unsafe Windows TCP owner = %d, %v", pid, ok)
			}
		})
	}
}

func TestWindowsGatewayTCPTableFindsLiveCurrentProcess(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	table, err := windowsGatewayTCPTable()
	if err != nil {
		t.Fatal(err)
	}
	address, err := netip.ParseAddrPort(server.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	pid, ok := windowsGatewayConnectionPID(server.RemoteAddr().String(), address, table)
	if !ok || pid != os.Getpid() {
		t.Fatalf("live TCP owner = %d, %v; want current process", pid, ok)
	}
}

func TestWindowsCloudGatewayPrivatePipeFirstAndRepeat(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	root := filepath.Join(anchor, "dots", "runtime")
	if err := PrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	path, _, err := dotsPipePath(root, "cloud-gateway-"+strconv.Itoa(pid))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		listener, err := newWindowsCloudGatewayListener(root, pid)
		if err != nil {
			t.Fatal(err)
		}
		if other, err := newWindowsCloudGatewayListener(root, pid); err == nil {
			_ = other.Close()
			_ = listener.Close()
			t.Fatal("gateway pipe replaced live listener")
		}
		done := make(chan net.Conn, 1)
		go func() {
			defer func() { _ = recover() }()
			c, err := listener.Accept()
			if err != nil {
				done <- nil
				return
			}
			done <- c
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		client, err := winio.DialPipeContext(ctx, path)
		cancel()
		if err != nil {
			_ = listener.Close()
			t.Fatal(err)
		}
		if !dotsPipePeer(client, false) {
			_ = client.Close()
			_ = listener.Close()
			t.Fatal("gateway server peer SID mismatch")
		}
		server := <-done
		if server == nil {
			_ = client.Close()
			_ = listener.Close()
			t.Fatal("gateway rejected current-user client")
		}
		_ = server.Close()
		_ = client.Close()
		_ = listener.Close()
	}
}

func TestWindowsCloudGatewayLaunchFailureReapsStockChild(t *testing.T) {
	stopped, waited, callback := false, false, false
	deps := cloudStdioDependencies{
		localExecutable: "fixture",
		launch: func(context.Context, string, []string, []string, io.Writer) (*cloudLocal, error) {
			return &cloudLocal{pid: 54321, input: cloudTestWriter{write: func(raw []byte) (int, error) { return len(raw), nil }}, output: io.NopCloser(strings.NewReader("")), stop: func() { stopped = true }, wait: func() error { waited = true; return nil }}, nil
		},
		gatewayListenerForPID: func(pid int) (net.Listener, error) {
			callback = pid == 54321
			return nil, errors.New("isolated gateway listener unavailable")
		},
	}
	err := runCloudStdio(context.Background(), &Settings{}, nil, nil, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !callback || !stopped || !waited {
		t.Fatalf("gateway launch failure: err=%v callback=%v stopped=%v waited=%v", err, callback, stopped, waited)
	}
}
