package installer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestInstallerFreshInstallCommitsBinaryServicesMetadataAndState(t *testing.T) {
	harness := newInstallerHarness(t)

	if err := harness.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	harness.assertInstalled(t, harness.downloader.body, "0.27.0", installstate.OwnerGo)
	if harness.lifecycle.stopCalls != 0 || harness.lifecycle.installCalls != 1 || harness.lifecycle.statusCalls != 1 || harness.metadata.installCalls != 1 || harness.metadata.inspectCalls != 1 {
		t.Fatalf("lifecycle/metadata calls = %#v / %#v", harness.lifecycle, harness.metadata)
	}
	if harness.locker.acquireCalls != 1 || harness.locker.closeCalls != 1 || harness.temporary.removeCalls != 1 {
		t.Fatalf("lock/temp calls = %#v / %#v", harness.locker, harness.temporary)
	}
}

func TestInstallerCreatesMissingExecutableDirectory(t *testing.T) {
	harness := newInstallerHarness(t)
	directory := filepath.Join(filepath.Dir(filepath.Dir(harness.installer.Installation.Executable)), "new-bin")
	harness.installer.Installation.Executable = filepath.Join(directory, installerExecutableName())

	if err := harness.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if info, err := harness.fsys.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("destination directory = %v, %v", info, err)
	}
}

func TestInstallerWritesIntoOwnerControlledExecutableDirectory(t *testing.T) {
	fsys := &pathRestrictedInstallerFS{MemFS: fsutil.NewMemFS()}
	directory := filepath.Join("go", "bin")
	if err := fsys.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	installation := Installation{Executable: filepath.Join(directory, installerExecutableName())}
	installer := Installer{FS: fsys, Installation: installation}
	body := []byte("cq")

	if err := installer.writeExclusive(installer.candidatePath(), body, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := fsys.ReadFile(installer.candidatePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("candidate body = %q", got)
	}
}

func TestInstallerRemovesNewExecutableDirectoryAfterFailure(t *testing.T) {
	harness := newInstallerHarness(t)
	directory := filepath.Join(filepath.Dir(filepath.Dir(harness.installer.Installation.Executable)), "new-bin")
	harness.installer.Installation.Executable = filepath.Join(directory, installerExecutableName())
	harness.downloader.err = errors.New("download failed")

	if err := harness.installer.Install(context.Background()); err == nil {
		t.Fatal("Install() succeeded")
	}
	if _, err := harness.fsys.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed destination directory remains: %v", err)
	}
}

func TestInstallerIdempotentReinstallDoesNotReplaceMatchingBinary(t *testing.T) {
	harness := newInstallerHarness(t)
	if err := harness.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	renameCalls := harness.fsys.renameCalls
	if err := harness.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if harness.fsys.renameCalls != renameCalls || harness.lifecycle.stopCalls != 0 || harness.lifecycle.installCalls != 1 {
		t.Fatalf("idempotent reinstall mutated binary/services: renames=%d lifecycle=%#v", harness.fsys.renameCalls, harness.lifecycle)
	}
	if harness.lifecycle.statusCalls != 2 || harness.metadata.inspectCalls != 2 {
		t.Fatalf("idempotent validation calls = %#v / %#v", harness.lifecycle, harness.metadata)
	}
}

func TestInstallerIdempotentReinstallRepairsUnhealthyServices(t *testing.T) {
	harness := newInstallerHarness(t)
	if err := harness.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	renameCalls := harness.fsys.renameCalls
	harness.lifecycle.statusErrors = []error{errors.New("services unhealthy"), nil}

	if err := harness.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if harness.fsys.renameCalls != renameCalls || harness.lifecycle.stopCalls != 0 {
		t.Fatalf("repair replaced binary or stopped services: renames=%d lifecycle=%#v", harness.fsys.renameCalls, harness.lifecycle)
	}
	if harness.lifecycle.installCalls != 2 || harness.lifecycle.statusCalls != 3 || harness.metadata.inspectCalls != 2 {
		t.Fatalf("repair validation calls = %#v / %#v", harness.lifecycle, harness.metadata)
	}
}

func TestInstallerUpgradeReplacesOwnedBinary(t *testing.T) {
	harness := newInstallerHarness(t)
	harness.seedInstalled(t, []byte("old-binary"), "0.26.2", installstate.OwnerGo)

	if err := harness.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	harness.assertInstalled(t, harness.downloader.body, "0.27.0", installstate.OwnerGo)
	if harness.lifecycle.stopCalls != 1 || harness.lifecycle.installCalls != 1 {
		t.Fatalf("upgrade lifecycle = %#v", harness.lifecycle)
	}
	if _, err := harness.fsys.Stat(harness.installer.rollbackPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback file remains: %v", err)
	}
}

func TestInstallerRejectsStagedVersionMismatchBeforeMutation(t *testing.T) {
	harness := newInstallerHarness(t)
	harness.runner.version = "0.26.2"

	err := harness.installer.Install(context.Background())
	if !errors.Is(err, ErrStagedVersionMismatch) {
		t.Fatalf("Install() error = %v", err)
	}
	if harness.lifecycle.totalCalls() != 0 || harness.metadata.totalCalls() != 0 || harness.fsys.renameCalls != 0 {
		t.Fatalf("version mismatch mutated state")
	}
	if harness.temporary.removeCalls != 1 {
		t.Fatal("version mismatch did not clean temporary root")
	}
}

func TestInstallerRejectsForeignBinaryBeforeDownload(t *testing.T) {
	harness := newInstallerHarness(t)
	if err := harness.fsys.WriteFile(harness.installer.Installation.Executable, []byte("foreign"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := harness.installer.Install(context.Background())
	if !errors.Is(err, ErrForeignBinary) {
		t.Fatalf("Install() error = %v", err)
	}
	if harness.downloader.calls != 0 || harness.lifecycle.totalCalls() != 0 {
		t.Fatal("foreign binary reached download or service mutation")
	}
}

func TestInstallerAdoptsUnownedGoBinary(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("plain-go-binary")
	if err := harness.fsys.WriteFile(harness.installer.Installation.Executable, oldBody, 0o700); err != nil {
		t.Fatal(err)
	}
	harness.installer.Classifier = fakeBinaryClassifier{classification: BinaryAdoptable}

	if err := harness.installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	harness.assertInstalled(t, harness.downloader.body, "0.27.0", installstate.OwnerGo)
	if harness.lifecycle.stopCalls != 0 || harness.lifecycle.installCalls != 1 || harness.lifecycle.statusCalls != 1 {
		t.Fatalf("adoption lifecycle = %#v", harness.lifecycle)
	}
}

func TestInstallerAdoptionFailureRestoresUnownedBinary(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("plain-go-binary")
	if err := harness.fsys.WriteFile(harness.installer.Installation.Executable, oldBody, 0o700); err != nil {
		t.Fatal(err)
	}
	harness.installer.Classifier = fakeBinaryClassifier{classification: BinaryAdoptable}
	harness.lifecycle.statusErrors = []error{errors.New("candidate unhealthy")}

	err := harness.installer.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "candidate unhealthy") {
		t.Fatalf("Install() error = %v", err)
	}
	body, readErr := harness.fsys.ReadFile(harness.installer.Installation.Executable)
	if readErr != nil || !reflect.DeepEqual(body, oldBody) {
		t.Fatalf("restored adopted body = %q, %v", body, readErr)
	}
	if _, stateErr := harness.store.Load(); !errors.Is(stateErr, installstate.ErrNotInstalled) {
		t.Fatalf("adoption rollback state = %v", stateErr)
	}
	if harness.lifecycle.stopCalls != 0 || harness.lifecycle.installCalls != 1 || harness.lifecycle.uninstallCalls != 1 {
		t.Fatalf("adoption rollback lifecycle = %#v", harness.lifecycle)
	}
}

func TestInstallerRejectsOwnershipConflictBeforeDownload(t *testing.T) {
	harness := newInstallerHarness(t)
	harness.seedInstalled(t, []byte("old"), "0.26.2", installstate.OwnerHomebrew)

	err := harness.installer.Install(context.Background())
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("Install() error = %v", err)
	}
	if harness.downloader.calls != 0 || harness.lifecycle.totalCalls() != 0 {
		t.Fatal("ownership conflict reached download or services")
	}
}

func TestInstallerServiceFailureRollsBackBinaryServicesMetadataAndState(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("old-binary")
	harness.seedInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	harness.lifecycle.installErrors = []error{errors.New("candidate service failed"), nil}

	err := harness.installer.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "candidate service failed") {
		t.Fatalf("Install() error = %v", err)
	}
	harness.assertInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	if harness.lifecycle.uninstallCalls != 0 || harness.lifecycle.installCalls != 1 || harness.lifecycle.snapshotCalls != 1 || harness.lifecycle.restoreCalls != 1 || harness.lifecycle.statusCalls != 0 {
		t.Fatalf("rollback lifecycle = %#v", harness.lifecycle)
	}
	if harness.metadata.installCalls != 1 || harness.metadata.inspectCalls != 1 {
		t.Fatalf("rollback metadata = %#v", harness.metadata)
	}
	if _, err := harness.fsys.Stat(harness.installer.rollbackPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proved rollback file remains: %v", err)
	}
}

func TestInstallerStatusFailureRollsBack(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("old")
	harness.seedInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	harness.lifecycle.statusErrors = []error{errors.New("status timeout"), nil}

	err := harness.installer.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status timeout") {
		t.Fatalf("Install() error = %v", err)
	}
	harness.assertInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	if harness.lifecycle.uninstallCalls != 1 || harness.lifecycle.installCalls != 1 || harness.lifecycle.snapshotCalls != 1 || harness.lifecycle.restoreCalls != 1 || harness.lifecycle.statusCalls != 1 {
		t.Fatalf("status rollback lifecycle = %#v", harness.lifecycle)
	}
}

func TestInstallerPostServiceFailureRestoresExactServiceSnapshot(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("old")
	harness.seedInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	harness.lifecycle.modelServiceState = true
	harness.lifecycle.definition = "custom-owned-definition"
	harness.lifecycle.running = false
	harness.metadata.installErrors = []error{errors.New("metadata failed after service install")}

	err := harness.installer.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "metadata failed after service install") {
		t.Fatalf("Install() error = %v", err)
	}
	harness.assertInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	if harness.lifecycle.definition != "custom-owned-definition" || harness.lifecycle.running {
		t.Fatalf("restored service state = definition %q running %t", harness.lifecycle.definition, harness.lifecycle.running)
	}
}

func TestInstallerReplacementFailureRollsBackStoppedServices(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("old")
	harness.seedInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	harness.fsys.failRenameDestination = harness.installer.Installation.Executable
	harness.fsys.failRenameCount = 1

	err := harness.installer.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "replace") {
		t.Fatalf("Install() error = %v", err)
	}
	harness.assertInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	if harness.lifecycle.stopCalls != 1 || harness.lifecycle.installCalls != 0 || harness.lifecycle.snapshotCalls != 1 || harness.lifecycle.restoreCalls != 1 || harness.lifecycle.statusCalls != 0 {
		t.Fatalf("replacement rollback lifecycle = %#v", harness.lifecycle)
	}
}

func TestInstallerRollbackValidationFailurePreservesRecoveryFile(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("old")
	harness.seedInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	harness.lifecycle.statusErrors = []error{errors.New("candidate unhealthy")}
	harness.lifecycle.restoreErrors = []error{errors.New("rollback unhealthy")}

	err := harness.installer.Install(context.Background())
	if !errors.Is(err, ErrRollbackUnverified) || !strings.Contains(err.Error(), harness.installer.rollbackPath()) {
		t.Fatalf("Install() error = %v", err)
	}
	body, readErr := harness.fsys.ReadFile(harness.installer.rollbackPath())
	if readErr != nil || !reflect.DeepEqual(body, oldBody) {
		t.Fatalf("rollback evidence = %q, %v", body, readErr)
	}
	if harness.temporary.removeCalls != 1 {
		t.Fatal("rollback failure did not clean download root")
	}
}

func TestInstallerUninstallRemovesOnlyOwnedInstallation(t *testing.T) {
	harness := newInstallerHarness(t)
	harness.seedInstalled(t, []byte("owned"), "0.26.2", installstate.OwnerGo)
	preserved := filepath.Join(harness.roots.State, "history.json")
	if err := harness.fsys.WriteFile(preserved, []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := harness.installer.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.fsys.Stat(harness.installer.Installation.Executable); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binary remains: %v", err)
	}
	if _, err := harness.store.Load(); !errors.Is(err, installstate.ErrNotInstalled) {
		t.Fatalf("state remains: %v", err)
	}
	if body, err := harness.fsys.ReadFile(preserved); err != nil || string(body) != "history" {
		t.Fatalf("preserved state = %q, %v", body, err)
	}
	if harness.lifecycle.uninstallCalls != 1 || harness.metadata.removeCalls != 1 {
		t.Fatalf("uninstall calls = %#v / %#v", harness.lifecycle, harness.metadata)
	}
}

func TestInstallerUninstallRejectsChangedOwnedBinary(t *testing.T) {
	harness := newInstallerHarness(t)
	harness.seedInstalled(t, []byte("owned"), "0.26.2", installstate.OwnerGo)
	if err := harness.fsys.WriteFile(harness.installer.Installation.Executable, []byte("replaced"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := harness.installer.Uninstall(context.Background())
	if !errors.Is(err, ErrForeignBinary) {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if harness.lifecycle.totalCalls() != 0 || harness.metadata.totalCalls() != 0 {
		t.Fatal("changed binary triggered uninstall mutation")
	}
}

func TestInstallerUninstallLifecycleFailureRestoresExactSnapshot(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("owned")
	harness.seedInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	harness.lifecycle.modelServiceState = true
	harness.lifecycle.definition = "custom-owned-definition"
	harness.lifecycle.running = false
	harness.lifecycle.mutateOnUninstallError = true
	harness.lifecycle.uninstallErrors = []error{errors.New("remove refresh service")}

	err := harness.installer.Uninstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "uninstall CQ services") {
		t.Fatalf("Uninstall() error = %v", err)
	}
	harness.assertInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	if harness.lifecycle.definition != "custom-owned-definition" || harness.lifecycle.running {
		t.Fatalf("restored service state = definition %q running %t", harness.lifecycle.definition, harness.lifecycle.running)
	}
	if harness.lifecycle.snapshotCalls != 1 || harness.lifecycle.restoreCalls != 1 {
		t.Fatalf("uninstall rollback lifecycle = %#v", harness.lifecycle)
	}
}

func TestInstallerUninstallStateRemovalFailureRestoresExactSnapshot(t *testing.T) {
	harness := newInstallerHarness(t)
	oldBody := []byte("owned")
	harness.seedInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	harness.lifecycle.modelServiceState = true
	harness.lifecycle.definition = "custom-owned-definition"
	harness.lifecycle.running = false
	harness.fsys.failRemoveTarget = harness.store.Path()
	harness.fsys.failRemoveCount = 1

	err := harness.installer.Uninstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remove CQ installation ownership") {
		t.Fatalf("Uninstall() error = %v", err)
	}
	harness.assertInstalled(t, oldBody, "0.26.2", installstate.OwnerGo)
	if harness.lifecycle.definition != "custom-owned-definition" || harness.lifecycle.running {
		t.Fatalf("restored service state = definition %q running %t", harness.lifecycle.definition, harness.lifecycle.running)
	}
	if harness.lifecycle.snapshotCalls != 1 || harness.lifecycle.restoreCalls != 1 {
		t.Fatalf("uninstall rollback lifecycle = %#v", harness.lifecycle)
	}
}

func TestInstallerUninstallIsIdempotentWhenNothingIsOwned(t *testing.T) {
	harness := newInstallerHarness(t)
	if err := harness.installer.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if harness.lifecycle.totalCalls() != 0 || harness.metadata.totalCalls() != 0 {
		t.Fatal("empty uninstall mutated services or metadata")
	}
}

func TestInstallerReadBinaryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "cq")
	if err := os.WriteFile(target, []byte("cq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	installer := Installer{FS: fsutil.OSFileSystem{}}
	if _, _, _, err := installer.readBinary(link); err == nil {
		t.Fatal("symlink executable accepted")
	}
}

type installerHarness struct {
	installer  *Installer
	fsys       *failingInstallerFS
	store      installstate.Store
	roots      userdirs.Roots
	downloader *fakeInstallerDownloader
	runner     *fakeInstallerRunner
	lifecycle  *fakeInstallerLifecycle
	metadata   *fakeInstallerMetadata
	locker     *fakeInstallerLocker
	temporary  *fakeTemporaryDirectories
}

func newInstallerHarness(t *testing.T) *installerHarness {
	t.Helper()
	root := filepath.Clean(t.TempDir())
	mem := fsutil.NewMemFS()
	fsys := &failingInstallerFS{MemFS: mem}
	roots := userdirs.Roots{
		Config:  filepath.Join(root, "config"),
		State:   filepath.Join(root, "state"),
		Cache:   filepath.Join(root, "cache"),
		Runtime: filepath.Join(root, "runtime"),
		Logs:    filepath.Join(root, "logs"),
	}
	for _, directory := range []string{roots.State, filepath.Join(root, "bin"), filepath.Join(root, "tmp")} {
		if err := fsys.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(root, "bin", installerExecutableName())
	downloader := &fakeInstallerDownloader{fsys: fsys, executableName: filepath.Base(executable), body: []byte("new-binary")}
	runner := &fakeInstallerRunner{version: "0.27.0"}
	lifecycle := &fakeInstallerLifecycle{}
	metadata := &fakeInstallerMetadata{}
	locker := &fakeInstallerLocker{}
	temporary := &fakeTemporaryDirectories{fsys: fsys, root: filepath.Join(root, "tmp")}
	store := installstate.Store{FS: fsys, Roots: roots}
	installation := Installation{
		Owner:      installstate.OwnerGo,
		Version:    "0.27.0",
		Executable: executable,
		Services:   []string{"proxy", "refresh"},
	}
	return &installerHarness{
		installer: &Installer{
			FS:           fsys,
			Downloader:   downloader,
			Runner:       runner,
			Lifecycle:    lifecycle,
			Metadata:     metadata,
			State:        store,
			Locker:       locker,
			Temporary:    temporary,
			Installation: installation,
		},
		fsys: fsys, store: store, roots: roots, downloader: downloader, runner: runner,
		lifecycle: lifecycle, metadata: metadata, locker: locker, temporary: temporary,
	}
}

func (harness *installerHarness) seedInstalled(t *testing.T, body []byte, version string, owner installstate.Owner) {
	t.Helper()
	if err := harness.fsys.WriteFile(harness.installer.Installation.Executable, body, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	record := installstate.Record{
		SchemaVersion: installstate.CurrentSchemaVersion,
		Owner:         owner,
		Version:       version,
		Executable:    harness.installer.Installation.Executable,
		BinaryDigest:  fmt.Sprintf("%x", digest),
		Services:      append([]string(nil), harness.installer.Installation.Services...),
	}
	if err := harness.store.Save(record); err != nil {
		t.Fatal(err)
	}
}

func (harness *installerHarness) assertInstalled(t *testing.T, wantBody []byte, wantVersion string, wantOwner installstate.Owner) {
	t.Helper()
	body, err := harness.fsys.ReadFile(harness.installer.Installation.Executable)
	if err != nil || !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("installed body = %q, %v; want %q", body, err, wantBody)
	}
	record, err := harness.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(wantBody)
	if record.Owner != wantOwner || record.Version != wantVersion || record.Executable != harness.installer.Installation.Executable || record.BinaryDigest != fmt.Sprintf("%x", digest) {
		t.Fatalf("installation state = %#v", record)
	}
}

func installerExecutableName() string {
	if os.PathSeparator == '\\' {
		return "cq.exe"
	}
	return "cq"
}

type failingInstallerFS struct {
	*fsutil.MemFS
	failRenameDestination string
	failRenameCount       int
	failRemoveTarget      string
	failRemoveCount       int
	failRemoveError       error
	renameCalls           int
}

type pathRestrictedInstallerFS struct {
	*fsutil.MemFS
}

func (*pathRestrictedInstallerFS) CreateExclusive(string, os.FileMode) (fsutil.DurableFile, error) {
	return nil, fsutil.ErrUnsafeSecurePath
}

func (fsys *failingInstallerFS) Rename(oldPath, newPath string) error {
	fsys.renameCalls++
	if newPath == fsys.failRenameDestination && fsys.failRenameCount > 0 {
		fsys.failRenameCount--
		return errors.New("injected replacement failure")
	}
	return fsys.MemFS.Rename(oldPath, newPath)
}

func (fsys *failingInstallerFS) Remove(path string) error {
	if path == fsys.failRemoveTarget && fsys.failRemoveCount > 0 {
		fsys.failRemoveCount--
		if fsys.failRemoveError != nil {
			return fsys.failRemoveError
		}
		return errors.New("injected removal failure")
	}
	return fsys.MemFS.Remove(path)
}

type fakeInstallerDownloader struct {
	fsys           fsutil.FileSystem
	executableName string
	body           []byte
	err            error
	calls          int
}

func (downloader *fakeInstallerDownloader) Download(_ context.Context, destination string) (StagedBinary, error) {
	downloader.calls++
	if downloader.err != nil {
		return StagedBinary{}, downloader.err
	}
	path := filepath.Join(destination, downloader.executableName)
	if err := downloader.fsys.WriteFile(path, downloader.body, 0o700); err != nil {
		return StagedBinary{}, err
	}
	digest := sha256.Sum256(downloader.body)
	return StagedBinary{Path: path, Digest: fmt.Sprintf("%x", digest)}, nil
}

type fakeInstallerRunner struct {
	version string
	err     error
	calls   []string
}

type fakeBinaryClassifier struct {
	classification BinaryOwnership
	err            error
}

func (classifier fakeBinaryClassifier) Classify(installstate.Owner, string) (BinaryOwnership, error) {
	return classifier.classification, classifier.err
}

func (runner *fakeInstallerRunner) Version(_ context.Context, executable string) (string, error) {
	runner.calls = append(runner.calls, executable)
	return runner.version, runner.err
}

type fakeInstallerLifecycle struct {
	stopCalls              int
	installCalls           int
	snapshotCalls          int
	restoreCalls           int
	statusCalls            int
	uninstallCalls         int
	installErrors          []error
	statusErrors           []error
	uninstallErrors        []error
	restoreErrors          []error
	mutateOnUninstallError bool
	modelServiceState      bool
	definition             string
	running                bool
	savedDefinition        string
	savedRunning           bool
}

func (lifecycle *fakeInstallerLifecycle) Stop(context.Context) error {
	lifecycle.stopCalls++
	if lifecycle.modelServiceState {
		lifecycle.definition = ""
		lifecycle.running = false
	}
	return nil
}

func (lifecycle *fakeInstallerLifecycle) Install(_ context.Context, _ installstate.Owner) error {
	lifecycle.installCalls++
	err := popInstallerError(&lifecycle.installErrors)
	if err == nil && lifecycle.modelServiceState {
		if lifecycle.installCalls == 1 {
			lifecycle.definition = "candidate-definition"
		} else {
			lifecycle.definition = "rendered-default-definition"
		}
		lifecycle.running = true
	}
	return err
}

func (lifecycle *fakeInstallerLifecycle) Snapshot(_ context.Context, _ installstate.Owner, _ string) error {
	lifecycle.snapshotCalls++
	lifecycle.savedDefinition = lifecycle.definition
	lifecycle.savedRunning = lifecycle.running
	return nil
}

func (lifecycle *fakeInstallerLifecycle) Restore(_ context.Context, _ installstate.Owner, _ string) error {
	lifecycle.restoreCalls++
	if err := popInstallerError(&lifecycle.restoreErrors); err != nil {
		return err
	}
	if lifecycle.modelServiceState {
		lifecycle.definition = lifecycle.savedDefinition
		lifecycle.running = lifecycle.savedRunning
	}
	return nil
}

func (lifecycle *fakeInstallerLifecycle) Status(context.Context) error {
	lifecycle.statusCalls++
	return popInstallerError(&lifecycle.statusErrors)
}

func (lifecycle *fakeInstallerLifecycle) Uninstall(_ context.Context, _ installstate.Owner) error {
	lifecycle.uninstallCalls++
	err := popInstallerError(&lifecycle.uninstallErrors)
	if lifecycle.modelServiceState && (err == nil || lifecycle.mutateOnUninstallError) {
		lifecycle.definition = ""
		lifecycle.running = false
	}
	return err
}

func (lifecycle *fakeInstallerLifecycle) totalCalls() int {
	return lifecycle.stopCalls + lifecycle.installCalls + lifecycle.snapshotCalls + lifecycle.restoreCalls + lifecycle.statusCalls + lifecycle.uninstallCalls
}

type fakeInstallerMetadata struct {
	installCalls  int
	removeCalls   int
	inspectCalls  int
	installErrors []error
	removeErrors  []error
	inspectErrors []error
}

func (metadata *fakeInstallerMetadata) Install(context.Context, Installation) error {
	metadata.installCalls++
	return popInstallerError(&metadata.installErrors)
}

func (metadata *fakeInstallerMetadata) Remove(context.Context, Installation) error {
	metadata.removeCalls++
	return popInstallerError(&metadata.removeErrors)
}

func (metadata *fakeInstallerMetadata) Inspect(context.Context, Installation) error {
	metadata.inspectCalls++
	return popInstallerError(&metadata.inspectErrors)
}

func (metadata *fakeInstallerMetadata) totalCalls() int {
	return metadata.installCalls + metadata.removeCalls + metadata.inspectCalls
}

func popInstallerError(errorsList *[]error) error {
	if len(*errorsList) == 0 {
		return nil
	}
	err := (*errorsList)[0]
	*errorsList = (*errorsList)[1:]
	return err
}

type fakeInstallerLocker struct {
	err          error
	acquireCalls int
	closeCalls   int
}

func (locker *fakeInstallerLocker) Acquire() (InstallLock, error) {
	locker.acquireCalls++
	if locker.err != nil {
		return nil, locker.err
	}
	return fakeInstallLock{close: func() { locker.closeCalls++ }}, nil
}

type fakeInstallLock struct {
	close func()
}

func (lock fakeInstallLock) Close() error {
	lock.close()
	return nil
}

type fakeTemporaryDirectories struct {
	fsys        fsutil.FileSystem
	root        string
	createCalls int
	removeCalls int
	lastPath    string
}

func (temporary *fakeTemporaryDirectories) Create() (string, error) {
	temporary.createCalls++
	temporary.lastPath = filepath.Join(temporary.root, fmt.Sprintf("install-%d", temporary.createCalls))
	if err := temporary.fsys.MkdirAll(temporary.lastPath, 0o700); err != nil {
		return "", err
	}
	return temporary.lastPath, nil
}

func (temporary *fakeTemporaryDirectories) Remove(path string) error {
	temporary.removeCalls++
	entries, _ := temporary.fsys.ReadDir(path)
	for _, entry := range entries {
		if err := temporary.fsys.Remove(filepath.Join(path, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := temporary.fsys.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
