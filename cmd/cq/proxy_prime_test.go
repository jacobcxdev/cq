package main

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestProxyPrimeEnableDisable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := runProxyPrime([]string{"enable"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CodexWindowPriming.Enabled {
		t.Fatal("priming not enabled")
	}
	if err := runProxyPrime([]string{"disable"}); err != nil {
		t.Fatal(err)
	}
	cfg, err = proxy.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexWindowPriming.Enabled {
		t.Fatal("priming not disabled")
	}
}

func TestProxyPrimeStatusDoesNotChangeConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := proxy.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.CodexWindowPriming.ModelOverrides = map[string]string{"codex_spark": "gpt-5.3-codex-spark"}
	if err := proxy.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := runProxyPrime([]string{"status"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := proxy.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CodexWindowPriming.Enabled || loaded.CodexWindowPriming.ModelOverrides["codex_spark"] != "gpt-5.3-codex-spark" {
		t.Fatalf("status changed config: %+v", loaded.CodexWindowPriming)
	}
}

func TestProxyPrimeRejectsUnknownCommand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := runProxyPrime([]string{"now"}); err == nil {
		t.Fatal("unknown prime command accepted")
	}
}

func TestProxyPrimeSubcommandHelp(t *testing.T) {
	for _, command := range []string{"status", "enable", "disable"} {
		if err := runProxyPrime([]string{command, "--help"}); err != nil {
			t.Fatalf("%s help: %v", command, err)
		}
	}
}
