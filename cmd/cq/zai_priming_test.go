package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestZAIPrimeEnableDisablePreservesCodexConfiguration(t *testing.T) {
	cfg := &proxy.Config{CodexWindowPriming: proxy.CodexWindowPrimingConfig{Enabled: true}}
	saved := 0
	deps := zaiPrimeDependencies{
		loadConfig: func() (*proxy.Config, error) { return cfg, nil },
		saveConfig: func(*proxy.Config) error { saved++; return nil }, output: io.Discard,
	}
	for _, command := range []string{"enable", "disable"} {
		if err := runZAIPrimeCommand(context.Background(), command, true, deps); err != nil {
			t.Fatal(err)
		}
		if cfg.ZAIWindowPriming.Enabled != (command == "enable") || !cfg.CodexWindowPriming.Enabled {
			t.Fatal("priming command changed wrong provider configuration")
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip proxy.Config
		if err := json.Unmarshal(data, &roundtrip); err != nil || roundtrip.ZAIWindowPriming.Enabled != cfg.ZAIWindowPriming.Enabled {
			t.Fatal("Z.ai configuration failed roundtrip")
		}
	}
	if saved != 2 {
		t.Fatal("configuration was not saved")
	}
}

func TestBuildZAIPrimerRequiresEnabledConfiguration(t *testing.T) {
	if primer, err := buildZAIPrimer(&proxy.Config{}, userdirs.Roots{}, fsutil.NewMemFS(), nil, nil); err != nil || primer != nil {
		t.Fatal("disabled priming accessed dependencies")
	}
	cfg := &proxy.Config{ZAIWindowPriming: proxy.ZAIWindowPrimingConfig{Enabled: true}}
	if _, err := buildZAIPrimer(cfg, userdirs.Roots{}, fsutil.NewMemFS(), nil, modelregistry.NewCatalog(modelregistry.Snapshot{})); err == nil {
		t.Fatal("enabled priming accepted missing dependencies")
	}
}
