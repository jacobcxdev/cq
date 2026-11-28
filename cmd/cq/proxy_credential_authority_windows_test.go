//go:build windows

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

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
	control, err := openProxyCredentialAuthority(false, 0, func(ctx context.Context) (*codexprov.CredentialControl, error) {
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
	control, err := openProxyCredentialAuthority(false, 0, func(ctx context.Context) (*codexprov.CredentialControl, error) {
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
	control, err := openProxyCredentialAuthority(false, 19281, func(ctx context.Context) (*codexprov.CredentialControl, error) {
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
