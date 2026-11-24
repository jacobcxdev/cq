//go:build windows

package dots

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const windowsGatewayTCPRowSize = 24

var getExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

type windowsCloudGatewayListener struct{ net.Listener }

func (l *windowsCloudGatewayListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if dotsPipePeer(connection, true) {
			return connection, nil
		}
		_ = connection.Close()
	}
}

func newWindowsCloudGatewayListener(runtimeRoot string, pid int) (net.Listener, error) {
	if pid <= 1 || !cleanAbsolute(runtimeRoot) {
		return nil, errors.New("native gateway identity invalid")
	}
	if err := privateDirectory(runtimeRoot); err != nil {
		return nil, err
	}
	path, sid, err := dotsPipePath(runtimeRoot, "cloud-gateway-"+strconv.Itoa(pid))
	if err != nil {
		return nil, err
	}
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: dotsPipeSDDL(sid), InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
	if err != nil {
		return nil, errors.New("native gateway listener unavailable")
	}
	return &windowsCloudGatewayListener{Listener: listener}, nil
}

func windowsGatewayTCPTable() ([]byte, error) {
	var size uint32
	result, _, _ := getExtendedTCPTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, windows.AF_INET, 4, 0)
	if result != 0 && result != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
		return nil, errors.New("native gateway TCP owner table unavailable")
	}
	if size < 4 || size > 4<<20 {
		return nil, errors.New("native gateway TCP owner table invalid")
	}
	table := make([]byte, size)
	result, _, _ = getExtendedTCPTable.Call(uintptr(unsafe.Pointer(&table[0])), uintptr(unsafe.Pointer(&size)), 0, windows.AF_INET, 4, 0)
	if result != 0 || size < 4 || int(size) > len(table) {
		return nil, errors.New("native gateway TCP owner table unavailable")
	}
	return table[:size], nil
}

func windowsGatewayPeerPID(remoteAddress string, table []byte) (int, bool) {
	return windowsGatewayConnectionPID(remoteAddress, netip.MustParseAddrPort("127.0.0.1:8000"), table)
}

func windowsGatewayConnectionPID(remoteAddress string, server netip.AddrPort, table []byte) (int, bool) {
	remote, err := netip.ParseAddrPort(remoteAddress)
	if err != nil || remote.Addr() != netip.MustParseAddr("127.0.0.1") || remote.Port() == 0 || remote.Port() == server.Port() || server.Addr() != netip.MustParseAddr("127.0.0.1") || len(table) < 4 {
		return 0, false
	}
	count := binary.LittleEndian.Uint32(table[:4])
	if count > uint32((len(table)-4)/windowsGatewayTCPRowSize) {
		return 0, false
	}
	var match int
	for index := uint32(0); index < count; index++ {
		row := table[4+int(index)*windowsGatewayTCPRowSize:][:windowsGatewayTCPRowSize]
		if binary.LittleEndian.Uint32(row[:4]) != 5 ||
			netip.AddrFrom4([4]byte(row[4:8])) != remote.Addr() ||
			binary.BigEndian.Uint16(row[8:10]) != remote.Port() ||
			netip.AddrFrom4([4]byte(row[12:16])) != server.Addr() ||
			binary.BigEndian.Uint16(row[16:18]) != server.Port() {
			continue
		}
		pid := int(binary.LittleEndian.Uint32(row[20:24]))
		if pid <= 1 || match != 0 && match != pid {
			return 0, false
		}
		match = pid
	}
	return match, match > 1
}

func nativeCloudGatewayPeer(ctx context.Context, cfg *Settings, remote string) (int, bool) {
	if err := ctx.Err(); err != nil || cfg == nil {
		return 0, false
	}
	table, err := windowsGatewayTCPTable()
	if err != nil {
		return 0, false
	}
	pid, ok := windowsGatewayPeerPID(remote, table)
	if !ok {
		return 0, false
	}
	_, arguments, err := dotsProcessArguments(ctx, pid)
	if err != nil || len(arguments) == 0 || !strings.EqualFold(arguments[0], cloudStockExecutable(cfg)) || !windowsCloudAppSession(arguments[1:]) {
		return 0, false
	}
	return pid, true
}

func cloudBridgeGateway(*Settings) (net.Listener, error) {
	return nil, errors.New("Windows native gateway is attached to the signed cloud wrapper")
}

func windowsNativeCloudGateway(p *Probe, w http.ResponseWriter, r *http.Request) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	pid, ok := nativeCloudGatewayPeer(ctx, &p.settings, r.RemoteAddr)
	if !ok {
		return false
	}
	if err := privateDirectory(p.settings.RuntimeRoot); err != nil {
		return false
	}
	path, _, err := dotsPipePath(p.settings.RuntimeRoot, "cloud-gateway-"+strconv.Itoa(pid))
	if err != nil {
		return false
	}
	upstream, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return false
	}
	defer upstream.Close()
	if !dotsPipePeer(upstream, false) {
		return false
	}
	_ = upstream.SetDeadline(time.Now().Add(25 * time.Second))
	relayCloudGateway(w, r, upstream, p.event)
	return true
}
