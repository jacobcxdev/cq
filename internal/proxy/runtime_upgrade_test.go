package proxy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type receiptReadReplacementFS struct {
	fsutil.OSFileSystem
	opens    int
	replace  func() error
	replaced bool
	oldLinks uint64
	openErr  error
}

func (fsys *receiptReadReplacementFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	fsys.opens++
	if fsys.openErr != nil {
		return nil, fsys.openErr
	}
	directory, err := fsys.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return receiptReadReplacementDirectory{SecureDirectory: directory, fsys: fsys}, nil
}

type receiptReadReplacementDirectory struct {
	fsutil.SecureDirectory
	fsys *receiptReadReplacementFS
}

func (directory receiptReadReplacementDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	file, err := directory.SecureDirectory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	return receiptReadReplacementFile{SecureReadFile: file, fsys: directory.fsys}, nil
}

type receiptReadReplacementFile struct {
	fsutil.SecureReadFile
	fsys *receiptReadReplacementFS
}

func (file receiptReadReplacementFile) Read(data []byte) (int, error) {
	if file.fsys.replace != nil && !file.fsys.replaced {
		file.fsys.replaced = true
		if err := file.fsys.replace(); err != nil {
			return 0, err
		}
		info, err := file.SecureReadFile.Stat()
		if err != nil {
			return 0, err
		}
		identity, _ := file.fsys.FileIdentity(info)
		file.fsys.oldLinks = identity.Links
	}
	return file.SecureReadFile.Read(data)
}

func TestRuntimeUpgradeReceiptRetriesAtomicReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX opened-unlinked descriptor regression")
	}
	store, receipt := upgradeReceiptFixture(t)
	for _, phase := range []string{"prepared", "waiting"} {
		receipt.Phase = phase
		if err := store.Save(receipt); err != nil {
			t.Fatal(err)
		}
	}
	base := store
	fsys := &receiptReadReplacementFS{replace: func() error {
		receipt.Phase = "deferred"
		receipt.ErrorCode = "quiet_deadline"
		return base.Save(receipt)
	}}
	store.FS = fsys
	loaded, err := store.Load()
	if err != nil || loaded.Phase != "deferred" || fsys.oldLinks != 0 || fsys.opens != 2 {
		t.Fatalf("receipt replacement did not reopen strictly: phase=%s old_links=%d opens=%d err=%v", loaded.Phase, fsys.oldLinks, fsys.opens, err)
	}
}

func TestRuntimeUpgradeReceiptRejectsPermanentUnsafeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode and link validation")
	}
	for _, kind := range []string{"mode", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			store, receipt := upgradeReceiptFixture(t)
			if err := store.Save(receipt); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "mode":
				if err := os.Chmod(store.Path(), 0o644); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(store.Path(), store.Path()+".link"); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(store.Path(), store.Path()+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(store.Path()+".target", store.Path()); err != nil {
					t.Fatal(err)
				}
			}
			fsys := &receiptReadReplacementFS{}
			store.FS = fsys
			if _, err := store.Load(); err == nil {
				t.Fatal("permanently unsafe receipt was accepted")
			}
			if fsys.opens > 3 || (kind != "symlink" && fsys.opens != 3) {
				t.Fatalf("unsafe receipt retry count=%d", fsys.opens)
			}
		})
	}
}

func TestRuntimeUpgradeReceiptDoesNotRetryOtherFailures(t *testing.T) {
	for _, kind := range []string{"io", "json", "schema"} {
		t.Run(kind, func(t *testing.T) {
			store, receipt := upgradeReceiptFixture(t)
			if err := store.Save(receipt); err != nil {
				t.Fatal(err)
			}
			fsys := &receiptReadReplacementFS{}
			if kind == "io" {
				fsys.openErr = io.ErrUnexpectedEOF
			} else {
				data := []byte("invalid JSON")
				if kind == "schema" {
					data = []byte(`{"schema_version":0}`)
				}
				if err := os.WriteFile(store.Path(), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store.FS = fsys
			_, err := store.Load()
			if err == nil || fsys.opens != 1 || (kind == "io" && !errors.Is(err, io.ErrUnexpectedEOF)) {
				t.Fatalf("unrelated failure retried: opens=%d err=%v", fsys.opens, err)
			}
		})
	}
}

func upgradeReceiptFixture(t *testing.T) (RuntimeUpgradeStore, RuntimeUpgradeReceiptV1) {
	t.Helper()
	roots := userdirs.Roots{State: filepath.Join(t.TempDir(), "state")}
	artifact := installer.RuntimeArtifact{Path: filepath.Join(roots.State, "runtime-artifacts", strings.Repeat("a", 64), "cq"), SHA256: strings.Repeat("a", 64), Version: "0.34.0", ProtocolVersion: 1}
	return RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}, RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "test-upgrade", Generation: 1, Phase: "prepared", Previous: artifact, Candidate: artifact, ListenerIdentity: "tcp:127.0.0.1:12345", SupervisorPID: os.Getpid()}
}
func TestRuntimeUpgradeReceiptRejectsInvalidTransitions(t *testing.T) {
	store, receipt := upgradeReceiptFixture(t)
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Phase = "committed"
	if err := store.Save(receipt); err == nil {
		t.Fatal("prepared skipped handoff and verification")
	}
	receipt.Phase = "waiting"
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Generation = 0
	if err := store.Save(receipt); err == nil {
		t.Fatal("generation regressed")
	}
	receipt.Generation = 1
	receipt.TransactionID = "competing"
	if err := store.Save(receipt); err == nil {
		t.Fatal("competing transaction replaced pending receipt")
	}
}
func TestRuntimeUpgradeReceiptRejectsUnsupportedProtocol(t *testing.T) {
	store, receipt := upgradeReceiptFixture(t)
	receipt.Candidate.ProtocolVersion++
	if err := store.Save(receipt); err == nil {
		t.Fatal("unsupported protocol accepted")
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("invalid receipt mutated disk: %v", err)
	}
}

func TestRuntimeUpgradeReceiptInterruptedWriteKeepsPrevious(t *testing.T) {
	store, receipt := upgradeReceiptFixture(t)
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	store.FS = upgradeReceiptFailRenameFS{}
	receipt.Phase = "waiting"
	if err := store.Save(receipt); err == nil {
		t.Fatal("failed rename reported success")
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Phase != "prepared" {
		t.Fatal("interrupted write replaced canonical receipt")
	}
}

type upgradeReceiptFailRenameFS struct{ fsutil.OSFileSystem }

func (upgradeReceiptFailRenameFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	dir, err := (fsutil.OSFileSystem{}).OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return upgradeReceiptFailRenameDirectory{dir}, nil
}

type upgradeReceiptFailRenameDirectory struct{ fsutil.SecureDirectory }

func (upgradeReceiptFailRenameDirectory) Rename(string, string) error { return os.ErrPermission }
