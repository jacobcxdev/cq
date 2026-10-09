package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type serviceUpgradeArtifacts map[string]installer.RuntimeArtifact

func (artifacts serviceUpgradeArtifacts) Stage(_ context.Context, path string) (installer.RuntimeArtifact, error) {
	artifact, ok := artifacts[path]
	if !ok {
		return artifact, proxy.ErrRuntimeUpgradeUnsupported
	}
	return artifact, nil
}
func (artifacts serviceUpgradeArtifacts) Verify(_ context.Context, artifact installer.RuntimeArtifact) error {
	for _, value := range artifacts {
		if value == artifact {
			return nil
		}
	}
	return proxy.ErrRuntimeUpgradeReceipt
}

type retainedServicePlatform struct {
	*fakeServicePlatform
	configured, live, refresh string
}

func (platform *retainedServicePlatform) Preflight(context.Context, string) error { return nil }
func (platform *retainedServicePlatform) InstallProxy(ctx context.Context, path string) error {
	platform.configured = path
	platform.live = path
	return platform.fakeServicePlatform.InstallProxy(ctx, path)
}
func (platform *retainedServicePlatform) InstallRefresh(ctx context.Context, path string) error {
	if err := platform.fakeServicePlatform.InstallRefresh(ctx, path); err != nil {
		return err
	}
	platform.refresh = path
	return nil
}
func (platform *retainedServicePlatform) Inspect(ctx context.Context) (serviceStatus, error) {
	status, err := platform.fakeServicePlatform.Inspect(ctx)
	status.Proxy.ConfiguredExecutable = platform.configured
	status.Proxy.LiveExecutable = platform.live
	status.Refresh.ConfiguredExecutable = platform.refresh
	return status, err
}

func homebrewUpgradeServiceFixture(t *testing.T) (*serviceLifecycle, *retainedServicePlatform, installstate.Store, installer.RuntimeArtifact, installer.RuntimeArtifact) {
	t.Helper()
	lifecycle, original, store := newServiceHarness(t)
	platform := &retainedServicePlatform{fakeServicePlatform: original}
	lifecycle.Platform = platform
	previous := installer.RuntimeArtifact{Path: filepath.Join(t.TempDir(), "retained-previous", "cq"), SHA256: strings.Repeat("a", 64), Version: "0.34.0", ProtocolVersion: 1}
	candidate := installer.RuntimeArtifact{Path: filepath.Join(t.TempDir(), "retained-candidate", "cq"), SHA256: strings.Repeat("b", 64), Version: "0.34.1", ProtocolVersion: 1}
	lifecycle.RuntimeArtifacts = serviceUpgradeArtifacts{lifecycle.Executable: previous, previous.Path: previous, candidate.Path: candidate}
	lifecycle.RuntimePreflight = func(context.Context, installstate.Record) error { return nil }
	lifecycle.RuntimeApply = func(_ context.Context, artifact installer.RuntimeArtifact) (proxy.RuntimeUpgradeReceiptV1, error) {
		platform.calls = append(platform.calls, "runtime-upgrade")
		before, _ := lifecycle.RuntimeArtifacts.Stage(context.Background(), platform.live)
		platform.live = artifact.Path
		return proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "fixture", Generation: 1, Phase: "committed", Previous: before, Candidate: artifact, ListenerIdentity: "tcp|127.0.0.1:12345", SupervisorPID: 41}, nil
	}
	lifecycle.RuntimeRestoreRefresh = func(_ context.Context, _ servicePlatformSnapshot) error {
		platform.calls = append(platform.calls, "restore-refresh")
		platform.refresh = previous.Path
		return nil
	}
	lifecycle.RuntimeCleanup = func(context.Context) error { platform.calls = append(platform.calls, "clean-runtime"); return nil }
	if err := lifecycle.Install(context.Background(), installstate.OwnerHomebrew); err != nil {
		t.Fatal(err)
	}
	platform.calls = nil
	return lifecycle, platform, store, previous, candidate
}

func TestHomebrewUpgradeKeepsJobsAndUsesRetainedRuntime(t *testing.T) {
	lifecycle, platform, store, previous, candidate := homebrewUpgradeServiceFixture(t)
	receipt, err := lifecycle.Upgrade(context.Background(), installstate.OwnerHomebrew, candidate.Path)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Phase != "committed" || platform.live != candidate.Path || platform.configured != previous.Path || platform.refresh != candidate.Path {
		t.Fatalf("wrong runtime selection: %+v %+v", receipt, platform)
	}
	for _, call := range platform.calls {
		if call == "install-proxy" || call == "remove-proxy" || call == "restart-proxy" || call == "restore" {
			t.Fatalf("upgrade changed proxy job: %v", platform.calls)
		}
	}
	record, err := store.Load()
	if err != nil || record.Executable != lifecycle.Executable || record.BinaryDigest != candidate.SHA256 || record.Version != candidate.Version {
		t.Fatalf("ownership selection: %+v %v", record, err)
	}
}

func TestHomebrewUpgradePrunesAfterOwnershipSettles(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprint(rollback), func(t *testing.T) {
			lifecycle, platform, store, previous, candidate := homebrewUpgradeServiceFixture(t)
			expected := candidate
			if rollback {
				platform.installRefreshErr = errors.New("refresh failed")
				expected = previous
			}
			pruned := false
			lifecycle.RuntimePrune = func(context.Context) error {
				pruned = true
				record, err := store.Load()
				if err != nil || record.BinaryDigest != expected.SHA256 || platform.live != expected.Path || platform.refresh != expected.Path {
					t.Fatalf("pruned before selection settled: %+v %v", record, err)
				}
				return errors.New("cleanup failed")
			}
			_, err := lifecycle.Upgrade(context.Background(), installstate.OwnerHomebrew, candidate.Path)
			if !pruned || (err != nil) != rollback {
				t.Fatalf("cleanup failure changed upgrade outcome: pruned=%v err=%v", pruned, err)
			}
		})
	}
}
func TestHomebrewRevertSelectsPreviousArtifact(t *testing.T) {
	lifecycle, platform, store, previous, candidate := homebrewUpgradeServiceFixture(t)
	platform.installRefreshErr = errors.New("refresh failed")
	receipt, err := lifecycle.Upgrade(context.Background(), installstate.OwnerHomebrew, candidate.Path)
	if err == nil || receipt.Candidate != previous || platform.live != previous.Path || platform.refresh != previous.Path {
		t.Fatalf("refresh failure lost previous selection: %+v %v", receipt, err)
	}
	record, err := store.Load()
	if err != nil || record.BinaryDigest != previous.SHA256 {
		t.Fatalf("ownership changed on failed upgrade: %+v %v", record, err)
	}
	for _, call := range platform.calls {
		if call == "restore" || call == "restart-proxy" || call == "remove-proxy" {
			t.Fatalf("rollback restarted proxy job: %v", platform.calls)
		}
	}
}

type failCandidateOwnershipStore struct {
	serviceStateStore
	candidate string
	failed    bool
}

func (store *failCandidateOwnershipStore) Save(record installstate.Record) error {
	if record.BinaryDigest == store.candidate && !store.failed {
		store.failed = true
		return errors.New("transient ownership write failure")
	}
	return store.serviceStateStore.Save(record)
}

func TestHomebrewOwnershipSaveFailureRestoresPreviousRecord(t *testing.T) {
	lifecycle, platform, store, previous, candidate := homebrewUpgradeServiceFixture(t)
	lifecycle.Store = &failCandidateOwnershipStore{serviceStateStore: store, candidate: candidate.SHA256}
	if _, err := lifecycle.Upgrade(context.Background(), installstate.OwnerHomebrew, candidate.Path); err == nil {
		t.Fatal("ownership save failure was hidden")
	}
	record, err := store.Load()
	if err != nil || record.BinaryDigest != previous.SHA256 || record.Version != previous.Version || platform.live != previous.Path || platform.refresh != previous.Path {
		t.Fatalf("rollback ownership disagrees with previous runtime: %+v %v", record, err)
	}
}

type strictRetainedSnapshotPlatform struct{ *retainedServicePlatform }

func (*strictRetainedSnapshotPlatform) Preflight(context.Context, string) error {
	return installstate.ErrOwnershipConflict
}

func TestHomebrewSnapshotUsesRetainedOwnershipAndRestores(t *testing.T) {
	lifecycle, platform, store, _, candidate := homebrewUpgradeServiceFixture(t)
	if _, err := lifecycle.Upgrade(context.Background(), installstate.OwnerHomebrew, candidate.Path); err != nil {
		t.Fatal(err)
	}
	lifecycle.Platform = &strictRetainedSnapshotPlatform{platform}
	checked := false
	lifecycle.RuntimePreflight = func(_ context.Context, record installstate.Record) error {
		checked = record.BinaryDigest == candidate.SHA256 && record.Executable == lifecycle.Executable
		if !checked {
			return installstate.ErrOwnershipConflict
		}
		return nil
	}
	path := filepath.Join(t.TempDir(), "private", "snapshot.json")
	if err := lifecycle.Snapshot(context.Background(), installstate.OwnerHomebrew, path); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("snapshot skipped retained ownership preflight")
	}
	if err := lifecycle.Restore(context.Background(), installstate.OwnerHomebrew, path); err != nil {
		t.Fatal(err)
	}
	record, err := store.Load()
	if err != nil || record.BinaryDigest != candidate.SHA256 {
		t.Fatalf("snapshot restore changed selected ownership: %+v %v", record, err)
	}
}

func TestHomebrewSnapshotRestorePreflightsBeforeChangingJobs(t *testing.T) {
	lifecycle, platform, _, _, _ := homebrewUpgradeServiceFixture(t)
	path := filepath.Join(t.TempDir(), "private", "snapshot.json")
	if err := lifecycle.Snapshot(context.Background(), installstate.OwnerHomebrew, path); err != nil {
		t.Fatal(err)
	}
	platform.calls = nil
	lifecycle.RuntimeSnapshotCheck = func(context.Context, servicePlatformSnapshot) error {
		return errors.New("snapshot executable unavailable")
	}
	if err := lifecycle.Restore(context.Background(), installstate.OwnerHomebrew, path); err == nil {
		t.Fatal("snapshot with unavailable executable restored")
	}
	if len(platform.calls) != 0 {
		t.Fatalf("rejected snapshot changed jobs: %v", platform.calls)
	}
}
func TestHomebrewTrueUninstallRemovesJobsAndState(t *testing.T) {
	lifecycle, platform, store, _, _ := homebrewUpgradeServiceFixture(t)
	if err := lifecycle.Uninstall(context.Background(), installstate.OwnerHomebrew); err != nil {
		t.Fatal(err)
	}
	if platform.proxyRegistered || platform.refreshRegistered {
		t.Fatal("true uninstall retained jobs")
	}
	if _, err := store.Load(); !errors.Is(err, installstate.ErrNotInstalled) {
		t.Fatalf("ownership retained: %v", err)
	}
	if !strings.Contains(strings.Join(platform.calls, ","), "clean-runtime") {
		t.Fatal("runtime artifacts not cleaned")
	}
}
func TestHomebrewUpgradeDeferralKeepsOwnership(t *testing.T) {
	lifecycle, platform, store, previous, candidate := homebrewUpgradeServiceFixture(t)
	lifecycle.RuntimeApply = func(context.Context, installer.RuntimeArtifact) (proxy.RuntimeUpgradeReceiptV1, error) {
		return proxy.RuntimeUpgradeReceiptV1{Phase: "deferred", Previous: previous, Candidate: candidate}, nil
	}
	receipt, err := lifecycle.Upgrade(context.Background(), installstate.OwnerHomebrew, candidate.Path)
	if !errors.Is(err, ErrServiceUpgradeDeferred) || receipt.Phase != "deferred" {
		t.Fatalf("deferred reported success: %+v %v", receipt, err)
	}
	record, _ := store.Load()
	if record.BinaryDigest != previous.SHA256 || platform.live != previous.Path {
		t.Fatal("deferral changed selection")
	}
}
func TestLegacyHomebrewBootstrapReportsMaintenanceTransition(t *testing.T) {
	lifecycle, platform, _, _, candidate := homebrewUpgradeServiceFixture(t)
	lifecycle.RuntimePreflight = func(context.Context, installstate.Record) error { return ErrServiceUpgradeMaintenance }
	if _, err := lifecycle.Upgrade(context.Background(), installstate.OwnerHomebrew, candidate.Path); !errors.Is(err, ErrServiceUpgradeMaintenance) {
		t.Fatalf("unsupported predecessor: %v", err)
	}
	if len(platform.calls) != 0 {
		t.Fatalf("legacy transition mutated jobs: %v", platform.calls)
	}
}
func TestServiceUpgradeParserRequiresNativeHomebrewCandidate(t *testing.T) {
	command, err := parseServiceCommand([]string{"upgrade", "--owner=homebrew", "--candidate-executable=/tmp/cq", "--service-executable=/opt/homebrew/bin/cq", "--json"})
	if err != nil || command.CandidateExecutable != "/tmp/cq" {
		t.Fatalf("upgrade parser: %+v %v", command, err)
	}
	for _, args := range [][]string{{"upgrade", "--owner=go", "--candidate-executable=/tmp/cq"}, {"upgrade", "--owner=homebrew"}, {"upgrade", "--owner=homebrew", "--candidate-executable=relative"}, {"status", "--candidate-executable=/tmp/cq"}} {
		if _, err := parseServiceCommand(args); err == nil {
			t.Fatalf("accepted invalid candidate args: %v", args)
		}
	}
}
