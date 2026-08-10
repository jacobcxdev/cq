package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testCodexInstalledArtifacts = codexInstalledArtifactRequirement{
	cqExecutableSHA256:     sha256.Sum256([]byte("cq executable")),
	clientExecutableSHA256: sha256.Sum256([]byte("client executable")),
	serviceKind:            codexInstalledListenerServiceLaunchd,
	serviceIdentitySHA256:  sha256.Sum256([]byte("loaded service")),
}

func testCodexRequirements(transport CodexRoutingTransport) CodexTransportRequirements {
	required := CodexTransportRequirements{
		Transport:          transport,
		CQBuild:            "cq-test-build",
		ParserSchema:       3,
		LeaseSchema:        4,
		SemanticsRevision:  "http-test-semantics-v2",
		ClientBuild:        "codex-test-build",
		RetryBudget:        1,
		FixtureHash:        "fixture-sha256",
		RequiredGates:      []string{"corpus", "installed"},
		ObserveImplemented: true,
		EnforceImplemented: true,
	}
	if transport == CodexRoutingHTTP {
		required.installedArtifacts = testCodexInstalledArtifacts
	}
	return required
}

func testCodexMarker(requirements CodexTransportRequirements) CodexReadinessMarker {
	artifacts := requirements.installedArtifacts
	if requirements.Transport == CodexRoutingHTTP && !artifacts.valid() {
		artifacts = testCodexInstalledArtifacts
	}
	return CodexReadinessMarker{
		Version:                CodexReadinessMarkerVersion,
		Transport:              requirements.Transport,
		CQBuild:                requirements.CQBuild,
		ParserSchema:           requirements.ParserSchema,
		LeaseSchema:            requirements.LeaseSchema,
		SemanticsRevision:      requirements.SemanticsRevision,
		ClientBuild:            requirements.ClientBuild,
		RetryBudget:            requirements.RetryBudget,
		FixtureHash:            requirements.FixtureHash,
		CQExecutableSHA256:     artifacts.cqExecutableHex(),
		ClientExecutableSHA256: artifacts.clientExecutableHex(),
		ServiceKind:            string(artifacts.serviceKind),
		ServiceIdentitySHA256:  artifacts.serviceIdentityHex(),
		InstalledResult:        "passed",
		CompletedGates:         append([]string(nil), requirements.RequiredGates...),
		ValidatedAt:            time.Unix(20_000, 0).UTC(),
	}
}

func TestCodexReadinessMarkerRejectsChangedInstalledArtifactAtSameBuild(t *testing.T) {
	required := testCodexRequirements(CodexRoutingHTTP)
	required.CQBuild = "dev"
	required.ClientBuild = "0.146.0"
	valid := testCodexMarker(required)

	tests := []struct {
		name   string
		mutate func(*CodexReadinessMarker)
		want   string
	}{
		{name: "CQ executable", mutate: func(marker *CodexReadinessMarker) {
			marker.CQExecutableSHA256 = strings.Repeat("a", sha256.Size*2)
		}, want: "CQ executable"},
		{name: "client executable", mutate: func(marker *CodexReadinessMarker) {
			marker.ClientExecutableSHA256 = strings.Repeat("b", sha256.Size*2)
		}, want: "client executable"},
		{name: "loaded service", mutate: func(marker *CodexReadinessMarker) {
			marker.ServiceIdentitySHA256 = strings.Repeat("c", sha256.Size*2)
		}, want: "service identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			marker := valid
			test.mutate(&marker)
			if err := ValidateCodexReadinessMarker(marker, required); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q mismatch", err, test.want)
			}
		})
	}
}

func TestLoadCodexReadinessMarkerRejectsMalformedInstalledArtifactBinding(t *testing.T) {
	required := testCodexRequirements(CodexRoutingHTTP)
	for _, test := range []struct {
		name   string
		mutate func(*CodexReadinessMarker)
	}{
		{name: "zero digest", mutate: func(marker *CodexReadinessMarker) { marker.CQExecutableSHA256 = strings.Repeat("0", sha256.Size*2) }},
		{name: "uppercase digest", mutate: func(marker *CodexReadinessMarker) {
			marker.ClientExecutableSHA256 = strings.ToUpper(marker.ClientExecutableSHA256)
		}},
		{name: "invalid service kind", mutate: func(marker *CodexReadinessMarker) { marker.ServiceKind = "ephemeral" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			marker := testCodexMarker(required)
			test.mutate(&marker)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := json.MarshalIndent(marker, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, '\n')
			if err := os.WriteFile(codexReadinessPath(dir, CodexRoutingHTTP), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if loaded, err := LoadCodexReadinessMarker(dir, CodexRoutingHTTP); err == nil {
				t.Fatalf("malformed marker loaded: %#v", loaded)
			}
		})
	}
}

func TestLoadCodexReadinessMarkerRejectsUnsafeFilesystemAuthority(t *testing.T) {
	required := testCodexRequirements(CodexRoutingHTTP)
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string) string
	}{
		{name: "marker symlink", mutate: func(t *testing.T, dir string) string {
			markerPath := codexReadinessPath(dir, CodexRoutingHTTP)
			target := filepath.Join(t.TempDir(), "target.json")
			if err := os.Rename(markerPath, target); err != nil {
				t.Fatalf("move marker target: %v", err)
			}
			if err := os.Symlink(target, markerPath); err != nil {
				t.Fatalf("symlink marker: %v", err)
			}
			return dir
		}},
		{name: "directory symlink", mutate: func(t *testing.T, dir string) string {
			link := filepath.Join(t.TempDir(), "linked-state")
			if err := os.Symlink(dir, link); err != nil {
				t.Fatalf("symlink state directory: %v", err)
			}
			return link
		}},
		{name: "marker mode", mutate: func(t *testing.T, dir string) string {
			if err := os.Chmod(codexReadinessPath(dir, CodexRoutingHTTP), 0o644); err != nil {
				t.Fatalf("chmod marker: %v", err)
			}
			return dir
		}},
		{name: "marker hardlink", mutate: func(t *testing.T, dir string) string {
			markerPath := codexReadinessPath(dir, CodexRoutingHTTP)
			if err := os.Link(markerPath, filepath.Join(dir, "marker-copy.json")); err != nil {
				t.Fatalf("hardlink marker: %v", err)
			}
			return dir
		}},
		{name: "directory mode", mutate: func(t *testing.T, dir string) string {
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatalf("chmod directory: %v", err)
			}
			return dir
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := saveCodexHTTPReadinessMarkerDurably(dir, testCodexMarker(required)); err != nil {
				t.Fatalf("save marker: %v", err)
			}
			dir = test.mutate(t, dir)
			if marker, err := LoadCodexReadinessMarker(dir, CodexRoutingHTTP); err == nil {
				t.Fatalf("unsafe marker loaded: %#v", marker)
			}
		})
	}
}

func TestLoadCodexReadinessMarkerRejectsNonCanonicalJSON(t *testing.T) {
	required := testCodexRequirements(CodexRoutingHTTP)
	marker := testCodexMarker(required)
	canonical, err := json.MarshalIndent(&marker, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	canonical = append(canonical, '\n')
	minified, err := json.Marshal(&marker)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
	}{
		{name: "unknown field", data: []byte(strings.Replace(string(canonical), "{\n", "{\n  \"unknown\": true,\n", 1))},
		{name: "duplicate field", data: []byte(strings.Replace(string(canonical), "  \"version\":", "  \"version\": 3,\n  \"version\":", 1))},
		{name: "trailing whitespace", data: append(append([]byte(nil), canonical...), '\n')},
		{name: "minified", data: minified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := saveCodexHTTPReadinessMarkerDurably(dir, marker); err != nil {
				t.Fatalf("save marker: %v", err)
			}
			if err := os.WriteFile(codexReadinessPath(dir, CodexRoutingHTTP), test.data, 0o600); err != nil {
				t.Fatalf("replace marker bytes: %v", err)
			}
			if loaded, err := LoadCodexReadinessMarker(dir, CodexRoutingHTTP); err == nil {
				t.Fatalf("noncanonical marker loaded: %#v", loaded)
			}
		})
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
		{"semantics revision", func(m *CodexReadinessMarker) { m.SemanticsRevision = "old" }, "semantics revision"},
		{"client build", func(m *CodexReadinessMarker) { m.ClientBuild = "old" }, "client build"},
		{"retry budget", func(m *CodexReadinessMarker) { m.RetryBudget++ }, "retry budget"},
		{"fixture hash", func(m *CodexReadinessMarker) { m.FixtureHash = "old" }, "fixture hash"},
		{"installed result", func(m *CodexReadinessMarker) { m.InstalledResult = "failed" }, "installed result"},
		{"gate", func(m *CodexReadinessMarker) { m.CompletedGates = []string{"corpus"} }, "gate set mismatch"},
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

func TestCodexHTTPReadinessRevisionInvalidatesPriorMarker(t *testing.T) {
	required, _ := DefaultCodexRoutingRequirements("cq-build", "codex-cli 0.146.0")
	if required.LeaseSchema != 3 || required.SemanticsRevision != "http-conservative-routing-v3" {
		t.Fatalf("current readiness boundary = lease %d semantics %q, want 3/v3", required.LeaseSchema, required.SemanticsRevision)
	}
	prior := testCodexMarker(required)
	prior.LeaseSchema = 2
	prior.SemanticsRevision = "http-conservative-routing-v2"
	prior.CompletedGates = []string{
		"strong-metadata",
		"lease-pinning",
		"pre-admission-failover",
		"synchronous-journal",
		"continuity-affinity",
		"compressed-replay",
		"installed-listener",
	}
	if err := ValidateCodexReadinessMarker(prior, required); err == nil || !strings.Contains(err.Error(), "lease schema") {
		t.Fatalf("prior marker error = %v, want lease-schema invalidation", err)
	}
}

func TestCodexHTTPReadinessRequiresExactCanonicalGateTuple(t *testing.T) {
	required, _ := DefaultCodexRoutingRequirements("cq-build", "codex-cli 0.146.0")
	valid := testCodexMarker(required)
	if err := ValidateCodexReadinessMarker(valid, required); err != nil {
		t.Fatalf("valid marker rejected: %v", err)
	}

	tests := []struct {
		name  string
		gates []string
	}{
		{name: "missing", gates: valid.CompletedGates[:len(valid.CompletedGates)-1]},
		{name: "extra", gates: append(append([]string(nil), valid.CompletedGates...), "legacy-routing")},
		{name: "duplicate", gates: append(append([]string(nil), valid.CompletedGates...), valid.CompletedGates[0])},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			marker := valid
			marker.CompletedGates = append([]string(nil), test.gates...)
			if err := ValidateCodexReadinessMarker(marker, required); err == nil || !strings.Contains(err.Error(), "gate set mismatch") {
				t.Fatalf("gate error = %v, want exact-set rejection", err)
			}
		})
	}
}

func TestDefaultCodexHTTPReadinessTupleCoversConservativeRuntime(t *testing.T) {
	httpReq, wsReq := DefaultCodexRoutingRequirements("cq-build", "codex-cli 0.146.0")
	wantGates := []string{
		"frozen-single-transform-envelope",
		"warm-affinity",
		"deterministic-fallback",
		"terminal-default-once",
		"exact-pre-admission-hard429-replay",
		"admitted-no-migration",
		"v2-journal-runtime",
		"installed-listener",
	}
	if httpReq.SemanticsRevision != CodexHTTPReadinessSemanticsRevision {
		t.Fatalf("HTTP semantics revision = %q", httpReq.SemanticsRevision)
	}
	if !reflect.DeepEqual(httpReq.RequiredGates, wantGates) {
		t.Fatalf("HTTP gates = %#v, want %#v", httpReq.RequiredGates, wantGates)
	}
	httpReq.RequiredGates[0] = "mutated"
	freshHTTPReq, _ := DefaultCodexRoutingRequirements("cq-build", "codex-cli 0.146.0")
	if !reflect.DeepEqual(freshHTTPReq.RequiredGates, wantGates) {
		t.Fatalf("fresh HTTP gates = %#v after caller mutation, want %#v", freshHTTPReq.RequiredGates, wantGates)
	}
	if !httpReq.EnforceImplemented {
		t.Fatal("HTTP enforcement unexpectedly unavailable")
	}
	if wsReq.EnforceImplemented || wsReq.SemanticsRevision != "" || len(wsReq.RequiredGates) != 0 || wsReq.FixtureHash != "" {
		t.Fatalf("WebSocket readiness advanced early: %#v", wsReq)
	}
}

func TestCodexReadinessFingerprintChangesWithSemanticsOrGates(t *testing.T) {
	required, _ := DefaultCodexRoutingRequirements("cq-build", "codex-cli 0.146.0")
	baseline := requirementsFingerprint(required)

	changedRevision := required
	changedRevision.SemanticsRevision = "changed"
	if got := requirementsFingerprint(changedRevision); got == baseline {
		t.Fatal("semantics revision did not change readiness fingerprint")
	}

	changedGates := required
	changedGates.RequiredGates = append(append([]string(nil), required.RequiredGates...), "changed")
	if got := requirementsFingerprint(changedGates); got == baseline {
		t.Fatal("gate tuple did not change readiness fingerprint")
	}
}

func TestCodexEnforceInhibitedWithoutMarker(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{CodexTurnRouting: CodexRoutingEnforce, CodexWSTurnRouting: CodexRoutingOff}
	httpReq, wsReq := DefaultCodexRoutingRequirements("build", "client")
	runtime, err := openCodexRoutingRuntimeAt(dir, cfg, httpReq, wsReq)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.HTTP.Configured != CodexRoutingEnforce || runtime.HTTP.Effective != CodexRoutingObserve || runtime.HTTP.InhibitionReason != "installed artifact identity unavailable" {
		t.Fatalf("HTTP status = %+v", runtime.HTTP)
	}
}

func TestCodexRoutingRuntimeCapturesInstalledArtifactsOnlyForEnforce(t *testing.T) {
	for _, mode := range []CodexRoutingMode{CodexRoutingOff, CodexRoutingObserve} {
		t.Run(string(mode), func(t *testing.T) {
			calls := 0
			capture := func(context.Context, string) (codexInstalledArtifactRequirement, error) {
				calls++
				return testCodexInstalledArtifacts, nil
			}
			httpReq, wsReq := DefaultCodexRoutingRequirements("dev", "0.146.0")
			if _, err := openCodexRoutingRuntimeAtWithArtifactCapture(t.TempDir(), &Config{CodexTurnRouting: mode}, httpReq, wsReq, capture); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("artifact capture calls = %d, want 0", calls)
			}
		})
	}

	t.Run("enforce capture failure", func(t *testing.T) {
		calls := 0
		capture := func(context.Context, string) (codexInstalledArtifactRequirement, error) {
			calls++
			return codexInstalledArtifactRequirement{}, os.ErrNotExist
		}
		httpReq, wsReq := DefaultCodexRoutingRequirements("dev", "0.146.0")
		runtime, err := openCodexRoutingRuntimeAtWithArtifactCapture(t.TempDir(), &Config{CodexTurnRouting: CodexRoutingEnforce}, httpReq, wsReq, capture)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 || runtime.HTTP.Effective != CodexRoutingObserve || runtime.HTTP.InhibitionReason != "installed artifact identity unavailable" {
			t.Fatalf("calls/runtime = %d/%+v", calls, runtime.HTTP)
		}
	})

	t.Run("enforce capture success", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		httpReq, wsReq := DefaultCodexRoutingRequirements("dev", "0.146.0")
		bound := httpReq
		bound.installedArtifacts = testCodexInstalledArtifacts
		if err := saveCodexHTTPReadinessMarkerDurably(dir, testCodexMarker(bound)); err != nil {
			t.Fatal(err)
		}
		calls := 0
		capture := func(context.Context, string) (codexInstalledArtifactRequirement, error) {
			calls++
			return testCodexInstalledArtifacts, nil
		}
		runtime, err := openCodexRoutingRuntimeAtWithArtifactCapture(dir, &Config{CodexTurnRouting: CodexRoutingEnforce}, httpReq, wsReq, capture)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 || runtime.HTTP.Effective != CodexRoutingEnforce {
			t.Fatalf("calls/runtime = %d/%+v", calls, runtime.HTTP)
		}
	})
}

func TestCodexRoutingEpochsNeverPromoteShadowState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	httpReq := testCodexRequirements(CodexRoutingHTTP)
	wsReq := testCodexRequirements(CodexRoutingWebSocket)
	if err := saveCodexHTTPReadinessMarkerDurably(dir, testCodexMarker(httpReq)); err != nil {
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
