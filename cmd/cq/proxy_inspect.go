package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

type ProxyInspectionTarget struct {
	Inspector func(context.Context) proxy.Fact[proxy.InspectorIdentity]
	Desired   func(context.Context) proxy.Fact[proxy.DesiredProxyState]
	Service   func(context.Context) proxy.Fact[proxy.ServiceState]
	Listener  func(context.Context) proxy.Fact[proxy.ListenerState]
	Process   func(context.Context) proxy.Fact[proxy.ProcessState]
	Runtime   func(context.Context) proxy.Fact[proxy.RuntimeIdentity]
	DataPlane func(context.Context) proxy.Fact[proxy.DataPlaneProof]
}

// defaultProxyInspectionTarget is intentionally effect-free in CU-1. Platform
// collectors are added in the later execution unit through this seam.
var defaultProxyInspectionTarget = func() ProxyInspectionTarget { return ProxyInspectionTarget{} }

func InspectProxy(ctx context.Context, target ProxyInspectionTarget) proxy.ProxySnapshot {
	startedAt := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return proxy.ReconcileProxySnapshot(cancelledProxySnapshot())
	}
	snapshot := proxy.ProxySnapshot{
		Inspector: collectProxyFact(ctx, target.Inspector, proxy.UnavailableFact[proxy.InspectorIdentity]("inspector_unavailable")),
		Desired:   collectProxyFact(ctx, target.Desired, proxy.UnavailableFact[proxy.DesiredProxyState]("config_unavailable")),
		Service:   collectProxyFact(ctx, target.Service, proxy.UnavailableFact[proxy.ServiceState]("service_unavailable")),
		Listener:  collectProxyFact(ctx, target.Listener, proxy.UnavailableFact[proxy.ListenerState]("listener_unavailable")),
		Process:   collectProxyFact(ctx, target.Process, proxy.UnavailableFact[proxy.ProcessState]("process_unavailable")),
		Runtime:   collectProxyFact(ctx, target.Runtime, proxy.UnavailableFact[proxy.RuntimeIdentity]("runtime_unavailable")),
		DataPlane: collectProxyFact(ctx, target.DataPlane, proxy.UnavailableFact[proxy.DataPlaneProof]("data_plane_unavailable")),
	}
	snapshot = proxy.ReconcileProxySnapshot(snapshot)
	snapshot.CollectedAt = time.Now().UTC().Format(time.RFC3339Nano)
	snapshot.DurationMS = time.Since(startedAt).Milliseconds()
	return snapshot
}

func collectProxyFact[T any](ctx context.Context, collector func(context.Context) proxy.Fact[T], unavailable proxy.Fact[T]) proxy.Fact[T] {
	if collector == nil || ctx.Err() != nil {
		return unavailable
	}
	return collector(ctx)
}

func cancelledProxySnapshot() proxy.ProxySnapshot {
	return proxy.ProxySnapshot{
		Inspector: proxy.UnavailableFact[proxy.InspectorIdentity]("inspection_cancelled"),
		Desired:   proxy.UnavailableFact[proxy.DesiredProxyState]("inspection_cancelled"),
		Service:   proxy.UnavailableFact[proxy.ServiceState]("inspection_cancelled"),
		Listener:  proxy.UnavailableFact[proxy.ListenerState]("inspection_cancelled"),
		Process:   proxy.UnavailableFact[proxy.ProcessState]("inspection_cancelled"),
		Runtime:   proxy.UnavailableFact[proxy.RuntimeIdentity]("inspection_cancelled"),
		DataPlane: proxy.UnavailableFact[proxy.DataPlaneProof]("inspection_cancelled"),
	}
}

type ProxyRenderMode string

const (
	ProxyRenderHuman ProxyRenderMode = "human"
	ProxyRenderJSON  ProxyRenderMode = "json"
)

func RenderProxySnapshot(writer io.Writer, snapshot proxy.ProxySnapshot, mode ProxyRenderMode) error {
	if mode == ProxyRenderJSON {
		if snapshot.CollectedAt == "" {
			snapshot.CollectedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		return json.NewEncoder(writer).Encode(struct {
			SchemaVersion int                       `json:"schema_version"`
			Kind          string                    `json:"kind"`
			OK            bool                      `json:"ok"`
			State         proxy.ProxyVerdict        `json:"state"`
			Result        proxySnapshotProjectionV1 `json:"result"`
			Warnings      []ProxyWarningV1          `json:"warnings"`
			Errors        []ProxyErrorV1            `json:"errors"`
		}{1, "proxy_snapshot", snapshot.ExitCode == 0, snapshot.Verdict, projectProxySnapshot(snapshot), projectProxyWarnings(snapshot), projectProxyErrors(snapshot)})
	}
	if _, err := fmt.Fprintf(writer, "overall: %s\n", snapshot.Verdict); err != nil {
		return err
	}
	if snapshot.Service.Value != nil {
		if _, err := fmt.Fprintf(writer, "service: %s %s\n", snapshot.Service.Value.Manager, snapshot.Service.Value.State); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(writer, "service: %s\n", snapshot.Service.Status); err != nil {
			return err
		}
	}
	if snapshot.Listener.Value != nil {
		_, err := fmt.Fprintf(writer, "listener: %s\n", snapshot.Listener.Value.State)
		return err
	}
	_, err := fmt.Fprintf(writer, "listener: %s\n", snapshot.Listener.Status)
	return err
}

type proxySnapshotProjectionV1 struct {
	Instance            proxy.Fact[struct{}]                  `json:"instance"`
	Verdict             proxy.ProxyVerdict                    `json:"verdict"`
	Desired             proxy.Fact[proxyDesiredProjectionV1]  `json:"desired"`
	Authority           proxy.Fact[struct{}]                  `json:"authority"`
	Services            []proxyServiceProjectionV1            `json:"services"`
	Listener            proxy.Fact[proxyListenerProjectionV1] `json:"listener"`
	Runtime             proxy.Fact[proxyRuntimeProjectionV1]  `json:"runtime"`
	Routing             proxy.Fact[struct{}]                  `json:"routing"`
	ClientBearerBarrier proxy.Fact[struct{}]                  `json:"client_bearer_barrier"`
	CollectedAt         string                                `json:"collected_at"`
	DurationMS          int64                                 `json:"duration_ms"`
}

type proxyDesiredProjectionV1 struct {
	Manager    string `json:"manager,omitempty"`
	Configured bool   `json:"configured"`
}

type proxyServiceProjectionV1 struct {
	Manager string `json:"manager,omitempty"`
	State   string `json:"state,omitempty"`
}

type proxyListenerProjectionV1 struct {
	State string `json:"state,omitempty"`
}
type proxyRuntimeProjectionV1 struct {
	Reachable bool   `json:"reachable"`
	Health    string `json:"health,omitempty"`
}

func projectProxySnapshot(snapshot proxy.ProxySnapshot) proxySnapshotProjectionV1 {
	services := []proxyServiceProjectionV1{}
	if snapshot.Service.Status == proxy.FactKnown && snapshot.Service.Value != nil {
		services = append(services, proxyServiceProjectionV1{Manager: snapshot.Service.Value.Manager, State: snapshot.Service.Value.State})
	}
	return proxySnapshotProjectionV1{
		Instance: proxy.UnavailableFact[struct{}]("instance_unavailable"),
		Verdict:  snapshot.Verdict,
		Desired: projectProxyFact(snapshot.Desired, func(value proxy.DesiredProxyState) proxyDesiredProjectionV1 {
			return proxyDesiredProjectionV1{Manager: value.Manager, Configured: value.Configured}
		}),
		Authority: proxy.UnavailableFact[struct{}]("authority_unavailable"),
		Services:  services,
		Listener: projectProxyFact(snapshot.Listener, func(value proxy.ListenerState) proxyListenerProjectionV1 {
			return proxyListenerProjectionV1{State: value.State}
		}),
		Runtime: projectProxyFact(snapshot.Runtime, func(value proxy.RuntimeIdentity) proxyRuntimeProjectionV1 {
			return proxyRuntimeProjectionV1{Reachable: value.Reachable, Health: value.Health}
		}),
		Routing:             proxy.UnavailableFact[struct{}]("routing_unavailable"),
		ClientBearerBarrier: proxy.UnavailableFact[struct{}]("client_bearer_barrier_unavailable"),
		CollectedAt:         snapshot.CollectedAt,
		DurationMS:          snapshot.DurationMS,
	}
}

func projectProxyFact[Source, Projection any](fact proxy.Fact[Source], project func(Source) Projection) proxy.Fact[Projection] {
	result := proxy.Fact[Projection]{Status: fact.Status, ErrorCode: fact.ErrorCode}
	if fact.Value != nil {
		value := project(*fact.Value)
		result.Value = &value
	}
	return result
}

type ProxyErrorV1 struct {
	Code         string   `json:"code"`
	Category     string   `json:"category"`
	ExitCode     int      `json:"exit_code"`
	Message      string   `json:"message"`
	Field        *string  `json:"field"`
	Retryable    bool     `json:"retryable"`
	EvidenceRefs []string `json:"evidence_refs"`
}

type ProxyWarningV1 struct {
	Code               string   `json:"code"`
	Category           string   `json:"category"`
	Message            string   `json:"message"`
	Field              *string  `json:"field"`
	EvidenceRefs       []string `json:"evidence_refs"`
	RemediationCommand *string  `json:"remediation_command"`
}

func projectProxyWarnings(snapshot proxy.ProxySnapshot) []ProxyWarningV1 {
	warnings := make([]ProxyWarningV1, 0, len(snapshot.Warnings))
	for _, code := range snapshot.Warnings {
		if code == "inspector_skew" {
			warnings = append(warnings, ProxyWarningV1{Code: code, Category: "compatibility", Message: "inspector differs from the running service", EvidenceRefs: []string{"inspector", "service"}})
		}
	}
	return warnings
}

func projectProxyErrors(snapshot proxy.ProxySnapshot) []ProxyErrorV1 {
	if snapshot.ExitCode == 0 {
		return []ProxyErrorV1{}
	}
	code, category, message := "proxy_degraded", "runtime", "proxy state is degraded"
	switch snapshot.Verdict {
	case proxy.ProxyVerdictLegacy:
		code, category, message = "proxy_legacy", "configuration", "legacy proxy management is active"
	case proxy.ProxyVerdictDown:
		code, message = "proxy_down", "proxy is not running"
	case proxy.ProxyVerdictConflicted:
		code, category, message = "proxy_conflicted", "ownership", "proxy facts conflict"
	case proxy.ProxyVerdictIndeterminate:
		code, message = "proxy_indeterminate", "proxy state is indeterminate"
	}
	return []ProxyErrorV1{{Code: code, Category: category, ExitCode: snapshot.ExitCode, Message: message, EvidenceRefs: []string{}}}
}

type ProxyDoctorCheck struct {
	ID                 string   `json:"id"`
	Status             string   `json:"status"`
	Severity           string   `json:"severity"`
	Summary            string   `json:"summary"`
	EvidenceRefs       []string `json:"evidence_refs"`
	RemediationCommand *string  `json:"remediation_command"`
}

func ProxyDoctorChecks(snapshot proxy.ProxySnapshot) []ProxyDoctorCheck {
	checks := []ProxyDoctorCheck{
		proxyDoctorCheck("service.unique", "service ownership", []string{"services"}, serviceDoctorStatus(snapshot)),
		proxyDoctorCheck("listener.owner", "listener ownership", []string{"listener"}, listenerDoctorStatus(snapshot)),
		proxyDoctorCheck("runtime.identity", "runtime identity", []string{"runtime"}, runtimeDoctorStatus(snapshot)),
		proxyDoctorCheck("client.bearer_barrier", "client bearer barrier", []string{"client_bearer_barrier"}, "unknown"),
	}
	if checks[0].Status == "fail" || checks[1].Status == "fail" {
		remediation := "cq proxy service start --manager launchagent"
		for index := 0; index < 2; index++ {
			if checks[index].Status == "fail" {
				checks[index].RemediationCommand = &remediation
			}
		}
	}
	return checks
}

func proxyDoctorCheck(id, summary string, evidence []string, status string) ProxyDoctorCheck {
	severity := "info"
	if status == "warn" {
		severity = "warning"
	} else if status == "fail" || status == "unknown" {
		severity = "critical"
	}
	return ProxyDoctorCheck{ID: id, Status: status, Severity: severity, Summary: summary, EvidenceRefs: evidence}
}

func serviceDoctorStatus(snapshot proxy.ProxySnapshot) string {
	if snapshot.Service.Status == proxy.FactKnown && snapshot.Service.Value != nil {
		if snapshot.Service.Value.State == "running" {
			return "pass"
		}
		return "fail"
	}
	if snapshot.Service.Status == proxy.FactAbsent || snapshot.Service.Status == proxy.FactInvalid {
		return "fail"
	}
	return "unknown"
}

func listenerDoctorStatus(snapshot proxy.ProxySnapshot) string {
	if snapshot.Listener.Status == proxy.FactAbsent || snapshot.Listener.Status == proxy.FactInvalid {
		return "fail"
	}
	if snapshot.Listener.Status != proxy.FactKnown || snapshot.Listener.Value == nil || snapshot.Listener.Value.State != "listening" {
		if snapshot.Listener.Status == proxy.FactKnown {
			return "fail"
		}
		return "unknown"
	}
	if snapshot.Service.Value == nil || snapshot.Process.Value == nil || snapshot.Service.Value.PID != snapshot.Listener.Value.PID || snapshot.Process.Value.PID != snapshot.Listener.Value.PID || snapshot.Service.Value.Executable != snapshot.Listener.Value.Executable || snapshot.Process.Value.Executable != snapshot.Listener.Value.Executable {
		return "fail"
	}
	return "pass"
}

func runtimeDoctorStatus(snapshot proxy.ProxySnapshot) string {
	if snapshot.Runtime.Status == proxy.FactKnown && snapshot.Runtime.Value != nil && snapshot.Runtime.Value.Reachable && snapshot.Runtime.Value.Health == "healthy" && snapshot.DataPlane.Status == proxy.FactKnown && snapshot.DataPlane.Value != nil && snapshot.DataPlane.Value.Proven {
		return "pass"
	}
	if snapshot.Runtime.Status == proxy.FactInvalid || snapshot.DataPlane.Status == proxy.FactInvalid {
		return "fail"
	}
	if snapshot.Runtime.Status == proxy.FactKnown || snapshot.DataPlane.Status == proxy.FactKnown || snapshot.Runtime.Status == proxy.FactAbsent || snapshot.DataPlane.Status == proxy.FactAbsent {
		return "warn"
	}
	return "unknown"
}
