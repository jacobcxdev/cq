package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestGrokPrimeCommandsPreserveOtherConfiguration(t *testing.T) {
	cfg := &proxy.Config{CodexWindowPriming: proxy.CodexWindowPrimingConfig{Enabled: true}, ZAIWindowPriming: proxy.ZAIWindowPrimingConfig{Enabled: true}}
	var output bytes.Buffer
	saved := 0
	deps := grokPrimeDependencies{loadConfig: func() (*proxy.Config, error) { return cfg, nil }, saveConfig: func(*proxy.Config) error { saved++; return nil }, output: &output}
	for _, command := range []string{"enable", "disable"} {
		if err := runGrokPrimeCommand(t.Context(), command, true, deps); err != nil {
			t.Fatal(err)
		}
		var roundtrip proxy.Config
		data, _ := json.Marshal(cfg)
		if err := json.Unmarshal(data, &roundtrip); err != nil {
			t.Fatal(err)
		}
		if roundtrip.GrokWindowPriming.Enabled != (command == "enable") || !roundtrip.CodexWindowPriming.Enabled || !roundtrip.ZAIWindowPriming.Enabled {
			t.Fatal("wrong provider configuration changed")
		}
	}
	if saved != 2 {
		t.Fatal("configuration not saved")
	}
}
func TestGrokPrimeStatusAndDisabledRuntimeDoNotCreateState(t *testing.T) {
	if p, err := buildGrokPrimer(&proxy.Config{}, userdirs.Roots{}, nil, nil); err != nil || p != nil {
		t.Fatal("disabled runtime accessed dependencies")
	}
	root := t.TempDir()
	for _, name := range []string{"CONFIG", "CACHE", "STATE", "DATA"} {
		t.Setenv("XDG_"+name+"_HOME", filepath.Join(root, name))
	}
	deps := defaultGrokPrimeDependencies()
	deps.output = io.Discard
	if err := runGrokPrimeCommand(context.Background(), "status", true, deps); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("status created persistent state")
	}
	cfg := &proxy.Config{GrokWindowPriming: proxy.GrokWindowPrimingConfig{Enabled: true}}
	if _, err := buildGrokPrimer(cfg, userdirs.Roots{}, fsutil.NewMemFS(), nil); err == nil {
		t.Fatal("enabled primer accepted missing authority")
	}
}
func TestGrokPrimeCommandsReachOrdinaryDispatcher(t *testing.T) {
	for _, command := range []string{"status", "enable", "disable", "now"} {
		authority, err := classifyOrdinaryAuthority([]string{"grok", "prime", command})
		if err != nil || authority.Row != "grok_prime" {
			t.Fatalf("%s authority=%#v err=%v", command, authority, err)
		}
	}
}
