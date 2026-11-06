package dots

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// Each stock app-server keeps its PID through exec. Its sibling stdio broker
// owns this listener, so a gateway connection reaches that same bridge.
type cloudGatewayListener struct {
	*net.UnixListener
	path     string
	identity os.FileInfo
	once     sync.Once
	closeErr error
}

func cloudGatewayPath(runtimeRoot string, pid int, create bool) (string, error) {
	if !cleanAbsolute(runtimeRoot) || pid <= 1 {
		return "", errors.New("native gateway identity invalid")
	}
	if err := privateDirectory(runtimeRoot); err != nil {
		return "", err
	}
	root := filepath.Join(runtimeRoot, "cloud-gateway")
	name := "gateway-" + strconv.Itoa(pid) + ".sock"
	if len(filepath.Join(root, name)) > 103 {
		digest := sha256.Sum256([]byte(runtimeRoot))
		// Probe and native app environments can have different TMPDIR values.
		root = filepath.Join("/tmp", fmt.Sprintf("cq-gateway-%d-%x", os.Getuid(), digest[:10]))
	}
	if create {
		if err := PrivateDirectory(root); err != nil {
			return "", err
		}
	} else if err := privateDirectory(root); err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

func cloudGatewaySocket(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || !owned(info) || info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o600 {
		return nil, errors.New("native gateway socket must be owned and mode 0600")
	}
	return info, nil
}

func newCloudGatewayListener(runtimeRoot string, pid int) (*cloudGatewayListener, error) {
	path, err := cloudGatewayPath(runtimeRoot, pid, true)
	if err != nil {
		return nil, err
	}
	// Never unlink an existing pathname, even when a former owner appears gone.
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return nil, errors.New("native gateway socket already exists or is unavailable")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("native gateway listener unavailable")
	}
	listener.SetUnlinkOnClose(false)
	result := &cloudGatewayListener{UnixListener: listener, path: path}
	result.identity, err = os.Lstat(path)
	if err == nil {
		err = os.Chmod(path, 0o600)
	}
	if err != nil {
		_ = result.Close()
		return nil, errors.New("native gateway socket could not be secured")
	}
	return result, nil
}

func importCloudGatewayListener(runtimeRoot string, pid int, file *os.File) (net.Listener, error) {
	path, err := cloudGatewayPath(runtimeRoot, pid, false)
	if err != nil {
		return nil, err
	}
	identity, err := cloudGatewaySocket(path)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, errors.New("native gateway descriptor unavailable")
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeSocket == 0 || !owned(info) {
		return nil, errors.New("native gateway descriptor invalid")
	}
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, errors.New("native gateway descriptor invalid")
	}
	local, ok := listener.(*net.UnixListener)
	if ok {
		local.SetUnlinkOnClose(false)
	}
	if !ok || local.Addr().String() != path {
		_ = listener.Close()
		return nil, errors.New("native gateway descriptor identity mismatch")
	}
	current, err := cloudGatewaySocket(path)
	if err != nil || !os.SameFile(identity, current) {
		_ = local.Close()
		return nil, errors.New("native gateway socket changed during handoff")
	}
	return &cloudGatewayListener{UnixListener: local, path: path, identity: identity}, nil
}

func (l *cloudGatewayListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := nativePeerUID(connection)
		if err == nil && uid == uint32(os.Getuid()) {
			return connection, nil
		}
		_ = connection.Close()
	}
}

func (l *cloudGatewayListener) Close() error {
	l.once.Do(func() {
		l.closeErr = l.UnixListener.Close()
		if info, err := os.Lstat(l.path); err == nil && l.identity != nil && os.SameFile(info, l.identity) {
			_ = os.Remove(l.path)
		}
	})
	return l.closeErr
}

func cloudGatewayPeerPID(remoteAddress string, data []byte) (int, bool) {
	remote, err := netip.ParseAddrPort(remoteAddress)
	if err != nil || !remote.Addr().IsLoopback() || remote.Port() == 0 || remote.Port() == 8000 {
		return 0, false
	}
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	pid, match := 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "p") {
			pid, _ = strconv.Atoi(line[1:])
			continue
		}
		if !strings.HasPrefix(line, "n") || pid <= 1 {
			continue
		}
		ends := strings.Split(line[1:], "->")
		if len(ends) != 2 {
			continue
		}
		local, localErr := netip.ParseAddrPort(ends[0])
		server, serverErr := netip.ParseAddrPort(ends[1])
		if localErr != nil || serverErr != nil {
			continue
		}
		local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
		server = netip.AddrPortFrom(server.Addr().Unmap(), server.Port())
		// RunProbe binds this exact address; a different loopback listener can
		// have the same client address and port without owning this request.
		if local != remote || server != netip.MustParseAddrPort("127.0.0.1:8000") {
			continue
		}
		if match != 0 && match != pid {
			return 0, false
		}
		match = pid
	}
	return match, match > 1
}

func cloudGatewayNativeProcess(cfg *Settings, uid int, arguments []string) bool {
	if cfg == nil || uid != os.Getuid() || len(arguments) == 0 || arguments[0] != filepath.Join(cfg.AppPath, "Contents", "Resources", "codex-cli", "CodexCLI.app", "Contents", "MacOS", "codex") {
		return false
	}
	appServer, appSession := false, false
	for _, argument := range arguments[1:] {
		appServer = appServer || argument == "app-server"
		appSession = appSession || argument == cloudAppMarker+"true" || argument == cloudAppMarker+"false"
	}
	return appServer && appSession
}

func (p *Probe) nativeCloudGateway(w http.ResponseWriter, r *http.Request) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	pid, ok := nativeCloudGatewayPeer(ctx, &p.settings, r.RemoteAddr)
	if !ok {
		return false
	}
	path, err := cloudGatewayPath(p.settings.RuntimeRoot, pid, false)
	if err != nil {
		return false
	}
	identity, err := cloudGatewaySocket(path)
	if err != nil {
		return false
	}
	upstream, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return false
	}
	defer upstream.Close()
	connection, ok := upstream.(*net.UnixConn)
	if !ok {
		return false
	}
	uid, err := nativePeerUID(connection)
	current, currentErr := cloudGatewaySocket(path)
	if err != nil || uid != uint32(os.Getuid()) || currentErr != nil || !os.SameFile(identity, current) {
		return false
	}
	relayCloudGateway(w, r, upstream, p.event)
	return true
}

// HTTP readers may already contain WebSocket bytes when the upgrade completes.
// Keep both buffered readers as the stream sources rather than their raw conns.
func relayCloudGateway(w http.ResponseWriter, r *http.Request, upstream net.Conn, event func(map[string]any)) {
	_ = upstream.SetDeadline(time.Now().Add(25 * time.Second))
	request := r.Clone(r.Context())
	request.URL = &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath}
	request.RequestURI, request.Body, request.ContentLength = "", nil, 0
	if err := request.Write(upstream); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	reader := bufio.NewReaderSize(upstream, 65536)
	var header []byte
	for len(header) < 65536 {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		header = append(header, line...)
		if len(header) > 65536 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if string(line) == "\r\n" {
			break
		}
	}
	reader = bufio.NewReaderSize(io.MultiReader(strings.NewReader(string(header)), reader), 65536)
	response, err := http.ReadResponse(reader, r)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		body, err := httputil.ReadBody(response.Body)
		// Stop the connection before Close can drain an invalid or oversized body.
		_ = upstream.Close()
		_ = response.Body.Close()
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		for name, values := range response.Header {
			w.Header()[name] = append([]string(nil), values...)
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
		if event != nil {
			event(map[string]any{"operation": "native_cloud_gateway_handoff", "status": response.StatusCode})
		}
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	client, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err := buffer.Write(header); err != nil {
		return
	}
	if err := buffer.Flush(); err != nil {
		return
	}
	if event != nil {
		event(map[string]any{"operation": "native_cloud_gateway_handoff", "status": response.StatusCode})
	}
	_ = upstream.SetDeadline(time.Time{})
	_ = client.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	copyStream := func(destination net.Conn, source io.Reader) {
		defer func() { _ = recover(); _ = destination.Close(); done <- struct{}{} }()
		_, _ = io.CopyBuffer(destination, source, make([]byte, 65536))
	}
	go copyStream(upstream, buffer.Reader)
	go copyStream(client, reader)
	select {
	case <-r.Context().Done():
	case <-done:
	}
	_ = upstream.Close()
	_ = client.Close()
	<-done
}
