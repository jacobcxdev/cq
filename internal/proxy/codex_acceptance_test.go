package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testCodexAcceptanceRunner func(context.Context, codexAcceptanceCommand) ([]byte, error)

func (runner testCodexAcceptanceRunner) Run(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
	return runner(ctx, command)
}

type syntheticCodexClientOptions struct {
	metadata        bool
	zstd            bool
	requests        int
	output          string
	modelRequests   int
	wrongBootstrap  bool
	bootstrapAPIKey bool
	unexpectedRoute bool
	egress          bool
}

func successfulSyntheticCodexClient() syntheticCodexClientOptions {
	return syntheticCodexClientOptions{metadata: true, zstd: true, requests: 1, output: "PONG\n"}
}

func TestRunCodexHTTPInstalledAcceptanceWithInjectedRunner(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "user-api-key-must-not-be-inherited")
	t.Setenv("CODEX_ACCESS_TOKEN", "user-access-token-must-not-be-inherited")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "user-codex-home"))

	var mu sync.Mutex
	var commands []codexAcceptanceCommand
	runner := testCodexAcceptanceRunner(func(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
		mu.Lock()
		commands = append(commands, command)
		mu.Unlock()
		if command.captureOutput {
			return []byte(codexAcceptanceVersion + "\n"), nil
		}
		assertCodexAcceptanceCommandIsolation(t, command)
		return nil, runSyntheticCodexAcceptanceClient(ctx, command, successfulSyntheticCodexClient())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := runCodexHTTPInstalledAcceptance(ctx, codexAcceptanceDependencies{
		executable: "/synthetic/codex",
		runner:     runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Turns != 20 || result.Requests != 40 || result.SelectorCalls != 20 || result.ContinuityErrors != 0 || result.UnknownEvents != 0 {
		t.Fatalf("enforced result = %#v", result)
	}
	if result.InstalledVersion != codexAcceptanceVersion || result.InstalledRequests != 1 || result.InstalledModelRequests != 1 || result.InstalledAttempts != 1 || result.InstalledSelectorCalls != 1 || result.InstalledResolutions != 1 {
		t.Fatalf("installed request result = %#v", result)
	}
	if result.InstalledStrongKeys != 1 || result.InstalledZstdRequests != 1 || result.InstalledUnknownEvents != 0 || result.InstalledContinuityErrors != 0 || result.InstalledQuiescentLeases != 1 {
		t.Fatalf("installed metadata result = %#v", result)
	}
	if result.HeadroomRequests != 1 || result.HeadroomParseErrors != 0 || result.UnexpectedRoutes != 0 || result.EgressAttempts != 0 || !result.PongVerified {
		t.Fatalf("installed boundary result = %#v", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 2 {
		t.Fatalf("command calls = %d, want version + exec", len(commands))
	}
	if !commands[0].loopbackOnly || commandEnv(commands[0].env, "HOME") != "" || commandEnv(commands[0].env, "CODEX_HOME") != "" {
		t.Fatal("version probe was not confined from user state and external network")
	}
	if _, err := os.Stat(commandEnv(commands[1].env, "CODEX_HOME")); !os.IsNotExist(err) {
		t.Fatalf("isolated CODEX_HOME was not removed: %v", err)
	}
}

func TestCodexInstalledAcceptanceFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		options syntheticCodexClientOptions
	}{
		{name: "missing strong metadata", options: syntheticCodexClientOptions{zstd: true, requests: 1, output: "PONG\n"}},
		{name: "uncompressed request", options: syntheticCodexClientOptions{metadata: true, requests: 1, output: "PONG\n"}},
		{name: "retry", options: syntheticCodexClientOptions{metadata: true, zstd: true, requests: 2, output: "PONG\n"}},
		{name: "wrong output", options: syntheticCodexClientOptions{metadata: true, zstd: true, requests: 1, output: "NOT PONG\n"}},
		{name: "padded output", options: syntheticCodexClientOptions{metadata: true, zstd: true, requests: 1, output: "PONG \n"}},
		{name: "excess model refreshes", options: syntheticCodexClientOptions{metadata: true, zstd: true, requests: 1, modelRequests: 3, output: "PONG\n"}},
		{name: "wrong bootstrap auth", options: syntheticCodexClientOptions{metadata: true, zstd: true, requests: 1, output: "PONG\n", wrongBootstrap: true}},
		{name: "bootstrap API key", options: syntheticCodexClientOptions{metadata: true, zstd: true, requests: 1, output: "PONG\n", bootstrapAPIKey: true}},
		{name: "unexpected local route", options: syntheticCodexClientOptions{metadata: true, zstd: true, requests: 1, output: "PONG\n", unexpectedRoute: true}},
		{name: "egress attempt", options: syntheticCodexClientOptions{metadata: true, zstd: true, requests: 1, output: "PONG\n", egress: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := syntheticCodexAcceptanceRunner(test.options)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := runCodexInstalledClientAcceptance(ctx, codexAcceptanceDependencies{
				executable: "/synthetic/codex",
				runner:     runner,
			}); err == nil {
				t.Fatal("expected incomplete evidence rejection")
			}
		})
	}
}

func TestCodexInstalledAcceptanceAllowsObservedModelRefreshRace(t *testing.T) {
	options := successfulSyntheticCodexClient()
	options.modelRequests = 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runCodexInstalledClientAcceptance(ctx, codexAcceptanceDependencies{
		executable: "/synthetic/codex",
		runner:     syntheticCodexAcceptanceRunner(options),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.InstalledModelRequests != 2 {
		t.Fatalf("model requests = %d, want 2", result.InstalledModelRequests)
	}
}

func TestCodexInstalledAcceptanceRejectsWrongVersion(t *testing.T) {
	runner := testCodexAcceptanceRunner(func(_ context.Context, command codexAcceptanceCommand) ([]byte, error) {
		if !command.captureOutput {
			t.Fatal("exec must not run after version mismatch")
		}
		return []byte("codex-cli 0.145.0\n"), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := runCodexInstalledClientAcceptance(ctx, codexAcceptanceDependencies{
		executable: "/synthetic/codex",
		runner:     runner,
	}); err == nil {
		t.Fatal("expected version mismatch rejection")
	}
}

func TestCodexInstalledAcceptanceDoesNotLeakRunnerErrors(t *testing.T) {
	const private = "private-child-output-must-not-leak"
	var isolatedCodexHome string
	runner := testCodexAcceptanceRunner(func(_ context.Context, command codexAcceptanceCommand) ([]byte, error) {
		if command.captureOutput {
			return []byte(codexAcceptanceVersion + "\n"), nil
		}
		isolatedCodexHome = commandEnv(command.env, "CODEX_HOME")
		return nil, errors.New(private)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runCodexInstalledClientAcceptance(ctx, codexAcceptanceDependencies{
		executable: "/synthetic/codex",
		runner:     runner,
	})
	if err == nil {
		t.Fatal("expected runner failure")
	}
	if strings.Contains(err.Error(), private) {
		t.Fatalf("runner error leaked: %v", err)
	}
	if isolatedCodexHome == "" {
		t.Fatal("runner did not receive an isolated CODEX_HOME")
	}
	if _, statErr := os.Stat(isolatedCodexHome); !os.IsNotExist(statErr) {
		t.Fatalf("failed acceptance isolation was not removed: %v", statErr)
	}
}

func TestCodexInstalledAcceptanceHonoursCallerDeadline(t *testing.T) {
	runner := testCodexAcceptanceRunner(func(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
		if command.captureOutput {
			return []byte(codexAcceptanceVersion + "\n"), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := runCodexInstalledClientAcceptance(ctx, codexAcceptanceDependencies{
		executable: "/synthetic/codex",
		runner:     runner,
	}); err == nil {
		t.Fatal("expected deadline failure")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline took %v", elapsed)
	}
}

func TestRunCodexHTTPInstalledAcceptance(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_INSTALLED_ACCEPTANCE") != "1" {
		t.Skip("installed Codex acceptance requires explicit opt-in")
	}
	if _, err := os.Stat(codexAcceptanceExecutable); err != nil {
		t.Skip("installed Codex acceptance executable unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := RunCodexHTTPInstalledAcceptance(ctx)
	if err != nil {
		t.Fatalf("%v: %#v", err, result)
	}
	if result.Turns != 20 || result.Requests != 40 || result.SelectorCalls != 20 || result.ContinuityErrors != 0 || result.UnknownEvents != 0 {
		t.Fatalf("enforced result = %#v", result)
	}
	if result.InstalledVersion != codexAcceptanceVersion || result.InstalledRequests != 1 || result.InstalledModelRequests < 1 || result.InstalledModelRequests > 2 || result.InstalledAttempts != 1 || result.InstalledSelectorCalls != 1 || result.InstalledResolutions != 1 || result.InstalledStrongKeys != 1 || result.InstalledZstdRequests != 1 || result.HeadroomRequests != 1 || result.HeadroomParseErrors != 0 || result.InstalledUnknownEvents != 0 || result.InstalledContinuityErrors != 0 || result.InstalledQuiescentLeases != 1 || result.UnexpectedRoutes != 0 || result.EgressAttempts != 0 || !result.PongVerified {
		t.Fatalf("installed result = %#v", result)
	}
}

func syntheticCodexAcceptanceRunner(options syntheticCodexClientOptions) testCodexAcceptanceRunner {
	return func(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
		if command.captureOutput {
			return []byte(codexAcceptanceVersion + "\n"), nil
		}
		return nil, runSyntheticCodexAcceptanceClient(ctx, command, options)
	}
}

func runSyntheticCodexAcceptanceClient(ctx context.Context, command codexAcceptanceCommand, options syntheticCodexClientOptions) error {
	client := &http.Client{Timeout: 2 * time.Second}
	modelsURL := strings.TrimSuffix(command.endpoint, "/responses") + "/models?client_version=0.146.0"
	modelRequests := options.modelRequests
	if modelRequests == 0 {
		modelRequests = 1
	}
	for range modelRequests {
		modelsRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
		if err != nil {
			return err
		}
		modelsResponse, err := client.Do(modelsRequest)
		if err != nil {
			return err
		}
		_, modelsCopyErr := io.Copy(io.Discard, modelsResponse.Body)
		modelsCloseErr := modelsResponse.Body.Close()
		if modelsCopyErr != nil {
			return modelsCopyErr
		}
		if modelsCloseErr != nil {
			return modelsCloseErr
		}
		if modelsResponse.StatusCode != http.StatusOK {
			return errors.New("synthetic Codex models request failed")
		}
	}
	if options.unexpectedRoute {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(command.endpoint, "/responses")+"/models", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if options.egress {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, command.egressProxyURL, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}

	requestCount := options.requests
	if requestCount == 0 {
		requestCount = 1
	}
	for range requestCount {
		body := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
		encoding := ""
		if options.zstd {
			body = encodeCodexAcceptanceZstd(body)
			encoding = "zstd"
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, command.endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		bootstrapAccount := "acceptance-bootstrap"
		if options.wrongBootstrap {
			bootstrapAccount = "wrong-bootstrap"
		}
		request.Header.Set("Authorization", "Bearer "+codexAcceptanceLocalToken)
		request.Header.Set("ChatGPT-Account-ID", bootstrapAccount)
		if options.bootstrapAPIKey {
			request.Header.Set("x-api-key", "acceptance-bootstrap-api-key")
		}
		request.Header.Set("Content-Type", "application/json")
		if encoding != "" {
			request.Header.Set("Content-Encoding", encoding)
		}
		if options.metadata {
			request.Header.Set(codexTurnMetadataKey, `{"session_id":"synthetic-session","thread_id":"synthetic-thread","turn_id":"synthetic-turn","request_kind":"turn"}`)
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if response.StatusCode != http.StatusOK {
			return errors.New("synthetic Codex request failed")
		}
	}
	return os.WriteFile(command.outputPath, []byte(options.output), 0o600)
}

func assertCodexAcceptanceCommandIsolation(t *testing.T, command codexAcceptanceCommand) {
	t.Helper()
	if command.executable != "/synthetic/codex" {
		t.Fatalf("executable = %q", command.executable)
	}
	if !command.loopbackOnly {
		t.Fatal("installed client command is not loopback-confined")
	}
	args := strings.Join(command.args, "\n")
	if strings.Contains(args, "--strict-config") {
		t.Fatal("acceptance command requires option unavailable in pinned Codex build")
	}
	for _, required := range []string{
		"exec",
		"--ephemeral",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		`name = "OpenAI"`,
		"request_max_retries = 0",
		"stream_max_retries = 0",
		"supports_websockets = false",
		"analytics.enabled=false",
		"features.enable_request_compression=true",
		"features.apps=false",
		"features.plugins=false",
		"features.respect_system_proxy=true",
		"check_for_update_on_startup=false",
		`cli_auth_credentials_store="file"`,
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("command args missing %q: %s", required, args)
		}
	}
	for _, forbidden := range []string{"chatgpt.com", "api.openai.com", "user-api-key-must-not-be-inherited", "user-access-token-must-not-be-inherited"} {
		if strings.Contains(args, forbidden) || strings.Contains(strings.Join(command.env, "\n"), forbidden) {
			t.Fatalf("command inherited forbidden value %q", forbidden)
		}
	}
	codexHome := commandEnv(command.env, "CODEX_HOME")
	if codexHome == "" || codexHome == os.Getenv("CODEX_HOME") {
		t.Fatalf("CODEX_HOME is not isolated: %q", codexHome)
	}
	for _, required := range []string{"HOME", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"} {
		if commandEnv(command.env, required) == "" {
			t.Fatalf("isolated environment missing %s", required)
		}
	}
	authPath := filepath.Join(codexHome, "auth.json")
	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth mode = %o", info.Mode().Perm())
	}
	auth, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(auth, []byte(codexAcceptanceLocalToken)) {
		t.Fatal("synthetic acceptance token missing from isolated auth")
	}
}

func commandEnv(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
