//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCredentialControlFirstOwnerSecondDelegates(t *testing.T) {
	coordinator, fs := testCoordinator(t)
	path := shortControlPath(t)
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if !owner.Owner() {
		t.Fatal("first control did not own endpoint")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
	}
	client, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.Owner() {
		t.Fatal("second control unexpectedly owns endpoint")
	}
	ref, revision, err := client.SaveLogin(context.Background(), testLoginCredential())
	if err != nil {
		t.Fatal(err)
	}
	if ref.AccountKey == "" || ref.CandidateID == "" || revision == "" {
		t.Fatalf("delegated reply = %+v %q", ref, revision)
	}
	fs.dirEntries = map[string][]fakeDirEntry{
		"/fake/home/.codex/accounts": {{name: string(ref.CandidateID) + ".auth.json"}},
	}
	inventory, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Accounts) != 1 || inventory.Accounts[0].Key != ref.AccountKey {
		t.Fatalf("delegated inventory = %+v", inventory)
	}
	material, err := client.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if material.AccessToken != "access" {
		t.Fatalf("delegated material = %+v", material)
	}
}

func TestCredentialControlSimultaneousStartupHasOneOwner(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := shortControlPath(t)
	controls := make(chan *CredentialControl, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			control, err := OpenCredentialControl(path, coordinator)
			controls <- control
			errs <- err
		}()
	}
	wg.Wait()
	close(controls)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	owners := 0
	for control := range controls {
		if control.Owner() {
			owners++
		}
		defer control.Close()
	}
	if owners != 1 {
		t.Fatalf("owners = %d, want 1", owners)
	}
}

func TestCredentialControlOwnerCloseDrainsClientsBeforeRebind(t *testing.T) {
	coordinator, _ := testCoordinator(t)
	path := shortControlPath(t)
	owner, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	client, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if !replacement.Owner() {
		t.Fatal("replacement did not own drained endpoint")
	}
	if err := client.client.Call("CredentialRPC.Ping", struct{}{}, &struct{}{}); err == nil {
		t.Fatal("drained client remained usable")
	}
}

func TestCredentialControlStaleEndpointFailsClosed(t *testing.T) {
	path := shortControlPath(t)
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	control, err := OpenCredentialControl(path, nil)
	if control != nil || !errors.Is(err, ErrCredentialOwnerStale) {
		t.Fatalf("OpenCredentialControl = %v, %v, want stale-owner error", control, err)
	}
}

func shortControlPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cqctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "control.sock")
}
