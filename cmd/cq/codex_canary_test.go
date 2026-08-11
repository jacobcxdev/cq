package main

import "testing"

import "github.com/jacobcxdev/cq/internal/proxy"

func TestRunCodexCanaryRejectsUnknownCommand(t *testing.T) {
	if err := runCodexCanary([]string{"unknown"}); err == nil {
		t.Fatal("expected unknown command error")
	}
}

func TestRunCodexCanaryRejectsManualBaselineAcknowledgement(t *testing.T) {
	if err := runCodexCanary([]string{"acknowledge-explicit-switch"}); err == nil {
		t.Fatal("expected manual acknowledgement rejection")
	}
}

func TestValidateCodexCanaryStartConfigRejectsPayloadDiagnostics(t *testing.T) {
	err := validateCodexCanaryStartConfig(&proxy.Config{PayloadDiagnosticsLog: "/synthetic/private-payloads.jsonl"})
	if err == nil {
		t.Fatal("expected payload diagnostics rejection")
	}
	if got := err.Error(); got != "Codex canary requires payload diagnostics to be disabled" {
		t.Fatalf("error = %q", got)
	}
}

func TestValidateCodexCanaryStartConfigRequiresHTTPEnforcement(t *testing.T) {
	for _, mode := range []proxy.CodexRoutingMode{"", proxy.CodexRoutingOff, proxy.CodexRoutingObserve} {
		err := validateCodexCanaryStartConfig(&proxy.Config{CodexTurnRouting: mode})
		if err == nil || err.Error() != "Codex canary requires HTTP routing enforcement" {
			t.Fatalf("mode %q error = %v", mode, err)
		}
	}
	if err := validateCodexCanaryStartConfig(&proxy.Config{CodexTurnRouting: proxy.CodexRoutingEnforce}); err != nil {
		t.Fatalf("enforce configuration error = %v", err)
	}
}
