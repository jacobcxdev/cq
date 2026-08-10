package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCreateInstalledHTTPValidationRequestWritesPrivateBoundRequest(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	binding := installedHTTPValidationServiceBinding{
		label:            "homebrew.mxcl.cq",
		executableSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		serviceSHA256:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	store := installedHTTPValidationRequestStore{
		fs:     fsutil.OSFileSystem{},
		path:   path,
		now:    func() time.Time { return now },
		random: bytes.NewReader(bytes.Repeat([]byte{0x5a}, 32)),
		resolveService: func(label string) (installedHTTPValidationServiceBinding, error) {
			if label != "" {
				t.Fatalf("resolve service label = %q, want discovery", label)
			}
			return binding, nil
		},
	}

	if err := createInstalledHTTPValidationRequest(store, "cq-build-42"); err != nil {
		t.Fatalf("createInstalledHTTPValidationRequest: %v", err)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat request directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("request directory mode = %04o, want 0700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat request: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("request mode = %04o, want 0600", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	want := map[string]any{
		"version":                float64(1),
		"nonce":                  "WlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlo",
		"issued_at":              "2026-08-10T11:12:13Z",
		"expires_at":             "2026-08-10T11:14:13Z",
		"cq_build":               "cq-build-42",
		"service_label":          "homebrew.mxcl.cq",
		"cq_executable_sha256":   binding.executableSHA256,
		"service_binding_sha256": binding.serviceSHA256,
	}
	if len(got) != len(want) {
		t.Fatalf("request fields = %#v, want exactly %#v", got, want)
	}
	for key, wantValue := range want {
		if gotValue := got[key]; gotValue != wantValue {
			t.Fatalf("request[%q] = %#v, want %#v", key, gotValue, wantValue)
		}
	}
}

func TestConsumeInstalledHTTPValidationRequestClaimsOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	binding := validInstalledHTTPValidationServiceBinding()
	store := installedHTTPValidationTestStore(t, path, now, binding)
	if err := createInstalledHTTPValidationRequest(store, "cq-build-42"); err != nil {
		t.Fatalf("create request: %v", err)
	}

	present, err := consumeInstalledHTTPValidationRequest(store, "cq-build-42")
	if err != nil {
		t.Fatalf("consume request: %v", err)
	}
	if !present {
		t.Fatal("consume request present = false, want true")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("canonical request stat error = %v, want not exist", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read request directory: %v", err)
	}
	if len(entries) != 2 || entries[0].Name() != installedHTTPValidationLockName || !strings.HasPrefix(entries[1].Name(), "used-") || !strings.HasSuffix(entries[1].Name(), ".json") {
		t.Fatalf("request directory entries = %v, want operation lock and one used nonce tombstone", entryNames(entries))
	}
	if info, err := entries[1].Info(); err != nil {
		t.Fatalf("stat replay tombstone: %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("replay tombstone mode = %04o, want 0600", got)
	}
}

func TestConsumeInstalledHTTPValidationRequestAbsentDoesNotCreateState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "private", "request.json")
	resolveCalls := 0
	store := installedHTTPValidationRequestStore{
		fs:     fsutil.OSFileSystem{},
		path:   path,
		now:    time.Now,
		random: bytes.NewReader(bytes.Repeat([]byte{0x5a}, 32)),
		resolveService: func(string) (installedHTTPValidationServiceBinding, error) {
			resolveCalls++
			return validInstalledHTTPValidationServiceBinding(), nil
		},
	}

	present, err := consumeInstalledHTTPValidationRequest(store, "cq-build-42")
	if err != nil {
		t.Fatalf("consume absent request: %v", err)
	}
	if present {
		t.Fatal("consume absent request present = true, want false")
	}
	if resolveCalls != 0 {
		t.Fatalf("service resolver calls = %d, want 0", resolveCalls)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("request directory stat error = %v, want not exist", err)
	}
}

func TestConsumeInstalledHTTPValidationRequestAbsentIgnoresUnownedDirectory(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "ordinary-cache-directory")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatalf("create ordinary directory: %v", err)
	}
	path := filepath.Join(directory, "request.json")
	store := installedHTTPValidationTestStore(t, path, time.Now(), validInstalledHTTPValidationServiceBinding())

	present, err := consumeInstalledHTTPValidationRequest(store, "cq-build-42")
	if err != nil {
		t.Fatalf("consume absent request: %v", err)
	}
	if present {
		t.Fatal("consume absent request present = true, want false")
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("stat ordinary directory: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("ordinary directory mode = %04o, want unchanged 0755", got)
	}
}

func TestConsumeInstalledHTTPValidationRequestRejectsAndCleansInvalidRequests(t *testing.T) {
	t.Parallel()

	issuedAt := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	for _, test := range []struct {
		name         string
		consumeBuild string
		consumeAt    time.Time
		mutate       func([]byte) []byte
		binding      installedHTTPValidationServiceBinding
	}{
		{
			name:         "malformed",
			consumeBuild: "cq-build-42",
			consumeAt:    issuedAt,
			mutate: func([]byte) []byte {
				return []byte("{not-json}\n")
			},
			binding: validInstalledHTTPValidationServiceBinding(),
		},
		{
			name:         "duplicate field",
			consumeBuild: "cq-build-42",
			consumeAt:    issuedAt,
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte(`{"version":1,`), []byte(`{"version":1,"version":1,`), 1)
			},
			binding: validInstalledHTTPValidationServiceBinding(),
		},
		{
			name:         "expired",
			consumeBuild: "cq-build-42",
			consumeAt:    issuedAt.Add(installedHTTPValidationRequestTTL),
			mutate:       func(data []byte) []byte { return data },
			binding:      validInstalledHTTPValidationServiceBinding(),
		},
		{
			name:         "build mismatch",
			consumeBuild: "cq-build-43",
			consumeAt:    issuedAt,
			mutate:       func(data []byte) []byte { return data },
			binding:      validInstalledHTTPValidationServiceBinding(),
		},
		{
			name:         "service mismatch",
			consumeBuild: "cq-build-42",
			consumeAt:    issuedAt,
			mutate:       func(data []byte) []byte { return data },
			binding: installedHTTPValidationServiceBinding{
				label:            "homebrew.mxcl.cq",
				executableSHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				serviceSHA256:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "private", "request.json")
			createStore := installedHTTPValidationTestStore(t, path, issuedAt, validInstalledHTTPValidationServiceBinding())
			if err := createInstalledHTTPValidationRequest(createStore, "cq-build-42"); err != nil {
				t.Fatalf("create request: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read request: %v", err)
			}
			if err := os.WriteFile(path, test.mutate(data), 0o600); err != nil {
				t.Fatalf("mutate request: %v", err)
			}
			consumeStore := installedHTTPValidationTestStore(t, path, test.consumeAt, test.binding)

			present, err := consumeInstalledHTTPValidationRequest(consumeStore, test.consumeBuild)
			if err == nil {
				t.Fatal("consume invalid request error = nil")
			}
			if present {
				t.Fatal("consume invalid request present = true, want false")
			}
			assertNoPendingInstalledHTTPValidationRequest(t, path)
		})
	}
}

func TestConsumeInstalledHTTPValidationRequestRejectsReplay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	store := installedHTTPValidationTestStore(t, path, now, validInstalledHTTPValidationServiceBinding())
	if err := createInstalledHTTPValidationRequest(store, "cq-build-42"); err != nil {
		t.Fatalf("create request: %v", err)
	}
	requestBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	if present, err := consumeInstalledHTTPValidationRequest(store, "cq-build-42"); err != nil || !present {
		t.Fatalf("first consume = (%t, %v), want (true, nil)", present, err)
	}
	if err := os.WriteFile(path, requestBytes, 0o600); err != nil {
		t.Fatalf("restore replay: %v", err)
	}

	present, err := consumeInstalledHTTPValidationRequest(store, "cq-build-42")
	if err == nil {
		t.Fatal("replay consume error = nil")
	}
	if present {
		t.Fatal("replay consume present = true, want false")
	}
	assertNoPendingInstalledHTTPValidationRequest(t, path)
}

func TestConsumeInstalledHTTPValidationRequestConcurrentClaimHasOneWinner(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	store := installedHTTPValidationTestStore(t, path, now, validInstalledHTTPValidationServiceBinding())
	if err := createInstalledHTTPValidationRequest(store, "cq-build-42"); err != nil {
		t.Fatalf("create request: %v", err)
	}
	type result struct {
		present bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			present, err := consumeInstalledHTTPValidationRequest(store, "cq-build-42")
			results <- result{present: present, err: err}
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		got := <-results
		if got.present && got.err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("successful claim count = %d, want 1", winners)
	}
}

func TestConsumeInstalledHTTPValidationRequestDoesNotEraseLiveClaim(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	binding := validInstalledHTTPValidationServiceBinding()
	createStore := installedHTTPValidationTestStore(t, path, now, binding)
	if err := createInstalledHTTPValidationRequest(createStore, "cq-build-42"); err != nil {
		t.Fatalf("create request: %v", err)
	}
	firstResolving := make(chan struct{})
	allowFirst := make(chan struct{})
	firstStore := installedHTTPValidationTestStore(t, path, now, binding)
	firstStore.resolveService = func(label string) (installedHTTPValidationServiceBinding, error) {
		close(firstResolving)
		<-allowFirst
		return binding, nil
	}
	type result struct {
		present bool
		err     error
	}
	firstResult := make(chan result, 1)
	go func() {
		present, err := consumeInstalledHTTPValidationRequest(firstStore, "cq-build-42")
		firstResult <- result{present: present, err: err}
	}()
	<-firstResolving

	secondPresent, secondErr := consumeInstalledHTTPValidationRequest(
		installedHTTPValidationTestStore(t, path, now, binding),
		"cq-build-42",
	)
	if secondPresent || secondErr == nil {
		t.Fatalf("second consume = (%t, %v), want busy failure", secondPresent, secondErr)
	}
	close(allowFirst)
	first := <-firstResult
	if first.err != nil || !first.present {
		t.Fatalf("first consume = (%t, %v), want success", first.present, first.err)
	}
}

func TestInstalledHTTPValidationRequestOperationsRejectDirectoryReplacement(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	binding := validInstalledHTTPValidationServiceBinding()
	for _, operation := range []string{"create", "consume", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			directoryPath := filepath.Join(root, "private")
			path := filepath.Join(directoryPath, "request.json")
			store := installedHTTPValidationTestStore(t, path, now, binding)
			var receipt installedHTTPValidationRequestReceipt
			var replacement []byte
			if operation != "create" {
				var err error
				receipt, err = createInstalledHTTPValidationRequestWithReceipt(store, "cq-build-42")
				if err != nil {
					t.Fatalf("create fixture request: %v", err)
				}
				replacement, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			replacing := &replaceInstalledHTTPValidationDirectoryFS{
				OSFileSystem: fsutil.OSFileSystem{},
				path:         directoryPath,
				request:      replacement,
			}
			store.fs = replacing
			var err error
			switch operation {
			case "create":
				err = createInstalledHTTPValidationRequest(store, "cq-build-42")
			case "consume":
				_, err = consumeInstalledHTTPValidationRequest(store, "cq-build-42")
			case "cancel":
				_, err = cancelInstalledHTTPValidationRequest(store, receipt)
			}
			if !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
				t.Fatalf("%s directory replacement error = %v, want ErrUnsafeSecurePath", operation, err)
			}
			if operation != "create" {
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, replacement) {
					t.Fatalf("replacement request = %q, %v; want untouched copy", got, readErr)
				}
			}
		})
	}
}

func TestRunProxyValidateHTTPRequestsInstalledStartup(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	restartCalls := 0
	deps := proxyValidateHTTPDependencies{
		store: installedHTTPValidationTestStore(t, path, now, validInstalledHTTPValidationServiceBinding()),
		restart: func() error {
			restartCalls++
			return nil
		},
	}

	if err := runProxyValidateHTTP(nil, deps, "cq-build-42"); err != nil {
		t.Fatalf("runProxyValidateHTTP: %v", err)
	}
	if restartCalls != 1 {
		t.Fatalf("restart calls = %d, want 1", restartCalls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending request stat: %v", err)
	}
}

func TestRunProxyValidateHTTPCancelsRequestWhenKickstartFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	invalidations := 0
	deps := proxyValidateHTTPDependencies{
		store: installedHTTPValidationTestStore(t, path, now, validInstalledHTTPValidationServiceBinding()),
		restart: func() error {
			return errors.New("launch service failed")
		},
		invalidate: func() error {
			invalidations++
			return nil
		},
	}

	err := runProxyValidateHTTP(nil, deps, "cq-build-42")
	if err == nil {
		t.Fatal("runProxyValidateHTTP error = nil")
	}
	assertNoPendingInstalledHTTPValidationRequest(t, path)
	if invalidations != 1 {
		t.Fatalf("readiness invalidations = %d, want 1", invalidations)
	}
}

func TestCancelInstalledHTTPValidationRequestDoesNotDeleteSuccessor(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	firstStore := installedHTTPValidationTestStore(t, path, now, validInstalledHTTPValidationServiceBinding())
	firstReceipt, err := createInstalledHTTPValidationRequestWithReceipt(firstStore, "cq-build-42")
	if err != nil {
		t.Fatalf("create first request: %v", err)
	}
	if present, err := consumeInstalledHTTPValidationRequest(firstStore, "cq-build-42"); err != nil || !present {
		t.Fatalf("consume first request = (%t, %v), want success", present, err)
	}
	secondStore := installedHTTPValidationTestStore(t, path, now, validInstalledHTTPValidationServiceBinding())
	secondStore.random = bytes.NewReader(bytes.Repeat([]byte{0x6b}, 32))
	if _, err := createInstalledHTTPValidationRequestWithReceipt(secondStore, "cq-build-42"); err != nil {
		t.Fatalf("create successor request: %v", err)
	}
	if _, err := cancelInstalledHTTPValidationRequest(firstStore, firstReceipt); err != nil {
		t.Fatalf("cancel first request: %v", err)
	}
	if present, err := consumeInstalledHTTPValidationRequest(secondStore, "cq-build-42"); err != nil || !present {
		t.Fatalf("consume successor request = (%t, %v), want success", present, err)
	}
}

func TestConsumedInstalledHTTPValidationIntentRejectsCancellationBeforeCommit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	store := installedHTTPValidationTestStore(t, path, now, validInstalledHTTPValidationServiceBinding())
	receipt, err := createInstalledHTTPValidationRequestWithReceipt(store, "cq-build-42")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	intent, err := consumeInstalledHTTPValidationRequestWithIntent(store, "cq-build-42")
	if err != nil || intent == nil {
		t.Fatalf("consume request intent = (%#v, %v)", intent, err)
	}
	release, err := intent.Acquire()
	if err != nil {
		t.Fatalf("initial intent guard: %v", err)
	}
	release()
	if outcome, err := cancelInstalledHTTPValidationRequest(store, receipt); err != nil || outcome != installedHTTPValidationCancellationPoisoned {
		t.Fatalf("cancel consumed request = (%d, %v), want poisoned", outcome, err)
	}
	if release, err := intent.Acquire(); err == nil || release != nil {
		t.Fatalf("cancelled intent guard = (release=%t, %v), want failure", release != nil, err)
	}
}

func TestRunProxyValidateHTTPInvalidatesMarkerWhenCommitWinsCancellationLock(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	store := installedHTTPValidationTestStore(t, path, now, validInstalledHTTPValidationServiceBinding())
	committed := make(chan struct{})
	invalidations := 0
	deps := proxyValidateHTTPDependencies{
		store: store,
		restart: func() error {
			intent, err := consumeInstalledHTTPValidationRequestWithIntent(store, "cq-build-42")
			if err != nil || intent == nil {
				return fmt.Errorf("consume request in restarted service: %w", err)
			}
			release, err := intent.Acquire()
			if err != nil {
				return fmt.Errorf("acquire final commit fence: %w", err)
			}
			close(committed)
			go release()
			return errors.New("launch service reported failure after commit")
		},
		invalidate: func() error {
			select {
			case <-committed:
			default:
				t.Fatal("marker invalidation preceded simulated fenced commit")
			}
			invalidations++
			return nil
		},
	}

	if err := runProxyValidateHTTP(nil, deps, "cq-build-42"); err == nil {
		t.Fatal("runProxyValidateHTTP error = nil")
	}
	if invalidations != 1 {
		t.Fatalf("readiness invalidations = %d, want 1", invalidations)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	cancelled := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "cancelled-") {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Fatalf("cancelled poison count = %d, entries %v", cancelled, entryNames(entries))
	}
}

func TestRunProxyValidateHTTPCancelsRequestWhenKickstartPanics(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 10, 11, 12, 13, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "request.json")
	deps := proxyValidateHTTPDependencies{
		store: installedHTTPValidationTestStore(t, path, now, validInstalledHTTPValidationServiceBinding()),
		restart: func() error {
			panic("private launch service panic payload")
		},
	}

	err := runProxyValidateHTTP(nil, deps, "cq-build-42")
	if err == nil || !strings.Contains(err.Error(), "panic") || strings.Contains(err.Error(), "private launch service panic payload") {
		t.Fatalf("runProxyValidateHTTP error = %v, want panic failure", err)
	}
	assertNoPendingInstalledHTTPValidationRequest(t, path)
}

func TestRunProxyValidateHTTPRejectsArgumentsWithoutWriting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "private", "request.json")
	restartCalls := 0
	deps := proxyValidateHTTPDependencies{
		store: installedHTTPValidationTestStore(t, path, time.Now(), validInstalledHTTPValidationServiceBinding()),
		restart: func() error {
			restartCalls++
			return nil
		},
	}

	if err := runProxyValidateHTTP([]string{"unexpected"}, deps, "cq-build-42"); err == nil {
		t.Fatal("runProxyValidateHTTP unexpected argument error = nil")
	}
	if restartCalls != 0 {
		t.Fatalf("restart calls = %d, want 0", restartCalls)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request directory stat error = %v, want not exist", err)
	}
}

func assertNoPendingInstalledHTTPValidationRequest(t *testing.T, path string) {
	t.Helper()
	for _, pending := range []string{path, filepath.Join(filepath.Dir(path), installedHTTPValidationClaimName)} {
		if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pending request %q stat error = %v, want not exist", pending, err)
		}
	}
}

func validInstalledHTTPValidationServiceBinding() installedHTTPValidationServiceBinding {
	return installedHTTPValidationServiceBinding{
		label:            "homebrew.mxcl.cq",
		executableSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		serviceSHA256:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
}

func installedHTTPValidationTestStore(t *testing.T, path string, now time.Time, binding installedHTTPValidationServiceBinding) installedHTTPValidationRequestStore {
	t.Helper()
	return installedHTTPValidationRequestStore{
		fs:     fsutil.OSFileSystem{},
		path:   path,
		now:    func() time.Time { return now },
		random: bytes.NewReader(bytes.Repeat([]byte{0x5a}, 32)),
		resolveService: func(label string) (installedHTTPValidationServiceBinding, error) {
			if label != "" && label != binding.label {
				t.Fatalf("resolve service label = %q, want %q", label, binding.label)
			}
			return binding, nil
		},
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

type replaceInstalledHTTPValidationDirectoryFS struct {
	fsutil.OSFileSystem
	path    string
	request []byte
	once    sync.Once
	err     error
}

func (fsys *replaceInstalledHTTPValidationDirectoryFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	directory, err := fsys.OSFileSystem.OpenSecureDirectory(path)
	if err != nil {
		return nil, err
	}
	if path != fsys.path {
		return directory, nil
	}
	fsys.once.Do(func() {
		if err := os.Rename(path, path+".held"); err != nil {
			fsys.err = err
			return
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			fsys.err = err
			return
		}
		if fsys.request != nil {
			fsys.err = os.WriteFile(filepath.Join(path, "request.json"), fsys.request, 0o600)
		}
	})
	if fsys.err != nil {
		_ = directory.Close()
		return nil, fsys.err
	}
	return directory, nil
}
