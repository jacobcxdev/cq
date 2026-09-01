package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexInstalledHTTPValidationRuntimeRejectsNilServeBeforeReadiness(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	token, err := NewCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	err = RunCodexInstalledHTTPValidationRuntime(
		context.Background(),
		&Config{Port: 19281},
		"cq-build-42",
		"0.147.0-alpha.6.5",
		&testCodexInstalledHTTPValidationGuard{},
		token,
		nil,
	)
	if !errors.Is(err, errCodexInstalledListenerAcceptance) {
		t.Fatalf("nil runtime serve error = %v", err)
	}
	if !strings.Contains(err.Error(), "runtime serve") {
		t.Fatalf("nil runtime serve error = %v, want runtime serve stage", err)
	}
	paths, resolveErr := ResolveDefaultPaths()
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if _, statErr := os.Lstat(codexReadinessPath(paths.StateDir, CodexRoutingHTTP)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("readiness marker exists after nil runtime serve: %v", statErr)
	}
}

func TestCodexInstalledHTTPValidationKeepsMarkerAbsentOnFailureAndPanic(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{name: "failure", run: func() error { return errors.New("validation failed") }},
		{name: "panic", run: func() error { panic("validation panic") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			markerDir := filepath.Join(t.TempDir(), "state")
			marker := testCodexMarker(testCodexRequirements(CodexRoutingHTTP))
			if err := saveCodexHTTPReadinessMarkerDurably(markerDir, marker); err != nil {
				t.Fatalf("save prior marker: %v", err)
			}
			runs := 0
			err := runCodexInstalledHTTPValidationWithDependencies(
				context.Background(), &Config{Port: 19280}, "cq-build-42", "0.147.0-alpha.6.5",
				codexInstalledHTTPValidationDependencies{
					markerDir:  markerDir,
					invalidate: invalidateCodexHTTPReadinessMarkerDurably,
					guard:      &testCodexInstalledHTTPValidationGuard{},
					run: func(context.Context, *Config, string, string, string, CodexInstalledHTTPValidationGuard) error {
						runs++
						if _, statErr := os.Lstat(codexReadinessPath(markerDir, CodexRoutingHTTP)); !errors.Is(statErr, os.ErrNotExist) {
							t.Fatalf("prior marker survived before validation: %v", statErr)
						}
						if saveErr := saveCodexHTTPReadinessMarkerDurably(markerDir, marker); saveErr != nil {
							t.Fatalf("save simulated committed marker: %v", saveErr)
						}
						return test.run()
					},
				},
			)
			if err == nil {
				t.Fatal("failed validation returned nil error")
			}
			if runs != 1 {
				t.Fatalf("validation runs = %d, want 1", runs)
			}
			if _, statErr := os.Lstat(codexReadinessPath(markerDir, CodexRoutingHTTP)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("marker survived failed validation: %v", statErr)
			}
		})
	}
}

func TestCodexInstalledHTTPValidationInvalidatesBeforeArgumentFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		ctx         func() context.Context
		config      *Config
		cqBuild     string
		clientBuild string
	}{
		{name: "nil context", ctx: func() context.Context { return nil }, config: &Config{}, cqBuild: "cq-build-42", clientBuild: "0.147.0-alpha.6.5"},
		{name: "cancelled context", ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }, config: &Config{}, cqBuild: "cq-build-42", clientBuild: "0.147.0-alpha.6.5"},
		{name: "nil config", ctx: context.Background, cqBuild: "cq-build-42", clientBuild: "0.147.0-alpha.6.5"},
		{name: "empty build", ctx: context.Background, config: &Config{}, clientBuild: "0.147.0-alpha.6.5"},
		{name: "invalid client build", ctx: context.Background, config: &Config{}, cqBuild: "cq-build-42", clientBuild: "invalid build"},
	} {
		t.Run(test.name, func(t *testing.T) {
			markerDir := filepath.Join(t.TempDir(), "state")
			if err := saveCodexHTTPReadinessMarkerDurably(markerDir, testCodexMarker(testCodexRequirements(CodexRoutingHTTP))); err != nil {
				t.Fatalf("save prior marker: %v", err)
			}
			runs := 0
			err := runCodexInstalledHTTPValidationWithDependencies(test.ctx(), test.config, test.cqBuild, test.clientBuild,
				codexInstalledHTTPValidationDependencies{
					markerDir:  markerDir,
					invalidate: invalidateCodexHTTPReadinessMarkerDurably,
					guard:      &testCodexInstalledHTTPValidationGuard{},
					run: func(context.Context, *Config, string, string, string, CodexInstalledHTTPValidationGuard) error {
						runs++
						return nil
					},
				},
			)
			if err == nil {
				t.Fatal("invalid startup returned nil error")
			}
			if runs != 0 {
				t.Fatalf("validation runs = %d, want 0", runs)
			}
			if _, statErr := os.Lstat(codexReadinessPath(markerDir, CodexRoutingHTTP)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("prior marker survived invalid startup: %v", statErr)
			}
		})
	}
}
