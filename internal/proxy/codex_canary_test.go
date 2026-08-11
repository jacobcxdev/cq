package proxy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCodexCanaryPersistsNamedPrivacySafeProtectedDigests(t *testing.T) {
	fsys := fsutil.NewMemFS()
	files := map[string]string{
		"/home/.codex/auth.json":                  "system-secret",
		"/home/.codex/accounts/registry.json":     "registry-secret",
		"/home/.codex/accounts/one.auth.json":     "managed-secret",
		"/external/managed-codex-accounts.json":   "manifest-private",
		"/external/managed/account-one/auth.json": "external-secret",
		"/config/proxy.json":                      `{"codex_routing_default_account_key":"opaque-private-default","other":"ignored"}`,
	}
	for path, value := range files {
		if err := fsys.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := fsys.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	protected := []CodexCanaryProtection{
		CodexCanaryFileProtection(CodexCanarySystemAuth, "/home/.codex/auth.json"),
		CodexCanaryFileProtection(CodexCanaryRegistry, "/home/.codex/accounts/registry.json"),
		CodexCanaryDirectoryProtection(CodexCanaryCQManagedAuth, "/home/.codex/accounts", ".auth.json"),
		CodexCanaryOptionalFileProtection(CodexCanaryCodexBarManifest, "/external/managed-codex-accounts.json"),
		CodexCanaryOptionalSnapshotProtection(CodexCanaryCodexBarAuth, func() ([]byte, error) {
			return fsutil.ReadSecureFile(fsys, "/external/managed/account-one/auth.json", codexCanaryStateMaxBytes)
		}),
		CodexCanaryJSONFieldProtection(CodexCanaryRoutingDefault, "/config/proxy.json", "codex_routing_default_account_key"),
	}
	start := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsys, "/state/canary.json", protected, canaryTestTuple(), start)
	if err != nil {
		t.Fatal(err)
	}

	state := recorder.State()
	if state.Version != 2 || len(state.ProtectedDigests) != len(protected) {
		t.Fatalf("state = %+v", state)
	}
	seen := make(map[CodexCanaryProtectionKind]bool, len(protected))
	for _, protectedDigest := range state.ProtectedDigests {
		if seen[protectedDigest.Kind] || len(protectedDigest.Digest) != 64 {
			t.Fatalf("protected digest = %+v", protectedDigest)
		}
		seen[protectedDigest.Kind] = true
	}
	data, err := fsys.ReadFile("/state/canary.json")
	if err != nil {
		t.Fatal(err)
	}
	for path, secret := range files {
		for _, private := range []string{path, secret} {
			if strings.Contains(string(data), private) {
				t.Fatalf("canary leaked private fixture %q", private)
			}
		}
	}
	if strings.Contains(string(data), "other") || !strings.Contains(string(data), `"kind"`) {
		t.Fatalf("persisted state = %s", data)
	}

	if err := fsys.WriteFile("/home/.codex/accounts/one.auth.json", []byte("changed-managed-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordServiceHeartbeat(start); err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordAdmitted(start.Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	state = recorder.State()
	if state.AdmittedTurns != 1 || state.AutomaticHashChanges != 1 || state.ConsecutiveCalendarDays != 2 {
		t.Fatalf("state = %+v", state)
	}
}

func TestCodexCanaryOptionalAbsentProtectionIsStableEvidence(t *testing.T) {
	fsys := fsutil.NewMemFS()
	protected := []CodexCanaryProtection{
		CodexCanaryOptionalFileProtection(CodexCanaryCodexBarManifest, "/optional/manifest.json"),
		CodexCanaryOptionalSnapshotProtection(CodexCanaryCodexBarAuth, func() ([]byte, error) {
			return nil, os.ErrNotExist
		}),
	}
	recorder, err := StartCodexCanary(fsys, "/state/canary.json", protected, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before := recorder.State().ProtectedDigests
	if err := recorder.RecordServiceHeartbeat(time.Now()); err != nil {
		t.Fatal(err)
	}
	after := recorder.State().ProtectedDigests
	if len(before) != 2 || len(after) != 2 || before[0] != after[0] || before[1] != after[1] {
		t.Fatalf("absence evidence changed: before=%+v after=%+v", before, after)
	}
	if recorder.State().AutomaticHashChanges != 0 {
		t.Fatalf("state = %+v", recorder.State())
	}
}

func TestCodexCanaryRequiredProtectedStateMustExist(t *testing.T) {
	privatePath := "/synthetic/private/system-auth.json"
	_, err := StartCodexCanary(fsutil.NewMemFS(), "/state/canary.json", []CodexCanaryProtection{
		CodexCanaryFileProtection(CodexCanarySystemAuth, privatePath),
	}, canaryTestTuple(), time.Now())
	if err == nil {
		t.Fatal("expected required protected state rejection")
	}
	if strings.Contains(err.Error(), privatePath) {
		t.Fatalf("protected-state error contains path: %v", err)
	}
}

func TestCodexCanaryProtectedFileRejectsSymlinkWithoutReadingTarget(t *testing.T) {
	root := t.TempDir()
	protectedDirectory := filepath.Join(root, "protected")
	if err := os.MkdirAll(protectedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(protectedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "private-target.json")
	if err := os.WriteFile(target, []byte("private-target-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	protectedPath := filepath.Join(protectedDirectory, "auth.json")
	if err := os.Symlink(target, protectedPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := StartCodexCanary(fsutil.OSFileSystem{}, filepath.Join(root, "canary.json"), []CodexCanaryProtection{
		CodexCanaryFileProtection(CodexCanarySystemAuth, protectedPath),
	}, canaryTestTuple(), time.Now())
	if err == nil {
		t.Fatal("expected protected symlink rejection")
	}
	for _, private := range []string{protectedPath, target, "private-target-value"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("protected-state error contains private source data: %v", err)
		}
	}
}

func TestCodexCanaryRecordsProtectedStateReadFailure(t *testing.T) {
	fsys := fsutil.NewMemFS()
	directory := "/managed"
	if err := fsys.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(directory+"/one.auth.json", []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, err := StartCodexCanary(fsys, "/state/canary.json", []CodexCanaryProtection{
		CodexCanaryDirectoryProtection(CodexCanaryCQManagedAuth, directory, ".auth.json"),
	}, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.Remove(directory + "/one.auth.json"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkdirAll(directory+"/unsafe.auth.json", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(directory+"/unsafe.auth.json/child", []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordAdmitted(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().ProtectedStateFailures; got != 1 {
		t.Fatalf("protected state failures = %d, want 1", got)
	}
}

func TestCodexCanaryProtectedSnapshotPanicFailsClosed(t *testing.T) {
	privatePanic := "private protected-state panic value"
	protected := CodexCanaryProtection{
		Kind: CodexCanarySystemAuth,
		snapshot: func(fsutil.FileSystem) ([]byte, error) {
			panic(privatePanic)
		},
	}
	var (
		startErr error
		panicked any
	)
	func() {
		defer func() { panicked = recover() }()
		_, startErr = StartCodexCanary(
			fsutil.NewMemFS(),
			"/state/canary.json",
			[]CodexCanaryProtection{protected},
			canaryTestTuple(),
			time.Now(),
		)
	}()
	if panicked != nil {
		t.Fatalf("protected snapshot panic escaped: %v", panicked)
	}
	if startErr == nil {
		t.Fatal("expected protected snapshot failure")
	}
	if strings.Contains(startErr.Error(), privatePanic) {
		t.Fatalf("protected snapshot error leaked panic value: %v", startErr)
	}
}

func TestCodexCanaryActiveSnapshotPanicRecordsFailure(t *testing.T) {
	calls := 0
	protected := CodexCanaryProtection{
		Kind: CodexCanarySystemAuth,
		snapshot: func(fsutil.FileSystem) ([]byte, error) {
			calls++
			if calls > 1 {
				panic("private active protected-state panic value")
			}
			return []byte("initial protected value"), nil
		},
	}
	recorder, err := StartCodexCanary(
		fsutil.NewMemFS(),
		"/state/canary.json",
		[]CodexCanaryProtection{protected},
		canaryTestTuple(),
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		err = recorder.RecordServiceHeartbeat(time.Now())
	}()
	if panicked != nil {
		t.Fatalf("active protected snapshot panic escaped: %v", panicked)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().ProtectedStateFailures; got != 1 {
		t.Fatalf("protected state failures = %d, want 1", got)
	}
}

func TestCodexCanaryManagedDirectoryRejectsPathReplacementDuringSnapshot(t *testing.T) {
	const (
		directory         = "/managed"
		replacementSecret = "private replacement credential"
	)
	fsys := &replacingCanaryManagedDirectoryFS{
		MemFS:             fsutil.NewMemFS(),
		directory:         directory,
		replacementSecret: replacementSecret,
	}
	if err := fsys.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(directory+"/one.auth.json", []byte("initial credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := StartCodexCanary(fsys, "/state/canary.json", []CodexCanaryProtection{
		CodexCanaryDirectoryProtection(CodexCanaryCQManagedAuth, directory, ".auth.json"),
	}, canaryTestTuple(), time.Now())
	if err == nil {
		t.Fatal("expected managed directory replacement rejection")
	}
	for _, private := range []string{directory, replacementSecret} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("managed directory error contains private data: %v", err)
		}
	}
}

type replacingCanaryManagedDirectoryFS struct {
	*fsutil.MemFS
	directory         string
	replacementSecret string
	replaced          bool
}

func (fsys *replacingCanaryManagedDirectoryFS) ReadDir(path string) ([]os.DirEntry, error) {
	if filepath.Clean(path) == filepath.Clean(fsys.directory) {
		fsys.replaceDirectory()
	}
	return fsys.MemFS.ReadDir(path)
}

func (fsys *replacingCanaryManagedDirectoryFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(path) != filepath.Clean(fsys.directory) {
		return directory, nil
	}
	return &replacingCanaryManagedDirectory{
		SecureDirectory: directory,
		reader:          directory.(fsutil.SecureDirectoryReader),
		fsys:            fsys,
	}, nil
}

func (fsys *replacingCanaryManagedDirectoryFS) replaceDirectory() {
	if fsys.replaced {
		return
	}
	fsys.replaced = true
	_ = fsys.MemFS.Remove(fsys.directory)
	_ = fsys.MemFS.MkdirAll(fsys.directory, 0o700)
	_ = fsys.MemFS.WriteFile(fsys.directory+"/one.auth.json", []byte(fsys.replacementSecret), 0o600)
}

type replacingCanaryManagedDirectory struct {
	fsutil.SecureDirectory
	reader fsutil.SecureDirectoryReader
	fsys   *replacingCanaryManagedDirectoryFS
}

func (directory *replacingCanaryManagedDirectory) ReadDir() ([]os.DirEntry, error) {
	entries, err := directory.reader.ReadDir()
	if err == nil {
		directory.fsys.replaceDirectory()
	}
	return entries, err
}

func TestCodexCanaryDoesNotExposeManualProtectedBaselineReset(t *testing.T) {
	fsys := fsutil.NewMemFS()
	authPath := "/home/.codex/auth.json"
	managedPath := "/home/.codex/accounts/one.auth.json"
	if err := fsys.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkdirAll(filepath.Dir(managedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = fsys.WriteFile(authPath, []byte("before-auth"), 0o600)
	_ = fsys.WriteFile(managedPath, []byte("before-managed"), 0o600)
	recorder, err := StartCodexCanary(fsys, "/state/canary.json", []CodexCanaryProtection{
		CodexCanaryFileProtection(CodexCanarySystemAuth, authPath),
		CodexCanaryDirectoryProtection(CodexCanaryCQManagedAuth, "/home/.codex/accounts", ".auth.json"),
	}, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = fsys.WriteFile(authPath, []byte("explicit-auth"), 0o600)
	_ = fsys.WriteFile(managedPath, []byte("unexpected-managed"), 0o600)
	if err := recorder.RecordAdmitted(time.Now()); err != nil {
		t.Fatal(err)
	}
	if recorder.State().AutomaticHashChanges != 1 {
		t.Fatalf("changed protected state was not recorded: %+v", recorder.State())
	}
	if err := recorder.RecordAdmitted(time.Now()); err != nil {
		t.Fatal(err)
	}
	if recorder.State().AutomaticHashChanges != 1 {
		t.Fatalf("unchanged protected baseline counted twice: %+v", recorder.State())
	}
}

func TestCodexCanaryCLIOnlyLifecycleDoesNotMintServiceObservation(t *testing.T) {
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsutil.NewMemFS(), "/state/canary.json", nil, canaryTestTuple(), start)
	if err != nil {
		t.Fatal(err)
	}
	state := recorder.State()
	if !state.LastObservedAt.IsZero() || state.ConsecutiveCalendarDays != 0 {
		t.Fatalf("start minted service observation: %+v", state)
	}
	finaliseCodexCanaryForTest(t, recorder, start.Add(8*24*time.Hour))
	state = recorder.State()
	if !state.LastObservedAt.IsZero() || state.ConsecutiveCalendarDays != 0 {
		t.Fatalf("stop minted service observation: %+v", state)
	}
}

func TestCodexCanaryRejectsMutationsAfterStop(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/state/canary.json"
	recorder, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	finaliseCodexCanaryForTest(t, recorder, time.Now())
	before, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	generation := recorder.generation
	mutations := []struct {
		name string
		run  func() error
	}{
		{name: "admission", run: func() error { return recorder.RecordAdmitted(time.Now()) }},
		{name: "heartbeat", run: func() error { return recorder.RecordServiceHeartbeat(time.Now()) }},
		{name: "mismatch", run: recorder.RecordKeyedMismatch},
		{name: "lifecycle", run: recorder.RecordUnexplainedLifecycle},
		{name: "secret leak", run: recorder.RecordSecretLeak},
		{name: "live repair", run: recorder.RecordLiveSessionRepair},
		{name: "duplicate stop", run: func() error {
			_, err := recorder.finaliseCodexCanaryStop(time.Now(), codexCanaryClaimedStop{}, [32]byte{1}, 0)
			return err
		}},
	}
	for _, mutation := range mutations {
		if err := mutation.run(); err == nil {
			t.Errorf("%s mutation succeeded after stop", mutation.name)
		}
	}
	if recorder.generation != generation {
		t.Fatalf("generation advanced after stop: %d, want %d", recorder.generation, generation)
	}
	after, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("persisted state changed after stop")
	}
}

func TestCodexCanaryConsecutiveDaysResetAfterGap(t *testing.T) {
	start := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	recorder, err := StartCodexCanary(fsutil.NewMemFS(), "/state/canary.json", nil, canaryTestTuple(), start)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordServiceHeartbeat(start); err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordServiceHeartbeat(start.Add(72 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().ConsecutiveCalendarDays; got != 1 {
		t.Fatalf("days after gap = %d", got)
	}
	if err := recorder.RecordServiceHeartbeat(start.Add(96 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := recorder.State().ConsecutiveCalendarDays; got != 2 {
		t.Fatalf("days after next observation = %d", got)
	}
}

func TestCodexCanaryRecordsFailureCounters(t *testing.T) {
	recorder, err := StartCodexCanary(fsutil.NewMemFS(), "/state/canary.json", nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordSecretLeak(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordLiveSessionRepair(); err != nil {
		t.Fatal(err)
	}
	state := recorder.State()
	if state.SecretLeaks != 1 || state.LiveSessionRepairs != 1 {
		t.Fatalf("state = %+v", state)
	}
}

func TestBuildCodexCanaryTupleBindsCurrentReadinessMarker(t *testing.T) {
	required := testCodexRequirements(CodexRoutingHTTP)
	marker := testCodexMarker(required)
	tuple, err := BuildCodexCanaryTuple(required, marker)
	if err != nil {
		t.Fatal(err)
	}
	if tuple.CQBuild != required.CQBuild || tuple.ClientBuild != required.ClientBuild ||
		tuple.ParserSchema != required.ParserSchema || tuple.LeaseSchema != required.LeaseSchema ||
		tuple.SemanticsRevision != required.SemanticsRevision || tuple.RetryBudget != required.RetryBudget ||
		tuple.FixtureHash != required.FixtureHash || tuple.ReadinessFingerprint != markerFingerprint(marker) {
		t.Fatalf("tuple = %+v", tuple)
	}

	marker.ClientBuild = "stale-client"
	if _, err := BuildCodexCanaryTuple(required, marker); err == nil {
		t.Fatal("expected stale readiness marker rejection")
	}
}

func TestCodexCanaryRecorderRejectsRuntimeTupleDrift(t *testing.T) {
	recorder, err := StartCodexCanary(fsutil.NewMemFS(), "/state/canary.json", nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.ValidateTuple(canaryTestTuple()); err != nil {
		t.Fatal(err)
	}
	drifted := canaryTestTuple()
	drifted.SemanticsRevision = "stale-semantics"
	if err := recorder.ValidateTuple(drifted); err == nil {
		t.Fatal("expected runtime tuple drift rejection")
	}
}

func TestOpenCodexCanaryRejectsVersionOneEvidence(t *testing.T) {
	fsys := fsutil.NewMemFS()
	data, err := json.Marshal(CodexCanaryState{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/canary.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCodexCanary(fsys, "/state/canary.json", nil); err == nil {
		t.Fatal("expected v1 canary rejection")
	}
}

func TestOpenCodexCanaryRejectsEditedSignedState(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/state/canary.json"
	if _, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope codexCanaryEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.State.AdmittedTurns = 100
	edited, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCodexCanary(fsys, path, nil); err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("edited state error = %v", err)
	}
}

func TestOpenCodexCanaryRejectsNonCanonicalSignedState(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/state/canary.json"
	if _, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["unknown"] = "unsigned-extension"
	modified, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(path, modified, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCodexCanary(fsys, path, nil); err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("non-canonical state error = %v", err)
	}
}

func TestOpenCodexCanaryRequiresOriginalIntegrityKey(t *testing.T) {
	for _, test := range []struct {
		name       string
		replaceKey bool
	}{
		{name: "missing"},
		{name: "replaced", replaceKey: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			path := "/state/canary.json"
			if _, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := fsys.Remove(path + ".key"); err != nil {
				t.Fatal(err)
			}
			if test.replaceKey {
				if err := fsys.WriteFile(path+".key", []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := OpenCodexCanary(fsys, path, nil); err == nil {
				t.Fatal("expected integrity key rejection")
			}
		})
	}
}

func TestOpenCodexCanaryVerifiesPersistedGeneration(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/state/canary.json"
	recorder, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordSecretLeak(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCodexCanary(fsys, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.generation != 2 || reopened.State().SecretLeaks != 1 {
		t.Fatalf("reopened generation/state = %d %+v", reopened.generation, reopened.State())
	}
}

func TestOpenCodexCanaryIsReadOnly(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/state/canary.json"
	owner, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenCodexCanary(fsys, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := readOnly.RecordSecretLeak(); err == nil {
		t.Fatal("read-only recorder mutated state")
	}
	after, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("read-only recorder changed persisted state")
	}
}

func TestOpenServingCodexCanaryRetainsSingleWriterLock(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/state/canary.json"
	initial, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := OpenServingCodexCanary(fsys, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := OpenServingCodexCanary(fsys, path, nil); err == nil {
		t.Fatal("second serving owner acquired canary")
	}
	if _, err := OpenCodexCanary(fsys, path, nil); err != nil {
		t.Fatalf("read-only open while serving owner active: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenServingCodexCanary(fsys, path, nil)
	if err != nil {
		t.Fatalf("serving owner after release: %v", err)
	}
	if err := second.RecordSecretLeak(); err != nil {
		t.Fatalf("serving owner mutation: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.RecordSecretLeak(); err == nil {
		t.Fatal("closed serving owner mutated state")
	}
}

func TestCodexCanaryRunIDIsRandomAndAuthenticated(t *testing.T) {
	fsys := fsutil.NewMemFS()
	path := "/state/canary.json"
	first, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	firstRunID := first.State().RunID
	decoded, err := base64.RawURLEncoding.DecodeString(firstRunID)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("run ID = %q, decoded length %d, error %v", firstRunID, len(decoded), err)
	}
	finaliseCodexCanaryForTest(t, first, time.Now())
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := StartCodexCanary(fsys, path, nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if second.State().RunID == firstRunID {
		t.Fatal("new canary run reused the previous run ID")
	}

	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope codexCanaryEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.State.RunID = ""
	envelope.MAC, err = codexCanaryEnvelopeMAC(second.key, envelope)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCodexCanary(fsys, path, nil); err == nil {
		t.Fatal("expected authenticated empty run ID rejection")
	}
}

func TestCodexCanaryRefusesToReplaceActiveRun(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if _, err := StartCodexCanary(fsys, "/state/canary.json", nil, canaryTestTuple(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := StartCodexCanary(fsys, "/state/canary.json", nil, canaryTestTuple(), time.Now()); !errors.Is(err, ErrCodexCanaryActive) {
		t.Fatalf("second start error = %v", err)
	}
}

func TestCodexCanaryIntegrityKeyIsNotReopenedAfterSecureCreation(t *testing.T) {
	fsys := &countingCanaryKeyOpenFS{MemFS: fsutil.NewMemFS(), keyPath: "/state/canary.json.key"}
	recorder, err := StartCodexCanary(fsys, "/state/canary.json", nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// One absent read plus destination and descriptor-bound verification opens
	// belong to the retained secure-create transaction. No pathname reread follows.
	if got := fsys.keyOpenCount; got != 4 {
		t.Fatalf("integrity key open count after creation = %d, want 4", got)
	}
	finaliseCodexCanaryForTest(t, recorder, time.Now())
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	fsys.keyOpenCount = 0
	if _, err := StartCodexCanary(fsys, "/state/canary.json", nil, canaryTestTuple(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := fsys.keyOpenCount; got != 2 {
		t.Fatalf("integrity key open count for restart = %d, want 2", got)
	}
}

func TestOpenCodexCanaryReadsEnvelopeAndKeyThroughOneRetainedDirectory(t *testing.T) {
	fsys := &countingCanaryKeyOpenFS{MemFS: fsutil.NewMemFS(), keyPath: "/state/canary.json.key"}
	recorder, err := StartCodexCanary(fsys, "/state/canary.json", nil, canaryTestTuple(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	fsys.directoryOpenCount = 0
	fsys.keyOpenCount = 0
	if _, err := OpenCodexCanary(fsys, "/state/canary.json", nil); err != nil {
		t.Fatal(err)
	}
	if got := fsys.directoryOpenCount; got != 1 {
		t.Fatalf("canary directory open count = %d, want 1 retained transaction", got)
	}
	if got := fsys.keyOpenCount; got != 2 {
		t.Fatalf("integrity key open count = %d, want 2", got)
	}
}

type countingCanaryKeyOpenFS struct {
	*fsutil.MemFS
	keyPath            string
	keyOpenCount       int
	directoryOpenCount int
}

func (fsys *countingCanaryKeyOpenFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	fsys.directoryOpenCount++
	directory, err := fsys.MemFS.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	return &countingCanaryKeyOpenDirectory{SecureDirectory: directory, fsys: fsys, path: filepath.Clean(path)}, nil
}

type countingCanaryKeyOpenDirectory struct {
	fsutil.SecureDirectory
	fsys *countingCanaryKeyOpenFS
	path string
}

func (directory *countingCanaryKeyOpenDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	if filepath.Join(directory.path, name) == filepath.Clean(directory.fsys.keyPath) {
		directory.fsys.keyOpenCount++
	}
	return directory.SecureDirectory.OpenNoFollow(name)
}

func canaryTestTuple() CodexCanaryTuple {
	return CodexCanaryTuple{
		CQBuild:              "build",
		ClientBuild:          "client",
		ParserSchema:         1,
		LeaseSchema:          1,
		SemanticsRevision:    "semantics",
		RetryBudget:          1,
		FixtureHash:          "fixture",
		ReadinessFingerprint: strings.Repeat("b", 64),
	}
}

func finaliseCodexCanaryForTest(t *testing.T, recorder *CodexCanaryRecorder, now time.Time) codexCanaryFinalEnvelope {
	t.Helper()
	if err := RequestCodexCanaryStop(recorder.fs, recorder.path, recorder.protected, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := claimCodexCanaryStopRequest(recorder, now)
	if err != nil {
		t.Fatal(err)
	}
	final, err := recorder.finaliseCodexCanaryStop(now, claimed, [32]byte{1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return final
}

func canaryTestProtectedDigests() []CodexCanaryProtectedDigest {
	kinds := []CodexCanaryProtectionKind{
		CodexCanarySystemAuth,
		CodexCanaryRegistry,
		CodexCanaryCQManagedAuth,
		CodexCanaryCodexBarManifest,
		CodexCanaryCodexBarAuth,
		CodexCanaryRoutingDefault,
	}
	result := make([]CodexCanaryProtectedDigest, 0, len(kinds))
	for _, kind := range kinds {
		result = append(result, CodexCanaryProtectedDigest{Kind: kind, Digest: strings.Repeat("a", 64)})
	}
	return result
}
