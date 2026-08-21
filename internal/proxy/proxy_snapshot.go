package proxy

// FactStatus describes whether one independently collected proxy fact can be
// trusted. Unknown facts remain explicit instead of being converted to a
// successful zero value.
type FactStatus string

const (
	FactKnown            FactStatus = "known"
	FactAbsent           FactStatus = "absent"
	FactInvalid          FactStatus = "invalid"
	FactUnavailable      FactStatus = "unavailable"
	FactPermissionDenied FactStatus = "permission_denied"
)

type Fact[T any] struct {
	Status    FactStatus `json:"status"`
	Value     *T         `json:"value"`
	ErrorCode *string    `json:"error_code"`
}

func KnownFact[T any](value T) Fact[T] { return Fact[T]{Status: FactKnown, Value: &value} }
func AbsentFact[T any]() Fact[T]       { return Fact[T]{Status: FactAbsent} }

func InvalidFact[T any](code string) Fact[T] {
	return Fact[T]{Status: FactInvalid, ErrorCode: factErrorCode(code)}
}

func UnavailableFact[T any](code string) Fact[T] {
	return Fact[T]{Status: FactUnavailable, ErrorCode: factErrorCode(code)}
}

func PermissionDeniedFact[T any](code string) Fact[T] {
	return Fact[T]{Status: FactPermissionDenied, ErrorCode: factErrorCode(code)}
}

func factErrorCode(code string) *string { return &code }

type InspectorIdentity struct {
	Executable string `json:"executable,omitempty"`
	Version    string `json:"version,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

type DesiredProxyState struct {
	Manager    string `json:"manager,omitempty"`
	Configured bool   `json:"configured"`
	Listener   string `json:"listener,omitempty"`
}

type ServiceState struct {
	Manager    string `json:"manager,omitempty"`
	State      string `json:"state,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Executable string `json:"executable,omitempty"`
}

type ListenerState struct {
	State      string `json:"state,omitempty"`
	Listener   string `json:"listener,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Executable string `json:"executable,omitempty"`
}

type ProcessState struct {
	PID        int    `json:"pid,omitempty"`
	Executable string `json:"executable,omitempty"`
}

type RuntimeIdentity struct {
	Reachable  bool   `json:"reachable"`
	PID        int    `json:"pid,omitempty"`
	Executable string `json:"executable,omitempty"`
	Health     string `json:"health,omitempty"`
}

type DataPlaneProof struct {
	Proven bool   `json:"proven"`
	Code   string `json:"code,omitempty"`
}

type ProxyVerdict string

const (
	ProxyVerdictHealthy       ProxyVerdict = "healthy"
	ProxyVerdictLegacy        ProxyVerdict = "legacy"
	ProxyVerdictDegraded      ProxyVerdict = "degraded"
	ProxyVerdictDown          ProxyVerdict = "down"
	ProxyVerdictConflicted    ProxyVerdict = "conflicted"
	ProxyVerdictIndeterminate ProxyVerdict = "indeterminate"
)

type ProxySnapshot struct {
	Inspector   Fact[InspectorIdentity] `json:"inspector"`
	Desired     Fact[DesiredProxyState] `json:"desired"`
	Service     Fact[ServiceState]      `json:"service"`
	Listener    Fact[ListenerState]     `json:"listener"`
	Process     Fact[ProcessState]      `json:"process"`
	Runtime     Fact[RuntimeIdentity]   `json:"runtime"`
	DataPlane   Fact[DataPlaneProof]    `json:"data_plane"`
	Verdict     ProxyVerdict            `json:"verdict"`
	ExitCode    int                     `json:"exit_code"`
	Warnings    []string                `json:"warnings"`
	CollectedAt string                  `json:"collected_at"`
	DurationMS  int64                   `json:"duration_ms"`
}

// ReconcileProxySnapshot applies the closed status precedence after every
// independent fact has been collected. A public health response alone never
// establishes data-plane or process authority.
func ReconcileProxySnapshot(snapshot ProxySnapshot) ProxySnapshot {
	snapshot.Warnings = nil
	normaliseProxySnapshotFacts(&snapshot)
	if factInvalid(snapshot.Desired.Status, snapshot.Service.Status, snapshot.Listener.Status, snapshot.Process.Status, snapshot.Runtime.Status, snapshot.DataPlane.Status) {
		return withProxyVerdict(snapshot, ProxyVerdictConflicted, 3)
	}
	if factUnavailable(snapshot.Desired.Status, snapshot.Service.Status, snapshot.Listener.Status, snapshot.Process.Status) {
		return withProxyVerdict(snapshot, ProxyVerdictIndeterminate, 4)
	}

	service := snapshot.Service.Value
	listener := snapshot.Listener.Value
	process := snapshot.Process.Value
	runtime := snapshot.Runtime.Value
	if listener != nil && listener.State == "foreign" {
		return withProxyVerdict(snapshot, ProxyVerdictConflicted, 3)
	}
	if identitiesConflict(service, listener, process, runtime) {
		return withProxyVerdict(snapshot, ProxyVerdictConflicted, 3)
	}
	if desiredEffectiveConflict(snapshot.Desired.Value, service, listener) {
		return withProxyVerdict(snapshot, ProxyVerdictConflicted, 3)
	}
	if listener != nil && listener.State == "listening" && (service == nil || service.State != "running" || process == nil) {
		return withProxyVerdict(snapshot, ProxyVerdictConflicted, 3)
	}
	if service != nil && service.State == "crash_looping" {
		return withProxyVerdict(snapshot, ProxyVerdictDegraded, 1)
	}
	if snapshot.Listener.Status == FactAbsent {
		if service != nil && service.State != "stopped" && service.State != "absent" && service.State != "" {
			return withProxyVerdict(snapshot, ProxyVerdictDegraded, 1)
		}
		return withProxyVerdict(snapshot, ProxyVerdictDown, 2)
	}
	if factUnavailable(snapshot.Runtime.Status, snapshot.DataPlane.Status) {
		return withProxyVerdict(snapshot, ProxyVerdictIndeterminate, 4)
	}
	desired := snapshot.Desired.Value
	if desired == nil || !desired.Configured || desired.Manager == "" || desired.Listener == "" || service == nil || desired.Manager != service.Manager || service.State != "running" || listener == nil || desired.Listener != listener.Listener || listener.State != "listening" || process == nil || runtime == nil || !runtime.Reachable || runtime.Health != "healthy" || !dataPlaneSatisfiesManager(service.Manager, snapshot.DataPlane) {
		return withProxyVerdict(snapshot, ProxyVerdictDegraded, 1)
	}
	if service.Manager == "manual" {
		return withProxyVerdict(snapshot, ProxyVerdictLegacy, 1)
	}
	if snapshot.Inspector.Status == FactKnown && snapshot.Inspector.Value != nil && service != nil && snapshot.Inspector.Value.Executable != "" && service.Executable != "" && snapshot.Inspector.Value.Executable != service.Executable {
		snapshot.Warnings = []string{"inspector_skew"}
	}
	return withProxyVerdict(snapshot, ProxyVerdictHealthy, 0)
}

func dataPlaneSatisfiesManager(manager string, fact Fact[DataPlaneProof]) bool {
	if fact.Status != FactKnown || fact.Value == nil {
		return false
	}
	return fact.Value.Proven || (manager == "homebrew" && fact.Value.Code == "unproven")
}

func normaliseProxySnapshotFacts(snapshot *ProxySnapshot) {
	snapshot.Inspector = normaliseFact(snapshot.Inspector)
	snapshot.Desired = normaliseFact(snapshot.Desired)
	snapshot.Service = normaliseFact(snapshot.Service)
	snapshot.Listener = normaliseFact(snapshot.Listener)
	snapshot.Process = normaliseFact(snapshot.Process)
	snapshot.Runtime = normaliseFact(snapshot.Runtime)
	snapshot.DataPlane = normaliseFact(snapshot.DataPlane)

	if snapshot.Desired.Status == FactKnown && !validDesiredProxyState(*snapshot.Desired.Value) {
		snapshot.Desired = InvalidFact[DesiredProxyState]("invalid_fact")
	}
	if snapshot.Service.Status == FactKnown && !validServiceState(*snapshot.Service.Value) {
		snapshot.Service = InvalidFact[ServiceState]("invalid_fact")
	}
	if snapshot.Listener.Status == FactKnown && !validListenerState(*snapshot.Listener.Value) {
		snapshot.Listener = InvalidFact[ListenerState]("invalid_fact")
	}
	if snapshot.Process.Status == FactKnown && (snapshot.Process.Value.PID <= 0 || snapshot.Process.Value.Executable == "") {
		snapshot.Process = InvalidFact[ProcessState]("invalid_fact")
	}
	if snapshot.Runtime.Status == FactKnown && !validRuntimeIdentity(*snapshot.Runtime.Value) {
		snapshot.Runtime = InvalidFact[RuntimeIdentity]("invalid_fact")
	}
	if snapshot.DataPlane.Status == FactKnown {
		value := snapshot.DataPlane.Value
		if (value.Proven && value.Code != "") || (!value.Proven && !validFactCode(value.Code)) {
			snapshot.DataPlane = InvalidFact[DataPlaneProof]("invalid_fact")
		}
	}
}

func normaliseFact[T any](fact Fact[T]) Fact[T] {
	switch fact.Status {
	case FactKnown:
		if fact.Value != nil && fact.ErrorCode == nil {
			return fact
		}
	case FactAbsent:
		if fact.Value == nil && (fact.ErrorCode == nil || validFactCode(*fact.ErrorCode)) {
			return fact
		}
	case FactInvalid, FactUnavailable, FactPermissionDenied:
		if fact.Value == nil && fact.ErrorCode != nil && validFactCode(*fact.ErrorCode) {
			return fact
		}
	}
	return InvalidFact[T]("invalid_fact")
}

func validFactCode(code string) bool {
	if len(code) == 0 || len(code) > 64 || code[0] < 'a' || code[0] > 'z' {
		return false
	}
	for _, character := range code[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func validDesiredProxyState(value DesiredProxyState) bool {
	return value.Manager == "" || value.Manager == "launchagent" || value.Manager == "homebrew" || value.Manager == "manual"
}

func validServiceState(value ServiceState) bool {
	if value.Manager != "launchagent" && value.Manager != "homebrew" && value.Manager != "manual" {
		return false
	}
	switch value.State {
	case "running":
		return value.PID > 0 && value.Executable != ""
	case "stopped", "crash_looping", "absent":
		return value.PID == 0 && value.Executable == ""
	default:
		return false
	}
}

func validListenerState(value ListenerState) bool {
	return (value.State == "listening" || value.State == "foreign") && value.PID > 0 && value.Executable != ""
}

func validRuntimeIdentity(value RuntimeIdentity) bool {
	if value.Health != "healthy" && value.Health != "degraded" && value.Health != "unhealthy" {
		return false
	}
	if value.Reachable {
		return value.PID > 0 && value.Executable != ""
	}
	return value.PID == 0 && value.Executable == ""
}

func withProxyVerdict(snapshot ProxySnapshot, verdict ProxyVerdict, exitCode int) ProxySnapshot {
	snapshot.Verdict = verdict
	snapshot.ExitCode = exitCode
	if snapshot.Warnings == nil {
		snapshot.Warnings = []string{}
	}
	return snapshot
}

func factInvalid(statuses ...FactStatus) bool {
	for _, status := range statuses {
		if status == FactInvalid {
			return true
		}
	}
	return false
}

func factUnavailable(statuses ...FactStatus) bool {
	for _, status := range statuses {
		if status == FactUnavailable || status == FactPermissionDenied || status == "" {
			return true
		}
	}
	return false
}

func identitiesConflict(service *ServiceState, listener *ListenerState, process *ProcessState, runtime *RuntimeIdentity) bool {
	if service != nil && listener != nil && service.PID != 0 && listener.PID != 0 && service.PID != listener.PID {
		return true
	}
	if listener != nil && process != nil && listener.PID != 0 && process.PID != 0 && listener.PID != process.PID {
		return true
	}
	if listener != nil && runtime != nil && listener.PID != 0 && runtime.PID != 0 && listener.PID != runtime.PID {
		return true
	}
	for _, pair := range [][2]string{
		{valueOrEmpty(service, func(value *ServiceState) string { return value.Executable }), valueOrEmpty(listener, func(value *ListenerState) string { return value.Executable })},
		{valueOrEmpty(listener, func(value *ListenerState) string { return value.Executable }), valueOrEmpty(process, func(value *ProcessState) string { return value.Executable })},
		{valueOrEmpty(listener, func(value *ListenerState) string { return value.Executable }), valueOrEmpty(runtime, func(value *RuntimeIdentity) string { return value.Executable })},
	} {
		if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
			return true
		}
	}
	return false
}

func desiredEffectiveConflict(desired *DesiredProxyState, service *ServiceState, listener *ListenerState) bool {
	if desired == nil {
		return false
	}
	if desired.Manager != "" && service != nil && desired.Manager != service.Manager {
		return true
	}
	return desired.Listener != "" && listener != nil && listener.Listener != "" && desired.Listener != listener.Listener
}

func valueOrEmpty[T any](value *T, selectValue func(*T) string) string {
	if value == nil {
		return ""
	}
	return selectValue(value)
}
