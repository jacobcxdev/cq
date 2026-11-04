package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// Diagnostic only: the installed CLI serialises a tool-free request to a local
// relay. Only the parent holds the own-account Start JWT. This is not the
// production harness or proof of account-mode entitlement preflight.
func TestZCodeInstalledStartSerialization(t *testing.T) {
	if os.Getenv("CQ_ZCODE_CLI_LIVE") != "1" || runtime.GOOS != "darwin" {
		t.Skip("explicit macOS installed ZCode serialization qualification")
	}
	login, device := zcodeLiveLogin(t)
	root, pathErr := filepath.EvalSymlinks(t.TempDir())
	if pathErr != nil {
		t.Fatal("resolve diagnostic sandbox directory")
	}
	home, work, data := filepath.Join(root, "home"), filepath.Join(root, "work"), filepath.Join(root, "data")
	for _, dir := range []string{home, work, filepath.Join(data, "v2")} {
		if os.MkdirAll(dir, 0o700) != nil {
			t.Fatal("prepare isolated CLI directories")
		}
	}
	deviceJSON, _ := json.Marshal(map[string]string{"deviceMid": device})
	if os.WriteFile(filepath.Join(data, "v2", "telemetry-state.json"), deviceJSON, 0o600) != nil {
		t.Fatal("prepare own device metadata")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	var attempts, forwarded, status, vendorCode atomic.Int64
	var hasTools atomic.Bool
	client := httputil.NewClient(30*time.Second, "zcode-serialization-qualification")
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if recover() != nil {
				http.Error(w, "diagnostic failed", 502)
			}
		}()
		if req.Method != "POST" || req.URL.Path != "/v1/messages" || attempts.Add(1) != 1 {
			http.Error(w, "diagnostic request limit", 429)
			return
		}
		body, err := httputil.ReadBodyLimit(req.Body, maxGatewayRequest)
		var request map[string]json.RawMessage
		if err != nil || json.Unmarshal(body, &request) != nil {
			http.Error(w, "invalid diagnostic request", 400)
			return
		}
		var tools []json.RawMessage
		if len(request["tools"]) != 0 && json.Unmarshal(request["tools"], &tools) != nil || len(tools) != 0 {
			hasTools.Store(true)
			http.Error(w, "diagnostic requires no tools", 400)
			return
		}
		// Bound qualification cost only; preserve every other CLI field.
		request["max_tokens"] = json.RawMessage("128")
		body, _ = json.Marshal(request)
		upstream, _ := http.NewRequestWithContext(ctx, "POST", startMessagesURL, bytes.NewReader(body))
		for _, key := range []string{"User-Agent", "X-Title", "HTTP-Referer", "X-ZCode-App-Version", "X-Platform", "X-Os-Category", "X-Os-Version", "X-Client-Language", "X-Client-Timezone", "X-Device-Mid", "X-Request-Id", "X-ZCode-Trace-Id", "X-ZCode-Session-Type", "X-Session-Id", "X-Query-Id", "X-ZCode-Query-Source", "Anthropic-Version"} {
			if value := req.Header.Get(key); value != "" {
				upstream.Header.Set(key, value)
			}
		}
		upstream.Header.Set("Authorization", "Bearer "+login.ZCodeJWT)
		upstream.Header.Set("X-Api-Key", login.ZCodeJWT)
		upstream.Header.Set("Content-Type", "application/json")
		forwarded.Add(1)
		response, err := client.Do(upstream)
		if err != nil || response == nil || response.Body == nil {
			http.Error(w, "diagnostic upstream unavailable", 502)
			return
		}
		defer response.Body.Close()
		status.Store(int64(response.StatusCode))
		if response.StatusCode != 200 {
			raw, _ := httputil.ReadBody(response.Body)
			var failure struct {
				Code  json.RawMessage `json:"code"`
				Error struct {
					Code json.RawMessage `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(raw, &failure)
			code := failure.Code
			if len(code) == 0 {
				code = failure.Error.Code
			}
			var value string
			if json.Unmarshal(code, &value) != nil {
				value = string(code)
			}
			if number, err := strconv.Atoi(value); err == nil {
				vendorCode.Store(int64(number))
			}
			http.Error(w, "diagnostic upstream rejected", response.StatusCode)
			return
		}
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		_, _ = io.Copy(w, io.LimitReader(response.Body, httputil.MaxResponseBody+1))
	}))
	defer relay.Close()
	provider := map[string]any{"schemaVersion": 1, "config": map[string]any{
		"providerConfigRules": map[string]any{"providerRules": []any{map[string]any{"providerId": "cq-loopback-baseline", "providerName": "CQ loopback baseline", "config": map[string]any{
			"group": "standard-personal", "access": map[string]string{"type": "api-key", "apiKey": "loopback-dummy"}, "api": map[string]string{"type": "anthropic-messages", "baseUrl": relay.URL + "/v1"}, "personalModelIds": []string{"GLM-5.3-Flash"},
		}}}}, "modelConfigRules": map[string]any{"providerModelRules": []any{}, "manualProviderModelRules": []any{}}, "defaultModelSelection": map[string]string{"providerId": "cq-loopback-baseline", "modelId": "GLM-5.3-Flash"},
	}}
	providerJSON, _ := json.Marshal(provider)
	providerPath := filepath.Join(root, "provider.json")
	if os.WriteFile(providerPath, providerJSON, 0o600) != nil {
		t.Fatal("write isolated provider")
	}
	runtimeConfig := `{"features":{"compact":false,"rewind":false,"mcp":false,"subagent":false,"memory":false,"skill":false},"memory":{"use":false},"plugins":{"enabled":false},"skills":{"enabled":false},"hooks":{"enabled":false},"ui":{"locale":"en-US"},"modelStream":{"idleTimeoutMs":30000}}`
	if os.WriteFile(filepath.Join(work, "zcode.json"), []byte(runtimeConfig), 0o600) != nil {
		t.Fatal("write isolated runtime options")
	}
	node := "/opt/homebrew/bin/node"
	if _, err := os.Stat(node); err != nil {
		t.Fatal("Node 24 or later required")
	}
	denyTools := "ToolSearch LocalToolSearch DeferredToolCall Read Write Edit ApplyPatch Bash Glob Grep WebFetch WebSearch web_search TodoRead TodoWrite CronCreate CronList CronUpdate CronDelete CronPauseSelf OffPeakCreate OffPeakList EnterPlanMode ExitPlanMode AskUserQuestion SendMessage RespondToCoordinator submit_result escalate TaskOutput TaskStop ReadSessionContext Agent Task Skill js CreateWorkflow AmendWorkflow SaveWorkflow EvalWorkflowSnippet ListWorkflowRuns GetWorkflowRun ResumeWorkflowRun ResolveWorkflowQuestion FillWorkflowHole ListSavedWorkflows ListModels Workflow ReplyToChannel GoalRead"
	profile := `(version 1)(allow default)(deny network-outbound)(allow network-outbound (remote ip "localhost:*"))(deny file-write*)(allow file-write* (subpath ` + strconv.Quote(root) + `))`
	if originalHome, err := os.UserHomeDir(); err == nil {
		profile += `(deny file-read* (subpath ` + strconv.Quote(originalHome) + `))`
	}
	args := []string{"-p", profile, node, "/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs", "--cwd", work, "--mode", "plan", "--toolsearch-mode", "off", "--workflow-mode", "disabled", "--disallowed-tools", denyTools, "--prompt", "Return exactly OK. Do not call tools.", "--output-format", "json", "--locale", "en-US", "--no-color"}
	command := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", args...)
	command.Dir = work
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + root, "ZCODE_APP_VERSION=" + ClientVersion, "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=" + providerPath, "ZCODE_BUILTIN_PROVIDER_CONFIG_FILE=/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json", "ZCODE_DATA_BASE_DIR=" + data, "ZCODE_STORAGE_DIR=" + filepath.Join(root, "storage"), "ZCODE_LOG_DIR=" + filepath.Join(root, "logs"), "ZCODE_NO_PROXY=127.0.0.1,localhost"}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	for _, hint := range []string{"Operation not permitted", "Permission denied", "Cannot find module", "Cannot find package", "invalid config", "Unknown option", "unknown option", "dyld", "sandbox", "No such file", "not found", "SyntaxError", "Fatal", "Trace/BPT trap"} {
		if bytes.Contains(output.Bytes(), []byte(hint)) {
			t.Logf("cli_diagnostic=%q", hint)
		}
	}
	t.Logf("cli_exit=%v output_bytes=%d", err, output.Len())
	t.Logf("cli_serialization attempts=%d forwarded=%d status=%d vendor_code=%d tools_present=%t exited_ok=%t", attempts.Load(), forwarded.Load(), status.Load(), vendorCode.Load(), hasTools.Load(), err == nil)
	if forwarded.Load() != 1 {
		t.Fatal("installed CLI did not produce one tool-free bounded request")
	}
	if status.Load() != 200 {
		t.Fatalf("genuine CLI serialization rejected: HTTP %d, no retry forwarded", status.Load())
	}
}
