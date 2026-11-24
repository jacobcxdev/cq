package harness

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestRetryRequestPreservedOwnerAndPrivateBinding(t *testing.T) {
	fsys := newHarnessMemFS()
	store := Receipts{FS: fsys, Dir: testAbsPath("/state/receipts")}
	lease, err := store.Acquire(testChat)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	receipt := receiptFixture()
	receipt.Mode, receipt.State, receipt.Profile, receipt.RetrySupported = "full", "failed", Custom, true
	receipt.Fingerprint, receipt.Model, receipt.Cwd = strings.Repeat("a", 64), "fixture", testAbsPath("/fixture")
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	receipt, err = store.Load(testChat)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := fsys.ReadFile(filepath.Join(store.Dir, testChat+".json"))
	request := RetryRequest{Fingerprint: receipt.Fingerprint, Model: receipt.Model, Cwd: receipt.Cwd, FailedAt: receipt.UpdatedAt}
	first, err := store.RequestRetry(testChat, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RequestRetry(testChat, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Nonce == "" || first.Nonce != second.Nonce || !first.Matches(receipt) {
		t.Fatal("duplicate intent was not coalesced")
	}
	after, _ := fsys.ReadFile(filepath.Join(store.Dir, testChat+".json"))
	if !bytes.Equal(before, after) {
		t.Fatal("retry changed owner's receipt")
	}
	if another, err := store.Acquire(testChat); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		if another != nil {
			another.Close()
		}
		t.Fatal("retry released native owner's lease")
	}
	path := filepath.Join(store.Dir, testChat+".retry.json")
	info, err := fsys.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("retry intent was not private")
	}
	data, _ := fsys.ReadFile(path)
	other := "00000000-0000-0000-0000-000000000002"
	if err := fsys.WriteFile(filepath.Join(store.Dir, other+".retry.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadRetry(other); err == nil {
		t.Fatal("copied retry intent lost its chat binding")
	}
}

func TestRetryMissingReadCreatedNothing(t *testing.T) {
	fsys := newHarnessMemFS()
	store := Receipts{FS: fsys, Dir: testAbsPath("/state/receipts")}
	if _, err := store.LoadRetry(testChat); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := fsys.Stat(testAbsPath("/state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created retry state")
	}
}

func TestRetryRequestRejectedInvalidAuthority(t *testing.T) {
	valid := RetryRequest{ChatID: testChat, Nonce: "request-one", Fingerprint: strings.Repeat("a", 64), Model: "fixture", Cwd: testAbsPath("/fixture"), FailedAt: time.Now().UTC()}
	for name, alter := range map[string]func(*RetryRequest){
		"nonce":       func(request *RetryRequest) { request.Nonce = "private secret" },
		"fingerprint": func(request *RetryRequest) { request.Fingerprint = strings.Repeat("A", 64) },
		"project":     func(request *RetryRequest) { request.Cwd = "relative" },
		"time":        func(request *RetryRequest) { request.FailedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			alter(&request)
			if err := validateRetryRequest(testChat, request); err == nil {
				t.Fatal("invalid retry authority accepted")
			}
		})
	}
}
