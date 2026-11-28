//go:build windows

package codex

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestWindowsCredentialControlHandshakeHonoursCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sock")
	pipePath, sid, err := windowsCredentialPipePath(path)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := winio.ListenPipe(pipePath, &winio.PipeConfig{SecurityDescriptor: windowsCredentialPipeSDDL(sid)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn)
	acceptErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		select {
		case accepted <- conn:
		case <-ctx.Done():
			_ = conn.Close()
		}
	}()
	result := make(chan error, 1)
	go func() {
		client, err := dialWindowsCredentialOwner(ctx, pipePath, 5*time.Second)
		if client != nil {
			_ = client.Close()
		}
		result <- err
	}()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("credential pipe did not accept a client")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handshake error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("credential handshake stayed blocked after its context was cancelled")
	}
}

func TestWindowsCredentialControlFirstOwnerSecondDelegates(t *testing.T) {
	coordinator := &CredentialCoordinator{}
	path := filepath.Join(t.TempDir(), "credential.sock")
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if !owner.Owner() {
		t.Fatal("first control did not own named pipe")
	}
	delegate, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer delegate.Close()
	if delegate.Owner() {
		t.Fatal("second control unexpectedly owned named pipe")
	}
	if err := delegate.client.Call("CredentialRPC.Ping", struct{}{}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCredentialPipePathIsStableAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sock")
	first, sid, err := windowsCredentialPipePath(path)
	if err != nil {
		t.Fatal(err)
	}
	second, secondSID, err := windowsCredentialPipePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || sid != secondSID || !strings.HasPrefix(first, `\\.\pipe\cq-credential-`) {
		t.Fatalf("pipe paths/SIDs = %q/%q %q/%q", first, sid, second, secondSID)
	}
	if strings.Contains(strings.ToLower(first), strings.ToLower(path)) || strings.Contains(first, sid) {
		t.Fatalf("pipe path leaked identity: %q", first)
	}
}

func TestWindowsCredentialPipeDACLExcludesAdministrators(t *testing.T) {
	const sid = "S-1-5-21-1-2-3-1001"
	want := "O:" + sid + "G:" + sid + "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)"
	if got := windowsCredentialPipeSDDL(sid); got != want {
		t.Fatalf("credential pipe SDDL = %q, want %q", got, want)
	}
}
