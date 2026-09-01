package main

import (
	"context"
	"errors"
	"testing"
	"time"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestAcquireProxyWorkerCredentialAuthorityWaitsForPredecessor(t *testing.T) {
	delegate := &codexprov.CredentialControl{}
	owner := &codexprov.CredentialControl{}
	openCalls := 0
	closeCalls := 0
	waitCalls := 0
	control, err := acquireProxyWorkerCredentialAuthority(context.Background(), proxyCredentialAuthorityOperations{
		open: func(context.Context) (*codexprov.CredentialControl, error) {
			openCalls++
			if openCalls == 1 {
				return delegate, nil
			}
			return owner, nil
		},
		owner: func(control *codexprov.CredentialControl) bool { return control == owner },
		close: func(control *codexprov.CredentialControl) error {
			if control != delegate {
				t.Fatalf("closed control = %p, want delegate %p", control, delegate)
			}
			closeCalls++
			return nil
		},
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if control != owner || openCalls != 2 || closeCalls != 1 || waitCalls != 1 {
		t.Fatalf("control/calls = %p/%d/%d/%d, want owner/2/1/1", control, openCalls, closeCalls, waitCalls)
	}
}

func TestAcquireProxyWorkerCredentialAuthorityFailsClosed(t *testing.T) {
	want := errors.New("wait stopped")
	delegate := &codexprov.CredentialControl{}
	control, err := acquireProxyWorkerCredentialAuthority(context.Background(), proxyCredentialAuthorityOperations{
		open:  func(context.Context) (*codexprov.CredentialControl, error) { return delegate, nil },
		owner: func(*codexprov.CredentialControl) bool { return false },
		close: func(*codexprov.CredentialControl) error { return nil },
		wait:  func(context.Context, time.Duration) error { return want },
	})
	if control != nil || !errors.Is(err, want) || !errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable) {
		t.Fatalf("control/error = %v/%v, want nil joined authority/wait error", control, err)
	}
}

func TestAcquireProxyWorkerCredentialAuthorityDoesNotOpenAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	openCalls := 0
	control, err := acquireProxyWorkerCredentialAuthority(ctx, proxyCredentialAuthorityOperations{
		open: func(context.Context) (*codexprov.CredentialControl, error) {
			openCalls++
			return &codexprov.CredentialControl{}, nil
		},
		owner: func(*codexprov.CredentialControl) bool { return true },
		close: func(*codexprov.CredentialControl) error { return nil },
		wait:  waitProxyCredentialAuthority,
	})
	if control != nil || openCalls != 0 || !errors.Is(err, context.Canceled) || !errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable) {
		t.Fatalf("control/calls/error = %v/%d/%v, want nil/0 joined authority/cancellation error", control, openCalls, err)
	}
}

func TestAcquireProxyWorkerCredentialAuthorityRejectsOwnerReturnedAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	owner := &codexprov.CredentialControl{}
	closeCalls := 0
	control, err := acquireProxyWorkerCredentialAuthority(ctx, proxyCredentialAuthorityOperations{
		open: func(openCtx context.Context) (*codexprov.CredentialControl, error) {
			if openCtx != ctx {
				t.Fatal("credential authority context was not passed to opener")
			}
			cancel()
			return owner, nil
		},
		owner: func(*codexprov.CredentialControl) bool { return true },
		close: func(control *codexprov.CredentialControl) error {
			if control != owner {
				t.Fatalf("closed control = %p, want owner %p", control, owner)
			}
			closeCalls++
			return nil
		},
		wait: waitProxyCredentialAuthority,
	})
	if control != nil || closeCalls != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable) {
		t.Fatalf("control/closes/error = %v/%d/%v, want nil/1 joined authority/cancellation error", control, closeCalls, err)
	}
}
