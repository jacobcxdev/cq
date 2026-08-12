package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCaptureCodexInstalledProtectedDigestDetectsManagedAccountNamespaceChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "create", mutate: func(t *testing.T, accounts string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(accounts, "second.auth.json"), []byte("second\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "delete", mutate: func(t *testing.T, accounts string) {
			t.Helper()
			if err := os.Remove(filepath.Join(accounts, "first.auth.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "replacement with identical bytes", mutate: func(t *testing.T, accounts string) {
			t.Helper()
			path := filepath.Join(accounts, "first.auth.json")
			replacementPath := path + ".replacement"
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(replacementPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacementPath, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts := filepath.Join(t.TempDir(), "accounts")
			if err := os.Mkdir(accounts, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(accounts, "first.auth.json"), []byte("first\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := captureCodexInstalledProtectedDigest(codexInstalledProtectedPath{path: accounts})
			if err != nil {
				t.Fatalf("capture before: %v", err)
			}
			test.mutate(t, accounts)
			after, err := captureCodexInstalledProtectedDigest(codexInstalledProtectedPath{path: accounts})
			if err != nil {
				t.Fatalf("capture after: %v", err)
			}
			if before == after {
				t.Fatal("managed account namespace change was not detected")
			}
		})
	}
}

func TestCodexInstalledHTTPProtectedFileAcceptsStandardCodexCoreDirectory(t *testing.T) {
	coreDir := filepath.Join(t.TempDir(), ".codex")
	if err := os.Mkdir(coreDir, 0o755); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(coreDir, "auth.json")
	if err := os.WriteFile(authPath, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	protected := codexInstalledProtectedPath{path: authPath, ownerControlledDirectory: true}
	if digest, err := captureCodexInstalledProtectedDigest(protected); err != nil || digest.absent || digest.directory {
		t.Fatalf("standard Codex protected file = %+v, %v", digest, err)
	}
	protected.ownerControlledDirectory = false
	if _, err := captureCodexInstalledProtectedDigest(protected); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("strict private-directory audit error = %v, want unsafe path", err)
	}
}

func TestDefaultCodexInstalledHTTPProtectedPathsIncludeMarkerAndManagedAccounts(t *testing.T) {
	home := t.TempDir()
	markerDir := filepath.Join(t.TempDir(), "cq")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	paths, err := defaultCodexInstalledHTTPProtectedPaths(markerDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(home, ".codex", "auth.json"),
		filepath.Join(home, ".codex", "accounts", "registry.json"),
		filepath.Join(home, ".codex", "accounts"),
		codexReadinessPath(markerDir, CodexRoutingHTTP),
	}
	got := make([]string, len(paths))
	for index := range paths {
		got[index] = paths[index].path
	}
	for _, path := range want {
		if !bytes.Contains([]byte(strings.Join(got, "\n")), []byte(path)) {
			t.Fatalf("protected paths %v omit %q", paths, path)
		}
	}
}

func TestCaptureCodexInstalledProtectedDigestsRejectsWrongDeclaredKind(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "auth.json")
	if err := os.WriteFile(file, []byte("auth\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, protected := range []codexInstalledProtectedPath{
		{path: root},
		{path: file, directory: true},
	} {
		if _, err := captureCodexInstalledProtectedDigests([]codexInstalledProtectedPath{protected}); err == nil {
			t.Fatalf("wrong declared kind accepted for %q", protected.path)
		}
	}
}

func TestCodexInstalledHTTPAuditAuthoritySealsIndependentDeltas(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(root, "auth.json")
	registryPath := filepath.Join(root, "registry.json")
	for _, path := range []string{authPath, registryPath} {
		if err := os.WriteFile(path, []byte("protected\n"), 0o600); err != nil {
			t.Fatalf("write protected file: %v", err)
		}
	}
	privacyRoot := filepath.Join(root, "runtime")
	if err := os.Mkdir(privacyRoot, 0o700); err != nil {
		t.Fatalf("mkdir privacy root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(privacyRoot, "journal"), []byte("privacy-safe-digest\n"), 0o600); err != nil {
		t.Fatalf("write privacy-safe journal: %v", err)
	}
	routes, err := newCodexInstalledHTTPRouteAudit("0.147.0-alpha.6.5", testCodexInstalledLocalToken)
	if err != nil {
		t.Fatalf("new route audit: %v", err)
	}
	outcome := &codexInstalledHTTPClientOutcome{}
	authority := newCodexInstalledHTTPAuditAuthority(codexInstalledHTTPAuditAuthorityConfig{
		routes:         routes,
		client:         outcome,
		protectedPaths: []codexInstalledProtectedPath{{path: authPath}, {path: registryPath}},
		privacyRoot:    privacyRoot,
		privacyNeedles: [][]byte{[]byte("raw-session-id"), []byte("raw-token")},
	})
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	lease, err := authority.Begin(context.Background(), readinessTuple(required), binding)
	if err != nil {
		t.Fatalf("begin audit: %v", err)
	}
	defer lease.Release()

	for _, target := range []string{
		"/models?client_version=0.147.0-alpha.6.5",
		"/models?client_version=0.147.0-alpha.6.5&originator=codex_cli_rs",
	} {
		modelRequest := httptest.NewRequest(http.MethodGet, target, nil)
		modelRequest.Header.Set("Authorization", "Bearer "+testCodexInstalledLocalToken)
		routes.guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), modelRequest)
	}
	outcome.exactPong.Store(true)
	proof, err := lease.Complete(context.Background())
	if err != nil {
		t.Fatalf("complete audit: %v", err)
	}
	if !proof.valid(readinessTuple(required), binding) {
		t.Fatal("sealed audit proof is invalid")
	}
	if proof.modelRequests != 2 || proof.unexpectedRoutes != 0 || proof.rawIdentifierLeaks != 0 ||
		proof.automaticAuthWrites != 0 || proof.egressAttempts != 0 || !proof.exactClientPong {
		t.Fatalf("audit proof = %#v, want two model requests and zero failures", proof)
	}
}

func TestCodexInstalledHTTPAuditAuthorityCountsMutationLeakAndEgress(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(root, "auth.json")
	if err := os.WriteFile(protected, []byte("before\n"), 0o600); err != nil {
		t.Fatalf("write protected file: %v", err)
	}
	privacyRoot := filepath.Join(root, "runtime")
	if err := os.Mkdir(privacyRoot, 0o700); err != nil {
		t.Fatalf("mkdir privacy root: %v", err)
	}
	routes, err := newCodexInstalledHTTPRouteAudit("0.147.0-alpha.6.5", testCodexInstalledLocalToken)
	if err != nil {
		t.Fatalf("new route audit: %v", err)
	}
	outcome := &codexInstalledHTTPClientOutcome{}
	authority := newCodexInstalledHTTPAuditAuthority(codexInstalledHTTPAuditAuthorityConfig{
		routes:         routes,
		client:         outcome,
		protectedPaths: []codexInstalledProtectedPath{{path: protected}},
		privacyRoot:    privacyRoot,
		privacyNeedles: [][]byte{[]byte("raw-session-id")},
	})
	required := testCodexInstalledListenerRequirements()
	binding := testCodexInstalledListenerBinding(required.CQBuild)
	lease, err := authority.Begin(context.Background(), readinessTuple(required), binding)
	if err != nil {
		t.Fatalf("begin audit: %v", err)
	}
	defer lease.Release()

	if err := os.WriteFile(protected, []byte("after\n"), 0o600); err != nil {
		t.Fatalf("mutate protected file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(privacyRoot, "journal"), []byte("contains raw-session-id"), 0o600); err != nil {
		t.Fatalf("write leaking journal: %v", err)
	}
	unexpectedRequest := httptest.NewRequest(http.MethodGet, "/unexpected", nil)
	unexpectedRequest.Header.Set("Authorization", "Bearer "+testCodexInstalledLocalToken)
	routes.guard(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), unexpectedRequest)
	outcome.egressAttempts.Add(1)

	proof, err := lease.Complete(context.Background())
	if err != nil {
		t.Fatalf("complete audit: %v", err)
	}
	if proof.automaticAuthWrites != 1 || proof.rawIdentifierLeaks != 1 || proof.egressAttempts != 1 ||
		proof.unexpectedRoutes != 1 || proof.exactClientPong {
		t.Fatalf("audit proof = %#v, want detected failures", proof)
	}
}

func TestCodexInstalledHTTPAuditAuthorityRejectsPriorClientOutcome(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*codexInstalledHTTPClientOutcome)
	}{
		{name: "exact pong", prepare: func(outcome *codexInstalledHTTPClientOutcome) { outcome.exactPong.Store(true) }},
		{name: "egress", prepare: func(outcome *codexInstalledHTTPClientOutcome) { outcome.egressAttempts.Store(1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			protected := filepath.Join(root, "auth.json")
			if err := os.WriteFile(protected, []byte("protected\n"), 0o600); err != nil {
				t.Fatalf("write protected file: %v", err)
			}
			privacyRoot := filepath.Join(root, "runtime")
			if err := os.Mkdir(privacyRoot, 0o700); err != nil {
				t.Fatalf("mkdir privacy root: %v", err)
			}
			routes, err := newCodexInstalledHTTPRouteAudit("0.147.0-alpha.6.5", testCodexInstalledLocalToken)
			if err != nil {
				t.Fatalf("new route audit: %v", err)
			}
			outcome := &codexInstalledHTTPClientOutcome{}
			test.prepare(outcome)
			authority := newCodexInstalledHTTPAuditAuthority(codexInstalledHTTPAuditAuthorityConfig{
				routes:         routes,
				client:         outcome,
				protectedPaths: []codexInstalledProtectedPath{{path: protected}},
				privacyRoot:    privacyRoot,
				privacyNeedles: [][]byte{[]byte("raw-session-id")},
			})
			required := testCodexInstalledListenerRequirements()
			if lease, err := authority.Begin(
				context.Background(), readinessTuple(required), testCodexInstalledListenerBinding(required.CQBuild),
			); err == nil || lease != nil {
				t.Fatalf("Begin() = (%#v, %v), want fail-closed rejection", lease, err)
			}
		})
	}
}
