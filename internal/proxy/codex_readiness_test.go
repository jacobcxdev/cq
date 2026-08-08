package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCodexRequirements(transport CodexRoutingTransport) CodexTransportRequirements {
	return CodexTransportRequirements{
		Transport:          transport,
		CQBuild:            "cq-test-build",
		ParserSchema:       3,
		LeaseSchema:        4,
		ClientBuild:        "codex-test-build",
		RetryBudget:        1,
		FixtureHash:        "fixture-sha256",
		RequiredGates:      []string{"corpus", "installed"},
		ObserveImplemented: true,
		EnforceImplemented: true,
	}
}

func testCodexMarker(requirements CodexTransportRequirements) CodexReadinessMarker {
	return CodexReadinessMarker{
		Version:         CodexReadinessMarkerVersion,
		Transport:       requirements.Transport,
		CQBuild:         requirements.CQBuild,
		ParserSchema:    requirements.ParserSchema,
		LeaseSchema:     requirements.LeaseSchema,
		ClientBuild:     requirements.ClientBuild,
		RetryBudget:     requirements.RetryBudget,
		FixtureHash:     requirements.FixtureHash,
		InstalledResult: "passed",
		CompletedGates:  append([]string(nil), requirements.RequiredGates...),
		ValidatedAt:     time.Unix(20_000, 0).UTC(),
	}
}

func TestCodexReadinessMarkerRejectsEveryStaleDimension(t *testing.T) {
	required := testCodexRequirements(CodexRoutingHTTP)
	valid := testCodexMarker(required)
	if err := ValidateCodexReadinessMarker(valid, required); err != nil {
		t.Fatalf("valid marker rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*CodexReadinessMarker)
		want   string
	}{
		{"version", func(m *CodexReadinessMarker) { m.Version++ }, "version"},
		{"transport", func(m *CodexReadinessMarker) { m.Transport = CodexRoutingWebSocket }, "transport"},
		{"CQ build", func(m *CodexReadinessMarker) { m.CQBuild = "old" }, "CQ build"},
		{"parser schema", func(m *CodexReadinessMarker) { m.ParserSchema-- }, "parser schema"},
		{"lease schema", func(m *CodexReadinessMarker) { m.LeaseSchema-- }, "lease schema"},
		{"client build", func(m *CodexReadinessMarker) { m.ClientBuild = "old" }, "client build"},
		{"retry budget", func(m *CodexReadinessMarker) { m.RetryBudget++ }, "retry budget"},
		{"fixture hash", func(m *CodexReadinessMarker) { m.FixtureHash = "old" }, "fixture hash"},
		{"installed result", func(m *CodexReadinessMarker) { m.InstalledResult = "failed" }, "installed result"},
		{"gate", func(m *CodexReadinessMarker) { m.CompletedGates = []string{"corpus"} }, "installed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			marker := valid
			marker.CompletedGates = append([]string(nil), valid.CompletedGates...)
			test.mutate(&marker)
			err := ValidateCodexReadinessMarker(marker, required)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCodexEnforceInhibitedWithoutImplementationOrMarker(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{CodexTurnRouting: CodexRoutingEnforce, CodexWSTurnRouting: CodexRoutingOff}
	httpReq, wsReq := DefaultCodexRoutingRequirements("build", "client")
	runtime, err := openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.HTTP.Configured != CodexRoutingEnforce || runtime.HTTP.Effective != CodexRoutingObserve || runtime.HTTP.InhibitionReason != "enforcement implementation unavailable" {
		t.Fatalf("HTTP status = %+v", runtime.HTTP)
	}

	httpReq = testCodexRequirements(CodexRoutingHTTP)
	runtime, err = openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.HTTP.Effective != CodexRoutingObserve || runtime.HTTP.InhibitionReason != "readiness marker missing" {
		t.Fatalf("HTTP status with implementation = %+v", runtime.HTTP)
	}
}

func TestCodexRoutingEpochsNeverPromoteShadowState(t *testing.T) {
	dir := t.TempDir()
	httpReq := testCodexRequirements(CodexRoutingHTTP)
	wsReq := testCodexRequirements(CodexRoutingWebSocket)
	if err := SaveCodexReadinessMarker(dir, testCodexMarker(httpReq)); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{CodexTurnRouting: CodexRoutingObserve, CodexWSTurnRouting: CodexRoutingOff}
	observed, err := openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	if observed.HTTP.ShadowEpoch == 0 || observed.HTTP.AuthoritativeEpoch != 0 {
		t.Fatalf("observe status = %+v", observed.HTTP)
	}

	cfg.CodexTurnRouting = CodexRoutingEnforce
	enforced, err := openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	if enforced.HTTP.AuthoritativeEpoch == 0 || enforced.HTTP.AuthoritativeEpoch == observed.HTTP.ShadowEpoch || enforced.HTTP.ShadowEpoch != 0 {
		t.Fatalf("enforce status = %+v after %+v", enforced.HTTP, observed.HTTP)
	}
	firstAuthoritative := enforced.HTTP.AuthoritativeEpoch

	cfg.CodexTurnRouting = CodexRoutingOff
	off, err := openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	if off.HTTP.AuthoritativeEpoch != 0 || !containsEpoch(off.HTTP.RetainedAuthoritativeEpochs, firstAuthoritative) {
		t.Fatalf("off status = %+v, want retained %d", off.HTTP, firstAuthoritative)
	}

	cfg.CodexTurnRouting = CodexRoutingEnforce
	reenforced, err := openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	if reenforced.HTTP.AuthoritativeEpoch <= firstAuthoritative || !containsEpoch(reenforced.HTTP.RetainedAuthoritativeEpochs, firstAuthoritative) {
		t.Fatalf("re-enforce status = %+v, want fresh epoch retaining %d", reenforced.HTTP, firstAuthoritative)
	}
	if reenforced.HTTP.AuthoritativeEpoch == observed.HTTP.ShadowEpoch {
		t.Fatal("shadow epoch promoted to authoritative")
	}

	restarted, err := openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.HTTP.ModeEpoch != reenforced.HTTP.ModeEpoch {
		t.Fatalf("unchanged restart epoch = %d, want %d", restarted.HTTP.ModeEpoch, reenforced.HTTP.ModeEpoch)
	}
}

func TestCodexRoutingRuntimeDoesNotCreateReadinessMarker(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{CodexTurnRouting: CodexRoutingEnforce, CodexWSTurnRouting: CodexRoutingEnforce}
	httpReq, wsReq := DefaultCodexRoutingRequirements("new-build", "new-client")
	runtime, err := openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.HTTP.Effective != CodexRoutingObserve || runtime.WebSocket.Effective != CodexRoutingObserve {
		t.Fatalf("runtime = %+v, want observe fallback", runtime)
	}
	for _, transport := range []CodexRoutingTransport{CodexRoutingHTTP, CodexRoutingWebSocket} {
		if _, err := os.Stat(codexReadinessPath(dir, transport)); !os.IsNotExist(err) {
			t.Fatalf("upgrade created %s marker: %v", transport, err)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "codex-routing-mode.json")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestCodexRoutingRuntimeIsRestartScoped(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{CodexTurnRouting: CodexRoutingOff, CodexWSTurnRouting: CodexRoutingOff}
	httpReq, wsReq := DefaultCodexRoutingRequirements("build", "client")
	runtime, err := openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	cfg.CodexTurnRouting = CodexRoutingObserve
	if runtime.HTTP.Configured != CodexRoutingOff || runtime.HTTP.Effective != CodexRoutingOff {
		t.Fatalf("runtime mutated without restart: %+v", runtime.HTTP)
	}
	if !runtime.ConfiguredModesDiffer(cfg) {
		t.Fatal("restart-required mode difference not detected")
	}
}

func containsEpoch(epochs []uint64, want uint64) bool {
	for _, epoch := range epochs {
		if epoch == want {
			return true
		}
	}
	return false
}
