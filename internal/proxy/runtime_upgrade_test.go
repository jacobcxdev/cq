package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

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
