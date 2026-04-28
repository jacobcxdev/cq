package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
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
