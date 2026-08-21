package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyInspectCollectsIndependentFactsAndNeverSynthesisesSuccess(t *testing.T) {
	called := map[string]int{}
	target := ProxyInspectionTarget{
		Inspector: func(context.Context) proxy.Fact[proxy.InspectorIdentity] {
			called["inspector"]++
			return proxy.KnownFact(proxy.InspectorIdentity{Executable: "/opt/cq"})
		},
		Desired: func(context.Context) proxy.Fact[proxy.DesiredProxyState] {
			called["desired"]++
			return proxy.KnownFact(proxy.DesiredProxyState{Manager: "launchagent"})
		},
		Service: func(context.Context) proxy.Fact[proxy.ServiceState] {
			called["service"]++
			return proxy.UnavailableFact[proxy.ServiceState]("service_unavailable")
		},
		Listener: func(context.Context) proxy.Fact[proxy.ListenerState] {
			called["listener"]++
			return proxy.AbsentFact[proxy.ListenerState]()
		},
		Process: func(context.Context) proxy.Fact[proxy.ProcessState] {
			called["process"]++
			return proxy.AbsentFact[proxy.ProcessState]()
		},
		Runtime: func(context.Context) proxy.Fact[proxy.RuntimeIdentity] {
			called["runtime"]++
			return proxy.KnownFact(proxy.RuntimeIdentity{Reachable: true, PID: 42, Executable: "/opt/cq", Health: "healthy"})
		},
		DataPlane: func(context.Context) proxy.Fact[proxy.DataPlaneProof] {
			called["data_plane"]++
			return proxy.AbsentFact[proxy.DataPlaneProof]()
		},
	}

	got := InspectProxy(context.Background(), target)
	for _, name := range []string{"inspector", "desired", "service", "listener", "process", "runtime", "data_plane"} {
		if called[name] != 1 {
			t.Fatalf("collector %s calls = %d, want 1", name, called[name])
		}
	}
	if got.Verdict != proxy.ProxyVerdictIndeterminate || got.ExitCode != 4 {
		t.Fatalf("inspection verdict = %s/%d, want indeterminate/4", got.Verdict, got.ExitCode)
	}
}

func TestProxyInspectHonoursCancelledContextWithoutCallingCollectors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	got := InspectProxy(ctx, ProxyInspectionTarget{Service: func(context.Context) proxy.Fact[proxy.ServiceState] {
		called = true
		return proxy.KnownFact(proxy.ServiceState{})
	}})
	if called {
		t.Fatal("collector called after cancellation")
	}
	if got.Verdict != proxy.ProxyVerdictIndeterminate {
		t.Fatalf("cancelled verdict = %s", got.Verdict)
	}
}

func TestProxyInspectRendersHumanJSONAndDoctorFacts(t *testing.T) {
	snapshot := proxy.ReconcileProxySnapshot(proxy.ProxySnapshot{
		Desired:   proxy.KnownFact(proxy.DesiredProxyState{Manager: "launchagent", Configured: true}),
		Service:   proxy.KnownFact(proxy.ServiceState{Manager: "launchagent", State: "stopped"}),
		Listener:  proxy.AbsentFact[proxy.ListenerState](),
		Process:   proxy.AbsentFact[proxy.ProcessState](),
		Runtime:   proxy.AbsentFact[proxy.RuntimeIdentity](),
		DataPlane: proxy.AbsentFact[proxy.DataPlaneProof](),
	})
	var human bytes.Buffer
	if err := RenderProxySnapshot(&human, snapshot, ProxyRenderHuman); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "overall: down") || !strings.Contains(human.String(), "service: launchagent stopped") {
		t.Fatalf("human output = %q", human.String())
	}
	var jsonOutput bytes.Buffer
	if err := RenderProxySnapshot(&jsonOutput, snapshot, ProxyRenderJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOutput.String(), `"kind":"proxy_snapshot"`) || !strings.Contains(jsonOutput.String(), `"state":"down"`) {
		t.Fatalf("JSON output = %q", jsonOutput.String())
	}
	if strings.Contains(jsonOutput.String(), `"pid"`) || strings.Contains(jsonOutput.String(), "/opt/cq") {
		t.Fatalf("JSON output leaked process identity: %q", jsonOutput.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(jsonOutput.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "kind", "ok", "state", "result", "warnings", "errors"} {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("JSON envelope missing %q: %s", key, jsonOutput.String())
		}
	}
	if envelope["warnings"] == nil || envelope["errors"] == nil {
		t.Fatalf("JSON arrays must not be null: %s", jsonOutput.String())
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("JSON result = %#v", envelope["result"])
	}
	for _, key := range []string{"instance", "verdict", "desired", "authority", "services", "listener", "runtime", "routing", "client_bearer_barrier", "collected_at", "duration_ms"} {
		if _, ok := result[key]; !ok {
			t.Fatalf("JSON result missing %q: %s", key, jsonOutput.String())
		}
	}
	checks := ProxyDoctorChecks(snapshot)
	if len(checks) == 0 || checks[0].ID != "service.unique" || checks[0].Status != "fail" || checks[0].Summary == "" || checks[0].EvidenceRefs == nil {
		t.Fatalf("doctor checks = %+v", checks)
	}
}

func TestProxyDoctorChecksAreDerivedFromIndependentFacts(t *testing.T) {
	snapshot := proxy.ReconcileProxySnapshot(proxy.ProxySnapshot{
		Desired:   proxy.KnownFact(proxy.DesiredProxyState{Manager: "launchagent", Configured: true, Listener: "127.0.0.1:1234"}),
		Service:   proxy.KnownFact(proxy.ServiceState{Manager: "launchagent", State: "running", PID: 42, Executable: "/opt/cq"}),
		Listener:  proxy.KnownFact(proxy.ListenerState{State: "foreign", Listener: "127.0.0.1:1234", PID: 99, Executable: "/tmp/other"}),
		Process:   proxy.KnownFact(proxy.ProcessState{PID: 99, Executable: "/tmp/other"}),
		Runtime:   proxy.KnownFact(proxy.RuntimeIdentity{Reachable: true, PID: 99, Executable: "/tmp/other", Health: "healthy"}),
		DataPlane: proxy.KnownFact(proxy.DataPlaneProof{Proven: true}),
	})
	checks := ProxyDoctorChecks(snapshot)
	if checks[0].Status != "pass" || checks[1].Status != "fail" || checks[2].Status != "pass" {
		t.Fatalf("independent checks = %+v", checks)
	}
}

func TestProxyInspectNormalisesUnsafeCollectorFacts(t *testing.T) {
	raw := "/Users/alice/private/cq"
	got := InspectProxy(context.Background(), ProxyInspectionTarget{
		Desired: func(context.Context) proxy.Fact[proxy.DesiredProxyState] {
			return proxy.Fact[proxy.DesiredProxyState]{Status: proxy.FactUnavailable, ErrorCode: &raw}
		},
	})
	if got.Desired.Status != proxy.FactInvalid || got.Desired.ErrorCode == nil || *got.Desired.ErrorCode != "invalid_fact" {
		t.Fatalf("unsafe fact = %+v", got.Desired)
	}
	var output bytes.Buffer
	if err := RenderProxySnapshot(&output, got, ProxyRenderJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), raw) || strings.Contains(output.String(), "/Users/") {
		t.Fatalf("projection leaked unsafe collector text: %s", output.String())
	}
}

func TestProxyInspectPropagatesWriterErrors(t *testing.T) {
	snapshot := proxy.ReconcileProxySnapshot(proxy.ProxySnapshot{})
	if err := RenderProxySnapshot(failingProxyInspectWriter{}, snapshot, ProxyRenderJSON); !errors.Is(err, errProxyInspectWrite) {
		t.Fatalf("render error = %v", err)
	}
}

func TestProjectProxySnapshotMarksHomebrewResilienceFactsAbsent(t *testing.T) {
	snapshot := proxy.ReconcileProxySnapshot(proxy.ProxySnapshot{
		Desired:   proxy.KnownFact(proxy.DesiredProxyState{Manager: "homebrew", Configured: true, Listener: "127.0.0.1:19280"}),
		Service:   proxy.KnownFact(proxy.ServiceState{Manager: "homebrew", State: "running", PID: 42, Executable: "/opt/homebrew/bin/cq"}),
		Listener:  proxy.KnownFact(proxy.ListenerState{State: "listening", Listener: "127.0.0.1:19280", PID: 42, Executable: "/opt/homebrew/bin/cq"}),
		Process:   proxy.KnownFact(proxy.ProcessState{PID: 42, Executable: "/opt/homebrew/bin/cq"}),
		Runtime:   proxy.KnownFact(proxy.RuntimeIdentity{Reachable: true, PID: 42, Executable: "/opt/homebrew/bin/cq", Health: "healthy"}),
		DataPlane: proxy.KnownFact(proxy.DataPlaneProof{Code: "unproven"}),
	})

	got := projectProxySnapshot(snapshot)
	if snapshot.Verdict != proxy.ProxyVerdictHealthy {
		t.Fatalf("verdict = %s, want healthy", snapshot.Verdict)
	}
	for name, fact := range map[string]proxy.Fact[struct{}]{
		"instance": got.Instance, "authority": got.Authority, "routing": got.Routing, "client bearer barrier": got.ClientBearerBarrier,
	} {
		if fact.Status != proxy.FactAbsent {
			t.Errorf("%s status = %s, want absent", name, fact.Status)
		}
	}
}

var errProxyInspectWrite = errors.New("write failed")

type failingProxyInspectWriter struct{}

func (failingProxyInspectWriter) Write([]byte) (int, error) { return 0, errProxyInspectWrite }
