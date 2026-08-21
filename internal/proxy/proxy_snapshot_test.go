package proxy

import (
	"encoding/json"
	"testing"
)

func TestProxySnapshotReconcilesSupportedTopologies(t *testing.T) {
	tests := []struct {
		name string
		in   ProxySnapshot
		want ProxyVerdict
		exit int
	}{
		{
			name: "cq healthy",
			in: snapshotFixture(
				ServiceState{Manager: "launchagent", State: "running", PID: 42, Executable: "/opt/cq"},
				ListenerState{State: "listening", PID: 42, Executable: "/opt/cq"},
				RuntimeIdentity{Reachable: true, PID: 42, Executable: "/opt/cq", Health: "healthy"},
			),
			want: ProxyVerdictHealthy,
			exit: 0,
		},
		{
			name: "homebrew healthy",
			in: func() ProxySnapshot {
				snapshot := snapshotFixture(
					ServiceState{Manager: "homebrew", State: "running", PID: 43, Executable: "/opt/homebrew/bin/cq"},
					ListenerState{State: "listening", PID: 43, Executable: "/opt/homebrew/bin/cq"},
					RuntimeIdentity{Reachable: true, PID: 43, Executable: "/opt/homebrew/bin/cq", Health: "healthy"},
				)
				snapshot.DataPlane = KnownFact(DataPlaneProof{Code: "unproven"})
				return snapshot
			}(),
			want: ProxyVerdictHealthy,
			exit: 0,
		},
		{
			name: "manual legacy",
			in: snapshotFixture(
				ServiceState{Manager: "manual", State: "running", PID: 44, Executable: "/tmp/cq"},
				ListenerState{State: "listening", PID: 44, Executable: "/tmp/cq"},
				RuntimeIdentity{Reachable: true, PID: 44, Executable: "/tmp/cq", Health: "healthy"},
			),
			want: ProxyVerdictLegacy,
			exit: 1,
		},
		{
			name: "stopped",
			in: ProxySnapshot{
				Desired:   KnownFact(DesiredProxyState{Manager: "launchagent", Configured: true}),
				Service:   KnownFact(ServiceState{Manager: "launchagent", State: "stopped"}),
				Listener:  AbsentFact[ListenerState](),
				Process:   AbsentFact[ProcessState](),
				Runtime:   AbsentFact[RuntimeIdentity](),
				DataPlane: AbsentFact[DataPlaneProof](),
			},
			want: ProxyVerdictDown,
			exit: 2,
		},
		{
			name: "crash looping",
			in: ProxySnapshot{
				Desired:   KnownFact(DesiredProxyState{Manager: "launchagent", Configured: true}),
				Service:   KnownFact(ServiceState{Manager: "launchagent", State: "crash_looping"}),
				Listener:  AbsentFact[ListenerState](),
				Process:   AbsentFact[ProcessState](),
				Runtime:   AbsentFact[RuntimeIdentity](),
				DataPlane: AbsentFact[DataPlaneProof](),
			},
			want: ProxyVerdictDegraded,
			exit: 1,
		},
		{
			name: "foreign listener",
			in: ProxySnapshot{
				Desired:   KnownFact(DesiredProxyState{Manager: "launchagent", Configured: true}),
				Service:   KnownFact(ServiceState{Manager: "launchagent", State: "stopped"}),
				Listener:  KnownFact(ListenerState{State: "foreign", PID: 99, Executable: "/tmp/other"}),
				Process:   KnownFact(ProcessState{PID: 99, Executable: "/tmp/other"}),
				Runtime:   AbsentFact[RuntimeIdentity](),
				DataPlane: AbsentFact[DataPlaneProof](),
			},
			want: ProxyVerdictConflicted,
			exit: 3,
		},
		{
			name: "required collector unavailable",
			in: ProxySnapshot{
				Desired:   KnownFact(DesiredProxyState{}),
				Service:   UnavailableFact[ServiceState]("service_inspection_failed"),
				Listener:  AbsentFact[ListenerState](),
				Process:   AbsentFact[ProcessState](),
				Runtime:   AbsentFact[RuntimeIdentity](),
				DataPlane: AbsentFact[DataPlaneProof](),
			},
			want: ProxyVerdictIndeterminate,
			exit: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ReconcileProxySnapshot(test.in)
			if got.Verdict != test.want || got.ExitCode != test.exit {
				t.Fatalf("verdict/exit = %s/%d, want %s/%d", got.Verdict, got.ExitCode, test.want, test.exit)
			}
		})
	}
}

func TestProxySnapshotRejectsIdentityMismatchAndDoesNotTrustHealthAlone(t *testing.T) {
	mismatch := snapshotFixture(
		ServiceState{Manager: "launchagent", State: "running", PID: 42, Executable: "/opt/cq"},
		ListenerState{State: "listening", PID: 99, Executable: "/opt/cq"},
		RuntimeIdentity{Reachable: true, PID: 42, Executable: "/opt/cq", Health: "healthy"},
	)
	if got := ReconcileProxySnapshot(mismatch); got.Verdict != ProxyVerdictConflicted {
		t.Fatalf("mismatched identity verdict = %s, want conflicted", got.Verdict)
	}

	healthOnly := snapshotFixture(
		ServiceState{Manager: "launchagent", State: "running", PID: 42, Executable: "/opt/cq"},
		ListenerState{State: "listening", PID: 42, Executable: "/opt/cq"},
		RuntimeIdentity{Reachable: true, PID: 42, Executable: "/opt/cq", Health: "healthy"},
	)
	healthOnly.DataPlane = AbsentFact[DataPlaneProof]()
	if got := ReconcileProxySnapshot(healthOnly); got.Verdict != ProxyVerdictDegraded {
		t.Fatalf("health-only verdict = %s, want degraded", got.Verdict)
	}
}

func TestProxySnapshotRejectsMalformedFactsAndUnknownVocabulary(t *testing.T) {
	value := ServiceState{Manager: "launchagent", State: "running"}
	code := "service_unavailable"
	tests := []ProxySnapshot{
		{Service: Fact[ServiceState]{Status: FactKnown}},
		{Service: Fact[ServiceState]{Status: FactAbsent, Value: &value}},
		{Service: Fact[ServiceState]{Status: FactUnavailable}},
		{Service: Fact[ServiceState]{Status: FactUnavailable, ErrorCode: &code}, Desired: Fact[DesiredProxyState]{Status: "mystery"}},
		{Service: KnownFact(ServiceState{Manager: "mystery", State: "running"})},
		{Service: KnownFact(ServiceState{Manager: "launchagent", State: "mystery"})},
	}
	for index, input := range tests {
		got := ReconcileProxySnapshot(input)
		if got.Verdict != ProxyVerdictConflicted || got.ExitCode != 3 {
			t.Fatalf("case %d verdict = %s/%d, want conflicted/3", index, got.Verdict, got.ExitCode)
		}
	}
}

func TestProxySnapshotHealthyRequiresCoherentRunningTopology(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProxySnapshot)
	}{
		{"service stopped", func(snapshot *ProxySnapshot) {
			snapshot.Service = KnownFact(ServiceState{Manager: "launchagent", State: "stopped", PID: 42, Executable: "/opt/cq"})
		}},
		{"listener not listening", func(snapshot *ProxySnapshot) {
			snapshot.Listener = KnownFact(ListenerState{State: "inactive", PID: 42, Executable: "/opt/cq"})
		}},
		{"process identity missing", func(snapshot *ProxySnapshot) { snapshot.Process = KnownFact(ProcessState{}) }},
		{"data plane not proven", func(snapshot *ProxySnapshot) {
			snapshot.DataPlane = KnownFact(DataPlaneProof{Proven: false, Code: "not_proven"})
		}},
		{"data plane contradictory", func(snapshot *ProxySnapshot) {
			snapshot.DataPlane = KnownFact(DataPlaneProof{Proven: true, Code: "not_proven"})
		}},
		{"desired manager mismatch", func(snapshot *ProxySnapshot) {
			snapshot.Desired = KnownFact(DesiredProxyState{Manager: "manual", Configured: true, Listener: "127.0.0.1:1234"})
		}},
		{"desired listener mismatch", func(snapshot *ProxySnapshot) {
			snapshot.Desired = KnownFact(DesiredProxyState{Manager: "launchagent", Configured: true, Listener: "127.0.0.1:5678"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := snapshotFixture(
				ServiceState{Manager: "launchagent", State: "running", PID: 42, Executable: "/opt/cq"},
				ListenerState{State: "listening", PID: 42, Executable: "/opt/cq"},
				RuntimeIdentity{Reachable: true, PID: 42, Executable: "/opt/cq", Health: "healthy"},
			)
			test.mutate(&snapshot)
			if got := ReconcileProxySnapshot(snapshot); got.Verdict == ProxyVerdictHealthy {
				t.Fatalf("malformed topology reported healthy: %+v", got)
			}
		})
	}
}

func TestProxySnapshotInspectorSkewIsDescriptive(t *testing.T) {
	snapshot := snapshotFixture(
		ServiceState{Manager: "launchagent", State: "running", PID: 42, Executable: "/opt/cq"},
		ListenerState{State: "listening", PID: 42, Executable: "/opt/cq"},
		RuntimeIdentity{Reachable: true, PID: 42, Executable: "/opt/cq", Health: "healthy"},
	)
	snapshot.Inspector = KnownFact(InspectorIdentity{Executable: "/tmp/cq-new", Version: "new"})
	got := ReconcileProxySnapshot(snapshot)
	if got.Verdict != ProxyVerdictHealthy || len(got.Warnings) != 1 || got.Warnings[0] != "inspector_skew" {
		t.Fatalf("inspector skew = %+v, want healthy with warning", got)
	}
}

func TestProxySnapshotFactJSONPreservesUnknownState(t *testing.T) {
	snapshot := ProxySnapshot{
		Desired:   UnavailableFact[DesiredProxyState]("config_unavailable"),
		Service:   InvalidFact[ServiceState]("service_invalid"),
		Listener:  AbsentFact[ListenerState](),
		Process:   AbsentFact[ProcessState](),
		Runtime:   AbsentFact[RuntimeIdentity](),
		DataPlane: AbsentFact[DataPlaneProof](),
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := `"desired":{"status":"unavailable","value":null,"error_code":"config_unavailable"}`
	if !containsJSONFragment(data, want) {
		t.Fatalf("snapshot JSON = %s, want %s", data, want)
	}
}

func snapshotFixture(service ServiceState, listener ListenerState, runtime RuntimeIdentity) ProxySnapshot {
	listener.Listener = "127.0.0.1:1234"
	return ProxySnapshot{
		Inspector: KnownFact(InspectorIdentity{Executable: service.Executable, Version: "dev"}),
		Desired:   KnownFact(DesiredProxyState{Manager: service.Manager, Configured: true, Listener: listener.Listener}),
		Service:   KnownFact(service),
		Listener:  KnownFact(listener),
		Process:   KnownFact(ProcessState{PID: listener.PID, Executable: listener.Executable}),
		Runtime:   KnownFact(runtime),
		DataPlane: KnownFact(DataPlaneProof{Proven: true}),
	}
}

func containsJSONFragment(data []byte, fragment string) bool {
	for index := 0; index+len(fragment) <= len(data); index++ {
		if string(data[index:index+len(fragment)]) == fragment {
			return true
		}
	}
	return false
}
