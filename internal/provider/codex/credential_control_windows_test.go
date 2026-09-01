//go:build windows

package codex

import (
	"path/filepath"
	"strings"
	"testing"
)

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
