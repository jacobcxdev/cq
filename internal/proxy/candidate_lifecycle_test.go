package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCandidateLifecyclePrepareAndReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "candidate")
	registry, registryDigest := candidateRegistryForTest()
	store, state, err := PrepareCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, CandidatePrepareInputV1{
		Root: root, Port: 29280,
		SourceConfigDigest: strings.Repeat("1", 64), TargetReleaseBundleDigest: strings.Repeat("2", 64),
		TargetReleaseSetDigest: strings.Repeat("3", 64), ClientBuild: "codex-test",
		ClientExecutableDigest: strings.Repeat("4", 64), LocalTokenClientRegistryDigest: registryDigest, LocalTokenClientRegistry: registry,
		CredentialMode: "none",
	}, rand.Reader, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != CandidatePhasePrepared || state.ValidationRunID == "" || state.OperationID == "" || state.Port != 29280 {
		t.Fatalf("state = %#v", state)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		name string
		mode os.FileMode
	}{{".", 0o700}, {candidateLifecycleKeyName, 0o600}, {candidateLifecycleStateName, 0o600}} {
		info, err := os.Stat(filepath.Join(root, entry.name))
		if err != nil || info.Mode().Perm() != entry.mode {
			t.Fatalf("%s mode = %v, err = %v", entry.name, info.Mode().Perm(), err)
		}
	}
	reopened, got, err := OpenCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got != state {
		t.Fatalf("reopened = %#v, want %#v", got, state)
	}
	if _, _, err := PrepareCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, CandidatePrepareInputV1{
		Root: root, Port: 29281, SourceConfigDigest: strings.Repeat("1", 64),
		TargetReleaseBundleDigest: strings.Repeat("2", 64), TargetReleaseSetDigest: strings.Repeat("3", 64),
		ClientBuild: "codex-test", ClientExecutableDigest: strings.Repeat("4", 64),
		LocalTokenClientRegistryDigest: registryDigest, LocalTokenClientRegistry: registry, CredentialMode: "none",
	}, rand.Reader, time.Now); !errors.Is(err, ErrCandidateLifecycleExists) {
		t.Fatalf("second prepare error = %v", err)
	}
}

func TestCandidateLifecycleExternalEffectIsNeverReplayed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "candidate")
	registry, registryDigest := candidateRegistryForTest()
	store, _, err := PrepareCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, CandidatePrepareInputV1{
		Root: root, Port: 29280,
		SourceConfigDigest: strings.Repeat("1", 64), TargetReleaseBundleDigest: strings.Repeat("2", 64),
		TargetReleaseSetDigest: strings.Repeat("3", 64), ClientBuild: "codex-test",
		ClientExecutableDigest: strings.Repeat("4", 64), LocalTokenClientRegistryDigest: registryDigest, LocalTokenClientRegistry: registry,
		CredentialMode: "none",
	}, rand.Reader, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	want := errors.New("crash after effect")
	store.hook = func(point string) error {
		if point == "effect_returned" {
			return want
		}
		return nil
	}
	if _, err := store.Apply(context.Background(), CandidateActionStart, func(CandidateLifecycleStateV1) (string, error) {
		called++
		return strings.Repeat("a", 64), nil
	}); !errors.Is(err, want) {
		t.Fatalf("Apply error = %v", err)
	}
	if called != 1 {
		t.Fatalf("calls = %d", called)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, state, err := OpenCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if state.PendingAction != CandidateActionStart || !state.EffectStarted || state.EffectReceiptDigest != "" {
		t.Fatalf("pending state = %#v", state)
	}
	if _, err := reopened.Apply(context.Background(), CandidateActionStart, func(CandidateLifecycleStateV1) (string, error) {
		called++
		return strings.Repeat("b", 64), nil
	}); !errors.Is(err, ErrCandidateEffectIndeterminate) {
		t.Fatalf("replay error = %v", err)
	}
	if called != 1 {
		t.Fatalf("replayed calls = %d", called)
	}
	state, err = reopened.Reconcile(context.Background(), CandidateActionStart, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != CandidatePhaseRunning || state.PendingAction != "" || state.EffectReceiptDigest != strings.Repeat("a", 64) {
		t.Fatalf("reconciled state = %#v", state)
	}
}

func candidateRegistryForTest() ([]byte, string) {
	body := []byte(`{"schema_version":1,"revision":1,"senders":[]}`)
	digest := sha256.Sum256(body)
	return body, hex.EncodeToString(digest[:])
}

func TestCandidateLifecycleRejectsUnsafeInputBeforeCreation(t *testing.T) {
	base := t.TempDir()
	for name, input := range map[string]CandidatePrepareInputV1{
		"live port":      {Root: filepath.Join(base, "live"), Port: DefaultPort},
		"relative root":  {Root: "candidate", Port: 29280},
		"missing digest": {Root: filepath.Join(base, "digest"), Port: 29280},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := PrepareCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, input, rand.Reader, time.Now); err == nil {
				t.Fatal("invalid input accepted")
			}
			if filepath.IsAbs(input.Root) {
				if _, err := os.Stat(input.Root); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("root created, err = %v", err)
				}
			}
		})
	}
}
