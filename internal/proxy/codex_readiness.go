package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// CodexRoutingMode controls turn-aware routing for one transport.
type CodexRoutingMode string

const (
	CodexRoutingOff     CodexRoutingMode = "off"
	CodexRoutingObserve CodexRoutingMode = "observe"
	CodexRoutingEnforce CodexRoutingMode = "enforce"
)

func (m CodexRoutingMode) validate(field string) error {
	switch m {
	case CodexRoutingOff, CodexRoutingObserve, CodexRoutingEnforce:
		return nil
	default:
		return fmt.Errorf("invalid %s %q: must be \"off\", \"observe\", or \"enforce\"", field, m)
	}
}

// CodexRoutingTransport names one independently gated transport.
type CodexRoutingTransport string

const (
	CodexRoutingHTTP      CodexRoutingTransport = "http"
	CodexRoutingWebSocket CodexRoutingTransport = "websocket"
)

const (
	CodexReadinessMarkerVersion = 1
	CodexRoutingJournalVersion  = 1
	CurrentCodexParserSchema    = 1
	CurrentCodexLeaseSchema     = 2
	CodexHTTPFixtureHash        = "618be7afa604a4cdf1b34caf599a2d6e1b29db7da4ec71dd6527eb60d7e92dc1"
)

var CodexHTTPRequiredGates = []string{
	"strong-metadata",
	"lease-pinning",
	"pre-admission-failover",
	"synchronous-journal",
	"continuity-affinity",
	"compressed-replay",
	"installed-listener",
}

// CodexReadinessMarker is explicit, versioned proof for one enforcement tuple.
type CodexReadinessMarker struct {
	Version         int                   `json:"version"`
	Transport       CodexRoutingTransport `json:"transport"`
	CQBuild         string                `json:"cq_build"`
	ParserSchema    int                   `json:"parser_schema"`
	LeaseSchema     int                   `json:"lease_schema"`
	ClientBuild     string                `json:"client_build"`
	RetryBudget     int                   `json:"retry_budget"`
	FixtureHash     string                `json:"fixture_hash"`
	InstalledResult string                `json:"installed_result"`
	CompletedGates  []string              `json:"completed_gates"`
	ValidatedAt     time.Time             `json:"validated_at"`
}

// CodexTransportRequirements describes the exact runtime tuple a marker must match.
type CodexTransportRequirements struct {
	Transport          CodexRoutingTransport
	CQBuild            string
	ParserSchema       int
	LeaseSchema        int
	ClientBuild        string
	RetryBudget        int
	FixtureHash        string
	RequiredGates      []string
	ObserveImplemented bool
	EnforceImplemented bool
}

// CodexModeStatus is immutable for one proxy process.
type CodexModeStatus struct {
	Configured                  CodexRoutingMode `json:"configured"`
	Effective                   CodexRoutingMode `json:"effective"`
	InhibitionReason            string           `json:"inhibition_reason,omitempty"`
	ModeEpoch                   uint64           `json:"mode_epoch"`
	ShadowEpoch                 uint64           `json:"shadow_epoch,omitempty"`
	AuthoritativeEpoch          uint64           `json:"authoritative_epoch,omitempty"`
	RetainedAuthoritativeEpochs []uint64         `json:"retained_authoritative_epochs,omitempty"`
}

// CodexRoutingRuntime contains restart-scoped HTTP and WebSocket mode state.
type CodexRoutingRuntime struct {
	HTTP      CodexModeStatus `json:"http"`
	WebSocket CodexModeStatus `json:"websocket"`
}

type codexRoutingJournal struct {
	Version   int            `json:"version"`
	NextEpoch uint64         `json:"next_epoch"`
	HTTP      codexModeTrack `json:"http"`
	WebSocket codexModeTrack `json:"websocket"`
}

type codexModeTrack struct {
	CodexModeStatus
	RuntimeFingerprint string `json:"runtime_fingerprint,omitempty"`
}

// DefaultCodexRoutingRequirements returns the current binary's safe capability floor.
func DefaultCodexRoutingRequirements(cqBuild, clientBuild string) (CodexTransportRequirements, CodexTransportRequirements) {
	common := CodexTransportRequirements{
		CQBuild:            cqBuild,
		ParserSchema:       CurrentCodexParserSchema,
		LeaseSchema:        CurrentCodexLeaseSchema,
		ClientBuild:        clientBuild,
		ObserveImplemented: true,
	}
	httpReq := common
	httpReq.Transport = CodexRoutingHTTP
	httpReq.RetryBudget = 1
	httpReq.FixtureHash = CodexHTTPFixtureHash
	httpReq.RequiredGates = append([]string(nil), CodexHTTPRequiredGates...)
	httpReq.EnforceImplemented = true
	wsReq := common
	wsReq.Transport = CodexRoutingWebSocket
	return httpReq, wsReq
}

// OpenCodexRoutingRuntime resolves modes once for process lifetime.
func OpenCodexRoutingRuntime(cfg *Config, cqBuild, clientBuild string) (*CodexRoutingRuntime, error) {
	httpReq, wsReq := DefaultCodexRoutingRequirements(cqBuild, clientBuild)
	return openCodexRoutingRuntimeAt(configDir(), cfg, httpReq, wsReq)
}

func openCodexRoutingRuntimeAt(dir string, cfg *Config, httpReq, wsReq CodexTransportRequirements) (*CodexRoutingRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("proxy config is nil")
	}
	configuredHTTP := cfg.CodexTurnRouting
	configuredWS := cfg.CodexWSTurnRouting
	if configuredHTTP == "" {
		configuredHTTP = CodexRoutingOff
	}
	if configuredWS == "" {
		configuredWS = CodexRoutingOff
	}
	if err := configuredHTTP.validate("codex_turn_routing"); err != nil {
		return nil, err
	}
	if err := configuredWS.validate("codex_ws_turn_routing"); err != nil {
		return nil, err
	}

	journal, err := loadCodexRoutingJournal(dir)
	if err != nil {
		return nil, err
	}
	httpEffective, httpReason, httpFingerprint := resolveCodexMode(dir, configuredHTTP, httpReq)
	wsEffective, wsReason, wsFingerprint := resolveCodexMode(dir, configuredWS, wsReq)
	journal.HTTP = advanceCodexModeTrack(journal.HTTP, configuredHTTP, httpEffective, httpReason, httpFingerprint, &journal.NextEpoch)
	journal.WebSocket = advanceCodexModeTrack(journal.WebSocket, configuredWS, wsEffective, wsReason, wsFingerprint, &journal.NextEpoch)
	journal.Version = CodexRoutingJournalVersion
	if err := saveJSONFile(filepath.Join(dir, "codex-routing-mode.json"), &journal); err != nil {
		return nil, fmt.Errorf("save Codex routing mode journal: %w", err)
	}
	return &CodexRoutingRuntime{HTTP: journal.HTTP.CodexModeStatus, WebSocket: journal.WebSocket.CodexModeStatus}, nil
}

func resolveCodexMode(dir string, configured CodexRoutingMode, requirements CodexTransportRequirements) (CodexRoutingMode, string, string) {
	switch configured {
	case CodexRoutingOff:
		return CodexRoutingOff, "", ""
	case CodexRoutingObserve:
		if !requirements.ObserveImplemented {
			return CodexRoutingOff, "observe implementation unavailable", ""
		}
		return CodexRoutingObserve, "", requirementsFingerprint(requirements)
	case CodexRoutingEnforce:
		fallback := CodexRoutingOff
		if requirements.ObserveImplemented {
			fallback = CodexRoutingObserve
		}
		if !requirements.EnforceImplemented {
			return fallback, "enforcement implementation unavailable", requirementsFingerprint(requirements)
		}
		marker, err := LoadCodexReadinessMarker(dir, requirements.Transport)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fallback, "readiness marker missing", requirementsFingerprint(requirements)
			}
			return fallback, "readiness marker unreadable", requirementsFingerprint(requirements)
		}
		if err := ValidateCodexReadinessMarker(marker, requirements); err != nil {
			return fallback, err.Error(), requirementsFingerprint(requirements)
		}
		return CodexRoutingEnforce, "", markerFingerprint(marker)
	default:
		return CodexRoutingOff, "invalid configured mode", ""
	}
}

func advanceCodexModeTrack(previous codexModeTrack, configured, effective CodexRoutingMode, reason, fingerprint string, next *uint64) codexModeTrack {
	changed := previous.ModeEpoch == 0 || previous.Configured != configured || previous.Effective != effective || previous.RuntimeFingerprint != fingerprint
	if !changed {
		previous.InhibitionReason = reason
		return previous
	}
	(*next)++
	epoch := *next
	retained := append([]uint64(nil), previous.RetainedAuthoritativeEpochs...)
	if previous.AuthoritativeEpoch != 0 {
		retained = appendUniqueEpoch(retained, previous.AuthoritativeEpoch)
	}
	status := CodexModeStatus{
		Configured:                  configured,
		Effective:                   effective,
		InhibitionReason:            reason,
		ModeEpoch:                   epoch,
		RetainedAuthoritativeEpochs: retained,
	}
	switch effective {
	case CodexRoutingObserve:
		status.ShadowEpoch = epoch
	case CodexRoutingEnforce:
		status.AuthoritativeEpoch = epoch
	}
	return codexModeTrack{CodexModeStatus: status, RuntimeFingerprint: fingerprint}
}

func appendUniqueEpoch(epochs []uint64, epoch uint64) []uint64 {
	for _, existing := range epochs {
		if existing == epoch {
			return epochs
		}
	}
	return append(epochs, epoch)
}

func loadCodexRoutingJournal(dir string) (codexRoutingJournal, error) {
	path := filepath.Join(dir, "codex-routing-mode.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return codexRoutingJournal{Version: CodexRoutingJournalVersion}, nil
	}
	if err != nil {
		return codexRoutingJournal{}, fmt.Errorf("read Codex routing mode journal: %w", err)
	}
	var journal codexRoutingJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return codexRoutingJournal{}, fmt.Errorf("parse Codex routing mode journal: %w", err)
	}
	if journal.Version != CodexRoutingJournalVersion {
		return codexRoutingJournal{}, fmt.Errorf("unsupported Codex routing mode journal version %d", journal.Version)
	}
	return journal, nil
}

// LoadCodexReadinessMarker reads explicit validation proof for one transport.
func LoadCodexReadinessMarker(dir string, transport CodexRoutingTransport) (CodexReadinessMarker, error) {
	if transport != CodexRoutingHTTP && transport != CodexRoutingWebSocket {
		return CodexReadinessMarker{}, fmt.Errorf("invalid readiness marker transport %q", transport)
	}
	data, err := os.ReadFile(codexReadinessPath(dir, transport))
	if err != nil {
		return CodexReadinessMarker{}, err
	}
	var marker CodexReadinessMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return CodexReadinessMarker{}, err
	}
	return marker, nil
}

// SaveCodexReadinessMarker atomically persists explicit validation proof.
// Startup and upgrade paths never call this function.
func SaveCodexReadinessMarker(dir string, marker CodexReadinessMarker) error {
	if marker.Version != CodexReadinessMarkerVersion {
		return fmt.Errorf("invalid readiness marker version %d", marker.Version)
	}
	if marker.Transport != CodexRoutingHTTP && marker.Transport != CodexRoutingWebSocket {
		return fmt.Errorf("invalid readiness marker transport %q", marker.Transport)
	}
	if marker.CQBuild == "" || marker.ParserSchema <= 0 || marker.LeaseSchema <= 0 || marker.ClientBuild == "" || marker.RetryBudget < 0 || marker.FixtureHash == "" || marker.InstalledResult == "" || len(marker.CompletedGates) == 0 || marker.ValidatedAt.IsZero() {
		return fmt.Errorf("readiness marker is incomplete")
	}
	return saveJSONFile(codexReadinessPath(dir, marker.Transport), &marker)
}

// SaveDefaultCodexReadinessMarker stores explicit proof in CQ's runtime state.
func SaveDefaultCodexReadinessMarker(marker CodexReadinessMarker) error {
	return SaveCodexReadinessMarker(configDir(), marker)
}

// ValidateCodexReadinessMarker rejects any stale or incomplete tuple dimension.
func ValidateCodexReadinessMarker(marker CodexReadinessMarker, required CodexTransportRequirements) error {
	if required.Transport != CodexRoutingHTTP && required.Transport != CodexRoutingWebSocket {
		return fmt.Errorf("readiness requirements have invalid transport")
	}
	if required.CQBuild == "" || required.ParserSchema <= 0 || required.LeaseSchema <= 0 || required.ClientBuild == "" || required.RetryBudget < 0 || required.FixtureHash == "" || len(required.RequiredGates) == 0 {
		return fmt.Errorf("readiness requirements are incomplete")
	}
	checks := []struct {
		ok     bool
		reason string
	}{
		{marker.Version == CodexReadinessMarkerVersion, "readiness marker version mismatch"},
		{marker.Transport == required.Transport, "readiness marker transport mismatch"},
		{marker.CQBuild == required.CQBuild, "readiness marker CQ build mismatch"},
		{marker.ParserSchema == required.ParserSchema, "readiness marker parser schema mismatch"},
		{marker.LeaseSchema == required.LeaseSchema, "readiness marker lease schema mismatch"},
		{marker.ClientBuild == required.ClientBuild, "readiness marker client build mismatch"},
		{marker.RetryBudget == required.RetryBudget, "readiness marker retry budget mismatch"},
		{marker.FixtureHash == required.FixtureHash, "readiness marker fixture hash mismatch"},
		{marker.InstalledResult == "passed", "readiness marker installed result not passed"},
		{!marker.ValidatedAt.IsZero(), "readiness marker validation time missing"},
	}
	for _, check := range checks {
		if !check.ok {
			return fmt.Errorf("%s", check.reason)
		}
	}
	gates := make(map[string]bool, len(marker.CompletedGates))
	for _, gate := range marker.CompletedGates {
		gates[gate] = true
	}
	for _, gate := range required.RequiredGates {
		if !gates[gate] {
			return fmt.Errorf("readiness marker missing gate %q", gate)
		}
	}
	return nil
}

func codexReadinessPath(dir string, transport CodexRoutingTransport) string {
	return filepath.Join(dir, "codex-readiness-"+string(transport)+".json")
}

func requirementsFingerprint(requirements CodexTransportRequirements) string {
	gates := append([]string(nil), requirements.RequiredGates...)
	sort.Strings(gates)
	value := struct {
		Transport    CodexRoutingTransport
		CQBuild      string
		ParserSchema int
		LeaseSchema  int
		ClientBuild  string
		RetryBudget  int
		FixtureHash  string
		Gates        []string
	}{requirements.Transport, requirements.CQBuild, requirements.ParserSchema, requirements.LeaseSchema, requirements.ClientBuild, requirements.RetryBudget, requirements.FixtureHash, gates}
	data, _ := json.Marshal(value)
	return hashBytes(data)
}

func markerFingerprint(marker CodexReadinessMarker) string {
	copy := marker
	copy.ValidatedAt = time.Time{}
	copy.CompletedGates = append([]string(nil), marker.CompletedGates...)
	sort.Strings(copy.CompletedGates)
	data, _ := json.Marshal(copy)
	return hashBytes(data)
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func saveJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ConfiguredModesDiffer reports a restart-required config change.
func (r *CodexRoutingRuntime) ConfiguredModesDiffer(cfg *Config) bool {
	if r == nil || cfg == nil {
		return false
	}
	httpMode := cfg.CodexTurnRouting
	if httpMode == "" {
		httpMode = CodexRoutingOff
	}
	wsMode := cfg.CodexWSTurnRouting
	if wsMode == "" {
		wsMode = CodexRoutingOff
	}
	return r.HTTP.Configured != httpMode || r.WebSocket.Configured != wsMode
}
