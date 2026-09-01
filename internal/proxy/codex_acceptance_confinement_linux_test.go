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
	"strings"
	"syscall"
	"testing"
	"time"
)

const linuxAcceptanceClientMarker = "CQ_LINUX_ACCEPTANCE_CONFINED"

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

func TestLinuxAcceptanceConfinementPreservesNamespaceStartError(t *testing.T) {
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
		t.Fatalf("namespace start error = %v", err)
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
	connection, err := net.DialTimeout("tcp4", "192.0.2.1:80", 500*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatal("direct network connection succeeded")
	}
	fmt.Println(linuxAcceptanceClientMarker)
}
