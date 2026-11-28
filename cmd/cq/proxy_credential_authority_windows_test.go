//go:build windows

package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestWindowsStandaloneProxyWaitsForReservedCredentialPipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sock")
	reserved := make(chan struct{})
	release := make(chan struct{})
	released := false
	releaseOwner := func() {
		if !released {
			close(release)
			released = true
		}
	}
	t.Cleanup(releaseOwner)
	stopRefresh := errors.New("fixture refresh stopped during initialisation")
	ownerDone := make(chan error, 1)
	go func() {
		control, err := codexprov.OpenCredentialControlPrepared(context.Background(), path, &codexprov.CredentialCoordinator{},
			func(context.Context, *codexprov.CredentialCoordinator, codexprov.CredentialOwnerCapability) error {
				close(reserved)
				<-release
				return stopRefresh
			})
		if control != nil {
			_ = control.Close()
		}
		ownerDone <- err
	}()
	select {
	case <-reserved:
	case err := <-ownerDone:
		t.Fatalf("refresh did not reserve the pipe: %v", err)
	}

	openCalls := 0
	control, err := openProxyCredentialAuthority(false, 19280, 19280, func(ctx context.Context) (*codexprov.CredentialControl, error) {
		openCalls++
		control, err := codexprov.OpenCredentialControlPrepared(ctx, path, &codexprov.CredentialCoordinator{}, nil)
		if openCalls == 1 {
			if !errors.Is(err, codexprov.ErrCredentialOwnerStale) {
				t.Errorf("open against reserved pipe = %v, want transient owner error", err)
			}
			releaseOwner()
			if ownerErr := <-ownerDone; !errors.Is(ownerErr, stopRefresh) {
				t.Errorf("fixture refresh exit = %v", ownerErr)
			}
		}
		return control, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if !control.Owner() || openCalls != 2 {
		t.Fatalf("proxy owner/open calls = %t/%d, want true/2", control.Owner(), openCalls)
	}
}

func TestWindowsStandaloneProxyAcquiresCredentialAuthorityAfterRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sock")
	refresh, err := codexprov.OpenCredentialControl(path, &codexprov.CredentialCoordinator{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = refresh.Close() })
	if !refresh.Owner() {
		t.Fatal("refresh did not own the credential pipe")
	}

	openCalls := 0
	control, err := openProxyCredentialAuthority(false, 19280, 0, func(ctx context.Context) (*codexprov.CredentialControl, error) {
		store := &codexprov.ManagedStore{FS: fsutil.NewMemFSWithHome(filepath.Dir(path))}
		control, err := codexprov.OpenCredentialControlPrepared(ctx, path, &codexprov.CredentialCoordinator{Store: store}, nil)
		if err != nil {
			return nil, err
		}
		openCalls++
		if openCalls == 1 {
			if control.Owner() {
				t.Error("first proxy open did not delegate to refresh")
			}
			if err := refresh.Close(); err != nil {
				t.Errorf("close refresh: %v", err)
			}
		}
		return control, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, inventoryErr := listProxyCodexStartupInventory(context.Background(), control); !control.Owner() || inventoryErr != nil {
		t.Fatalf("proxy retained the closed refresh delegate: inventory error = %v", inventoryErr)
	}
	if openCalls != 2 {
		t.Fatalf("credential open calls = %d, want 2", openCalls)
	}
}

func TestWindowsStandaloneProxyAcquiresUnownedCredentialAuthorityOnFirstOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sock")
	openCalls := 0
	control, err := openProxyCredentialAuthority(false, 19280, 0, func(ctx context.Context) (*codexprov.CredentialControl, error) {
		openCalls++
		return codexprov.OpenCredentialControlPrepared(ctx, path, &codexprov.CredentialCoordinator{}, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if !control.Owner() || openCalls != 1 {
		t.Fatalf("proxy owner/open calls = %t/%d, want true/1", control.Owner(), openCalls)
	}
}

func TestWindowsAlternatePortProxyCanUseExistingCredentialAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.sock")
	owner, err := codexprov.OpenCredentialControl(path, &codexprov.CredentialCoordinator{})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	control, err := openProxyCredentialAuthority(false, 19280, 19281, func(ctx context.Context) (*codexprov.CredentialControl, error) {
		return codexprov.OpenCredentialControlPrepared(ctx, path, &codexprov.CredentialCoordinator{}, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if control.Owner() {
		t.Fatal("an alternate-port proxy displaced the existing credential authority")
	}
}
