package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexInstalledHTTPValidationRuntimeCoreIsolatesAuthorityAndCleansUp(t *testing.T) {
	canonicalHome := t.TempDir()
	t.Setenv("HOME", canonicalHome)
	canonicalAuth := filepath.Join(canonicalHome, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(canonicalAuth), 0o700); err != nil {
		t.Fatal(err)
	}
	const sentinel = `{"access_token":"must-not-change"}`
	if err := os.WriteFile(canonicalAuth, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotCodexInstalledValidationTestTree(t, canonicalHome)

	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("newCodexInstalledHTTPValidationRuntimeCore() error = %v", err)
	}
	root := core.tempRoot
	if root == "" || filepath.Clean(root) == filepath.Clean(canonicalHome) {
		t.Fatalf("validation temp root = %q, canonical home = %q", root, canonicalHome)
	}
	if core.nativeHTTPHandler() == nil {
		t.Fatal("nativeHTTPHandler() = nil, want production Codex handler")
	}
	statePath := filepath.Join(root, "cq-state")
	info, err := os.Stat(statePath)
	if err != nil || !info.IsDir() {
		t.Fatalf("isolated CQ state directory %q = %v, %v", statePath, info, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".codex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider credential directory mutated during CQ state setup: %v", err)
	}
	assertCodexInstalledValidationPrivateTree(t, root)

	if err := core.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	if err := core.close(); err != nil {
		t.Fatalf("second close() error = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp root remains after close: %v", err)
	}
	after := snapshotCodexInstalledValidationTestTree(t, canonicalHome)
	if before != after {
		t.Fatalf("canonical authority changed\nbefore: %s\nafter:  %s", before, after)
	}
	data, err := os.ReadFile(canonicalAuth)
	if err != nil || string(data) != sentinel {
		t.Fatalf("canonical auth = %q, %v; want untouched", data, err)
	}
}

func TestCodexInstalledHTTPValidationRuntimeCoreRetainsAuthorityUntilDripBodyDrains(t *testing.T) {
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := core.tempRoot
	body := newCodexNativeHTTPBlockingBody()
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", body)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		core.nativeHTTPHandler().TryServe(httptest.NewRecorder(), request, false)
	}()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("drip-body request did not enter native admission")
	}

	if err := core.closeWithTimeout(20 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked close error = %v, want deadline", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("authority root removed while request remained active: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("released drip-body request did not return")
	}
	if err := core.closeWithTimeout(time.Second); err != nil {
		t.Fatalf("retry close error = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authority root remains after successful retry: %v", err)
	}
}

func TestCodexInstalledHTTPValidationExerciseUsesProductionV2PathForSevenScenarios(t *testing.T) {
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("newCodexInstalledHTTPValidationRuntimeCore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Errorf("close() error = %v", err)
		}
	})

	listenerBinding := sha256.Sum256([]byte("installed-validation-test-listener"))
	probe, err := newCodexInstalledHTTPGateProbe(listenerBinding)
	if err != nil {
		t.Fatalf("newCodexInstalledHTTPGateProbe() error = %v", err)
	}
	detach, err := core.nativeHTTPHandler().installCodexInstalledHTTPGateProbe(probe)
	if err != nil {
		t.Fatalf("installCodexInstalledHTTPGateProbe() error = %v", err)
	}
	t.Cleanup(detach)

	listener := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/responses":
			core.nativeHTTPHandler().TryServe(writer, request, false)
		case "/responses/compact":
			core.nativeHTTPHandler().TryServe(writer, request, true)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(listener.Close)

	exercise, err := core.installedListenerExercise(strings.TrimPrefix(listener.URL, "http://"), testCodexInstalledLocalToken)
	if err != nil {
		t.Fatalf("installedListenerExercise() error = %v", err)
	}
	runtimeBefore := codexProcessRuntimeObservability.snapshot()
	admissionsBefore := core.nativeHTTPAdmissionSnapshot()
	if err := exercise.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	runtimeAfter := codexProcessRuntimeObservability.snapshot()
	admissionsAfter := core.nativeHTTPAdmissionSnapshot()

	after, err := probe.snapshot(context.Background(), listenerBinding)
	if err != nil {
		t.Fatalf("snapshot() error = %v", err)
	}
	health := after.health
	wantPeak := health.Diagnostics.ReplayEnvelopePeakBytes
	health.Diagnostics.ReplayEnvelopePeakBytes = 0
	wantHealth := codexInstalledHTTPProbeHealth{
		ProductionHandlerRequests: 40,
		NativeResponsesRequests:   38,
		NativeCompactRequests:     2,
		StrongTurns:               19,
		Gates: CodexHTTPReadinessGateEvidence{
			InstalledTurns:                      19,
			FrozenSingleTransformEnvelopeCases:  2,
			WarmAffinityCases:                   1,
			DeterministicFallbackCases:          1,
			TerminalDefaultOnceCases:            1,
			ExactPreAdmissionHard429ReplayCases: 2,
			AdmittedNoMigrationCases:            1,
			V2JournalRuntimeCases:               38,
		},
		Acceptance: CodexHTTPAcceptanceResult{
			Turns:                    19,
			Requests:                 40,
			SelectorCalls:            19,
			InstalledRequests:        40,
			InstalledAttempts:        42,
			InstalledSelectorCalls:   19,
			InstalledStrongKeys:      40,
			InstalledZstdRequests:    40,
			InstalledQuiescentLeases: 38,
			HeadroomRequests:         40,
			InstalledResolutions:     42,
		},
		Diagnostics: codexInstalledHTTPAggregateDiagnostics{
			AffinityReuseSelections: 1,
			FairnessSelections:      18,
			TerminalDefaultAttempts: 1,
		},
	}
	if health != wantHealth || wantPeak == 0 {
		t.Fatalf("exact synthetic health = %#v, peak %d; want %#v with nonzero peak", health, wantPeak, wantHealth)
	}
	if runtimeAfter.AffinityReuse-runtimeBefore.AffinityReuse != 1 ||
		runtimeAfter.FairnessSelect-runtimeBefore.FairnessSelect != 20 ||
		runtimeAfter.TerminalDefault-runtimeBefore.TerminalDefault != 1 ||
		runtimeBefore.CurrentReplayBytes != 0 || runtimeAfter.CurrentReplayBytes != 0 ||
		runtimeAfter.PeakReplayBytes != max(runtimeBefore.PeakReplayBytes, wantPeak) {
		t.Fatalf("runtime observability before/after = %#v / %#v, probe peak %d", runtimeBefore, runtimeAfter, wantPeak)
	}
	if delta, err := deltaUint64(admissionsBefore.FirstAuthoritative, admissionsAfter.FirstAuthoritative); err != nil || delta != 19 || admissionsBefore.PromotionBlocked || admissionsAfter.PromotionBlocked {
		t.Fatalf("synthetic first-authoritative admissions = before %#v after %#v delta %d error %v, want exact unblocked 19", admissionsBefore, admissionsAfter, delta, err)
	}

	core.upstream.mu.Lock()
	uniqueTurns := len(core.upstream.turns)
	requests := core.upstream.requests
	responses := core.upstream.responses
	compact := core.upstream.compact
	models := core.upstream.models
	core.upstream.mu.Unlock()
	if uniqueTurns != 20 {
		t.Errorf("unique strong turns = %d, want 20", uniqueTurns)
	}
	if requests != 42 || responses != 40 || compact != 2 || models != 42 {
		t.Errorf("synthetic upstream traffic = requests %d, responses %d, compact %d, models %d; want 42/40/2/42", requests, responses, compact, models)
	}
}

func TestCodexInstalledHTTPValidationExerciseHonoursCancellationWithoutTraffic(t *testing.T) {
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("newCodexInstalledHTTPValidationRuntimeCore() error = %v", err)
	}
	root := core.tempRoot
	t.Cleanup(func() { _ = core.close() })

	exercise, err := core.installedListenerExercise("127.0.0.1:1", testCodexInstalledLocalToken)
	if err != nil {
		t.Fatalf("installedListenerExercise() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exercise.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	core.upstream.mu.Lock()
	requests := core.upstream.requests
	core.upstream.mu.Unlock()
	if requests != 0 {
		t.Fatalf("synthetic upstream requests = %d, want zero", requests)
	}
	if err := core.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp root remains after cancelled exercise close: %v", err)
	}
}

func snapshotCodexInstalledValidationTestTree(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		line := relative + ":" + info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(data)
			line += ":" + string(digest[:])
		}
		entries = append(entries, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(entries, "\n")
}

func assertCodexInstalledValidationPrivateTree(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir() && info.Mode().Perm() != 0o700:
			t.Errorf("directory %q permissions = %04o, want 0700", path, info.Mode().Perm())
		case info.Mode().IsRegular() && info.Mode().Perm() != 0o600:
			t.Errorf("file %q permissions = %04o, want 0600", path, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
