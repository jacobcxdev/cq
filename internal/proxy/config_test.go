package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDiagnosticsLogJSONRoundTrip(t *testing.T) {
	cfg := Config{
		Port:           DefaultPort,
		ClaudeUpstream: DefaultUpstream,
		CodexUpstream:  DefaultCodexUpstream,
		LocalToken:     "tok",
		DiagnosticsLog: "/tmp/cq-routes.jsonl",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["diagnostics_log"]) != `"/tmp/cq-routes.jsonl"` {
		t.Fatalf("diagnostics_log = %s, want configured path in %s", raw["diagnostics_log"], data)
	}

	var roundTrip Config
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.DiagnosticsLog != cfg.DiagnosticsLog {
		t.Fatalf("DiagnosticsLog = %q, want %q", roundTrip.DiagnosticsLog, cfg.DiagnosticsLog)
	}
}

func TestConfigPreservesUnknownFieldsAcrossLoadSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	configDir := filepath.Join(dir, "cq")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "proxy.json")
	original := []byte(`{
		"port": 19280,
		"local_token": "token",
		"codex_turn_routing": "observe",
		"codex_ws_turn_routing": "off",
		"future_scalar": 7,
		"future_object": {"nested": [1, {"keep": true}]}
	}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.PinnedClaudeAccount = "person@example.test"
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["future_scalar"]) != "7" {
		t.Fatalf("future_scalar = %s, want 7", raw["future_scalar"])
	}
	var futureObject struct {
		Nested []any `json:"nested"`
	}
	if err := json.Unmarshal(raw["future_object"], &futureObject); err != nil || len(futureObject.Nested) != 2 {
		t.Fatalf("future_object = %s, want preserved object: %v", raw["future_object"], err)
	}
	if string(raw["codex_turn_routing"]) != `"observe"` || string(raw["codex_ws_turn_routing"]) != `"off"` {
		t.Fatalf("routing modes lost: %s", data)
	}
}

func TestGeneratedConfigDefaultsCodexRoutingOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexTurnRouting != CodexRoutingOff || cfg.CodexWSTurnRouting != CodexRoutingOff {
		t.Fatalf("modes = %q/%q, want off/off", cfg.CodexTurnRouting, cfg.CodexWSTurnRouting)
	}
	data, err := os.ReadFile(filepath.Join(dir, "cq", "proxy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"codex_turn_routing": "off"`) || !strings.Contains(string(data), `"codex_ws_turn_routing": "off"`) {
		t.Fatalf("generated config missing explicit safe modes: %s", data)
	}
}

func TestConfigRejectsInvalidCodexRoutingMode(t *testing.T) {
	for _, field := range []string{"codex_turn_routing", "codex_ws_turn_routing"} {
		t.Run(field, func(t *testing.T) {
			var cfg Config
			data := []byte(`{"local_token":"token","` + field + `":"automatic"}`)
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.setDefaults()
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("validate error = %v, want %s error", err, field)
			}
		})
	}
}

func TestConfigDiagnosticsLogDefaultDisabled(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"port":19280,"local_token":"tok"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.DiagnosticsLog != "" {
		t.Fatalf("DiagnosticsLog = %q, want empty", cfg.DiagnosticsLog)
	}

	data, err := json.Marshal(Config{Port: DefaultPort, LocalToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["diagnostics_log"]; ok {
		t.Fatalf("diagnostics_log should be omitted when empty: %s", data)
	}
}

func TestConfigPayloadDiagnosticsLogJSONRoundTrip(t *testing.T) {
	cfg := Config{
		Port:                  DefaultPort,
		ClaudeUpstream:        DefaultUpstream,
		CodexUpstream:         DefaultCodexUpstream,
		LocalToken:            "tok",
		PayloadDiagnosticsLog: "/tmp/cq-payloads.jsonl",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["payload_diagnostics_log"]) != `"/tmp/cq-payloads.jsonl"` {
		t.Fatalf("payload_diagnostics_log = %s, want configured path in %s", raw["payload_diagnostics_log"], data)
	}

	var roundTrip Config
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.PayloadDiagnosticsLog != cfg.PayloadDiagnosticsLog {
		t.Fatalf("PayloadDiagnosticsLog = %q, want %q", roundTrip.PayloadDiagnosticsLog, cfg.PayloadDiagnosticsLog)
	}
}

func TestConfigPayloadDiagnosticsLogDefaultDisabled(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"port":19280,"local_token":"tok"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PayloadDiagnosticsLog != "" {
		t.Fatalf("PayloadDiagnosticsLog = %q, want empty", cfg.PayloadDiagnosticsLog)
	}

	data, err := json.Marshal(Config{Port: DefaultPort, LocalToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["payload_diagnostics_log"]; ok {
		t.Fatalf("payload_diagnostics_log should be omitted when empty: %s", data)
	}
}

func TestConfigDiagnosticsLogPersisted(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(t.TempDir(), "routes.jsonl")

	if err := SaveConfig(&Config{
		LocalToken:     "tok",
		DiagnosticsLog: path,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(configHome, "cq", "proxy.json"))
	if err != nil {
		t.Fatalf("read proxy.json: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("proxy.json is not valid JSON: %s", data)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var persisted string
	if err := json.Unmarshal(raw["diagnostics_log"], &persisted); err != nil {
		t.Fatalf("unmarshal diagnostics_log: %v", err)
	}
	if persisted != path {
		t.Fatalf("persisted diagnostics_log = %q, want %q in %s", persisted, path, data)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DiagnosticsLog != path {
		t.Fatalf("loaded DiagnosticsLog = %q, want %q", cfg.DiagnosticsLog, path)
	}
}
