package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestCodexPrimerUsageURLFollowsCodexUpstream(t *testing.T) {
	got, err := CodexPrimerUsageURL("https://chatgpt.example/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://chatgpt.example/backend-api/wham/usage" {
		t.Fatalf("usage URL = %q", got)
	}
	if _, err := CodexPrimerUsageURL("https://chatgpt.example/v1"); err == nil {
		t.Fatal("non-Codex upstream accepted")
	}
}

func TestCodexPrimerUsageReadsExactAccountDescriptors(t *testing.T) {
	executor := &primerCaptureExecutor{stream: `{
		"plan_type":"pro",
		"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1786834644}},
		"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1786834644}}}]
	}`}
	reader := &CodexPrimerUsageReader{Router: primerTestRouter(executor), UsageURL: "https://chatgpt.example/backend-api/wham/usage"}

	observation, err := reader.Read(context.Background(), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 1 || executor.request.Method != http.MethodGet || len(observation.Windows) != 2 || observation.Result.Plan != "pro" {
		t.Fatalf("request/observation = calls:%d method:%s %+v", executor.calls, executor.request.Method, observation)
	}
}

func TestCodexPrimerUsageRejectsAuthFailure(t *testing.T) {
	executor := &primerCaptureExecutor{status: http.StatusUnauthorized}
	reader := &CodexPrimerUsageReader{Router: primerTestRouter(executor), UsageURL: "https://chatgpt.example/backend-api/wham/usage"}

	if _, err := reader.Read(context.Background(), "account-1"); err == nil {
		t.Fatal("auth failure returned no error")
	}
	if executor.calls != 1 {
		t.Fatalf("calls = %d, want exact-account attempt only", executor.calls)
	}
}

func TestCodexPrimerUsageBoundsTotalTime(t *testing.T) {
	reader := &CodexPrimerUsageReader{
		Router: primerTestRouter(&blockingPrimerExecutor{}), UsageURL: "https://chatgpt.example/backend-api/wham/usage",
		Timeout: time.Millisecond,
	}
	if _, err := reader.Read(context.Background(), "account-1"); err == nil {
		t.Fatal("usage timeout returned no error")
	}
}
