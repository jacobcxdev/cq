//go:build linux

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const linuxAcceptanceClientMarker = "CQ_LINUX_ACCEPTANCE_CONFINED"

type linuxAcceptanceMmsghdr struct {
	Header unix.Msghdr
	Length uint32
}

func TestLinuxAcceptanceConfinementUsesNamespacesRelaysAndLandlock(t *testing.T) {
	helperPath := filepath.Join(t.TempDir(), "cq")
	build := exec.Command("go", "build", "-o", helperPath, "./cmd/cq")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Linux acceptance helper: %v\n%s", err, output)
	}
	oldOpen := openLinuxAcceptanceHelper
	openLinuxAcceptanceHelper = func() (*os.File, error) { return os.Open(helperPath) }
	t.Cleanup(func() { openLinuxAcceptanceHelper = oldOpen })
	if !probeLinuxCandidateConfinement() {
		t.Fatal("full Linux candidate confinement probe failed")
	}

	proxyListener, proxyServer, proxyErrors, err := startCodexAcceptanceHTTP(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "proxy-ok")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownCodexAcceptanceServer(proxyServer)
	egressListener, egressServer, egressErrors, err := startCodexAcceptanceHTTP(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "egress-ok")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownCodexAcceptanceServer(egressServer)

	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(outside, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://" + proxyListener.Addr().String()
	egressURL := "http://" + egressListener.Addr().String()
	execution := codexAcceptanceExecution{
		executable: os.Args[0],
		args:       []string{"-test.run=^TestLinuxAcceptanceConfinementClientProcess$", "-test.v"},
		command: codexAcceptanceCommand{
			env: []string{
				"CQ_LINUX_ACCEPTANCE_CLIENT=1",
				"CQ_LINUX_ACCEPTANCE_ALLOWED=" + allowed,
				"CQ_LINUX_ACCEPTANCE_OUTSIDE=" + outside,
				"CQ_LINUX_ACCEPTANCE_PROXY=" + proxyURL,
				"CQ_LINUX_ACCEPTANCE_EGRESS=" + egressURL,
				"CQ_LINUX_ACCEPTANCE_ALLOWED_PORT=" + strings.TrimPrefix(proxyListener.Addr().String(), "127.0.0.1:"),
			},
			dir:              root,
			endpoint:         proxyURL + "/responses",
			egressProxyURL:   egressURL,
			sandboxWriteRoot: root,
			captureOutput:    true,
			loopbackOnly:     true,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := linuxCodexAcceptanceConfinement{}.Execute(ctx, execution)
	if err != nil {
		t.Fatalf("confined execution: %v", err)
	}
	if !bytes.Contains(output, []byte(linuxAcceptanceClientMarker)) {
		t.Fatalf("client output missing marker: %q", output)
	}
	if content, err := os.ReadFile(allowed); err != nil || string(content) != "allowed" {
		t.Fatalf("allowed write = %q, %v", content, err)
	}
	if content, err := os.ReadFile(outside); err != nil || string(content) != "preserve" {
		t.Fatalf("outside write changed = %q, %v", content, err)
	}
	for _, serverErrors := range []<-chan error{proxyErrors, egressErrors} {
		if err := codexAcceptanceServeError(serverErrors); err != nil {
			t.Fatalf("relay server error: %v", err)
		}
	}
}

func TestLinuxAcceptanceConfinementPreservesHelperStartError(t *testing.T) {
	oldStart := startLinuxAcceptanceCommand
	startLinuxAcceptanceCommand = func(*exec.Cmd) error { return syscall.EPERM }
	t.Cleanup(func() { startLinuxAcceptanceCommand = oldStart })
	root := t.TempDir()
	_, err := (linuxCodexAcceptanceConfinement{}).Execute(context.Background(), codexAcceptanceExecution{
		executable: "/bin/true",
		command: codexAcceptanceCommand{
			dir: root, sandboxWriteRoot: root, loopbackOnly: true,
		},
	})
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("helper start error = %v", err)
	}
}

func TestLinuxAcceptanceConfinementClientProcess(t *testing.T) {
	if os.Getenv("CQ_LINUX_ACCEPTANCE_CLIENT") != "1" {
		return
	}
	allowed := os.Getenv("CQ_LINUX_ACCEPTANCE_ALLOWED")
	outside := os.Getenv("CQ_LINUX_ACCEPTANCE_OUTSIDE")
	if err := os.WriteFile(allowed, []byte("allowed"), 0o600); err != nil {
		t.Fatalf("allowed write: %v", err)
	}
	if err := os.WriteFile(outside, []byte("changed"), 0o600); err == nil {
		t.Fatal("external write succeeded")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	for _, variable := range []string{"CQ_LINUX_ACCEPTANCE_PROXY", "CQ_LINUX_ACCEPTANCE_EGRESS"} {
		response, err := client.Get(os.Getenv(variable))
		if err != nil {
			t.Fatalf("relay %s: %v", variable, err)
		}
		content, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || !strings.HasSuffix(string(content), "-ok") {
			t.Fatalf("relay %s response = %q, %v", variable, content, readErr)
		}
	}
	for _, address := range []string{"192.0.2.1:80", "192.0.2.1:" + os.Getenv("CQ_LINUX_ACCEPTANCE_ALLOWED_PORT")} {
		connection, err := net.DialTimeout("tcp4", address, 500*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			t.Fatalf("direct network connection succeeded: %s", address)
		}
		if !errors.Is(err, syscall.EPERM) {
			t.Fatalf("direct network connection error = %v, want EPERM: %s", err, address)
		}
	}
	if fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP); err == nil {
		_ = unix.Close(fd)
		t.Fatal("UDP socket creation succeeded")
	} else if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("UDP socket error = %v, want EPERM", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_STRICT, 0, 0); errno != syscall.EPERM {
		t.Fatalf("seccomp replacement errno = %v, want EPERM", errno)
	}
	allowedPort, err := strconv.Atoi(os.Getenv("CQ_LINUX_ACCEPTANCE_ALLOWED_PORT"))
	if err != nil {
		t.Fatalf("parse allowed port: %v", err)
	}
	for _, target := range []*unix.SockaddrInet4{
		{Port: 80, Addr: [4]byte{192, 0, 2, 1}},
		{Port: allowedPort + 1, Addr: [4]byte{127, 0, 0, 1}},
	} {
		verifyLinuxAcceptanceFastOpenDenied(t, target)
	}
	verifyLinuxAcceptanceOrdinarySends(t, allowedPort)
	fmt.Println(linuxAcceptanceClientMarker)
}

func verifyLinuxAcceptanceFastOpenDenied(t *testing.T, target *unix.SockaddrInet4) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatalf("create TCP Fast Open socket: %v", err)
	}
	defer unix.Close(fd)
	if err := unix.Sendto(fd, []byte("blocked"), unix.MSG_FASTOPEN, target); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("TCP Fast Open sendto error = %v, want EPERM", err)
	}
	if _, err := unix.SendmsgN(fd, []byte("blocked"), nil, target, unix.MSG_FASTOPEN); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("TCP Fast Open sendmsg error = %v, want EPERM", err)
	}
	message, keepAlive := linuxAcceptanceSendmmsg([]byte("blocked"), target)
	if _, _, errno := unix.Syscall6(unix.SYS_SENDMMSG, uintptr(fd), uintptr(unsafe.Pointer(&message)), 1, unix.MSG_FASTOPEN, 0, 0); errno != syscall.EPERM {
		t.Fatalf("TCP Fast Open sendmmsg errno = %v, want EPERM", errno)
	}
	runtime.KeepAlive(keepAlive)
}

func verifyLinuxAcceptanceOrdinarySends(t *testing.T, port int) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatalf("create ordinary TCP socket: %v", err)
	}
	defer unix.Close(fd)
	if err := unix.Connect(fd, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("connect ordinary TCP socket: %v", err)
	}
	if err := unix.Sendto(fd, []byte("GET /"), 0, nil); err != nil {
		t.Fatalf("ordinary sendto: %v", err)
	}
	if _, err := unix.SendmsgN(fd, []byte(" HTTP/1.0\r\n"), nil, nil, 0); err != nil {
		t.Fatalf("ordinary sendmsg: %v", err)
	}
	message, keepAlive := linuxAcceptanceSendmmsg([]byte("Host: localhost\r\n\r\n"), nil)
	if _, _, errno := unix.Syscall6(unix.SYS_SENDMMSG, uintptr(fd), uintptr(unsafe.Pointer(&message)), 1, 0, 0, 0); errno != 0 {
		t.Fatalf("ordinary sendmmsg: %v", errno)
	}
	runtime.KeepAlive(keepAlive)
	buffer := make([]byte, 4096)
	count, err := unix.Read(fd, buffer)
	if err != nil || !bytes.Contains(buffer[:count], []byte("proxy-ok")) {
		t.Fatalf("ordinary send response = %q, %v", buffer[:count], err)
	}
}

func linuxAcceptanceSendmmsg(payload []byte, target *unix.SockaddrInet4) (linuxAcceptanceMmsghdr, [][]byte) {
	iovec := unix.Iovec{Base: &payload[0]}
	iovec.SetLen(len(payload))
	message := linuxAcceptanceMmsghdr{Header: unix.Msghdr{Iov: &iovec}}
	message.Header.SetIovlen(1)
	keepAlive := [][]byte{payload}
	if target != nil {
		encoded := linuxSockaddrInet4(target.Port, target.Addr)
		message.Header.Name = &encoded[0]
		message.Header.Namelen = uint32(len(encoded))
		keepAlive = append(keepAlive, encoded[:])
	}
	return message, keepAlive
}
