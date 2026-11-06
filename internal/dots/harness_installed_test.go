package dots

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// This opt-in contract test runs the installed native app-server with an empty
// home, fixture capabilities and a synthetic loopback provider. It never reads
// account credentials, invokes a real model or changes the installed app.
func TestHarnessInstalledChatCapabilityIsolation(t *testing.T) {
	t.Run("native-controls", func(t *testing.T) { runHarnessInstalledChatCapabilityIsolation(t, false, false) })
	t.Run("bridge-policy", func(t *testing.T) { runHarnessInstalledChatCapabilityIsolation(t, true, false) })
	t.Run("selective-policy", func(t *testing.T) { runHarnessInstalledChatCapabilityIsolation(t, true, true) })
}

func runHarnessInstalledChatCapabilityIsolation(t *testing.T, bridgeEnabled, selective bool) {
	if os.Getenv("CQ_DOTS_HARNESS_INSTALLED_ACCEPTANCE") != "1" {
		t.Skip("set CQ_DOTS_HARNESS_INSTALLED_ACCEPTANCE=1 for isolated installed acceptance")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("installed harness acceptance requires the macOS loopback sandbox")
	}
	client := os.Getenv("CQ_DOTS_HARNESS_INSTALLED_CODEX")
	if !filepath.IsAbs(client) {
		t.Fatal("CQ_DOTS_HARNESS_INSTALLED_CODEX must name the absolute native Codex executable")
	}
	clientBytes, err := os.ReadFile(client)
	if err != nil {
		t.Fatal("read installed native executable")
	}
	if len(clientBytes) < 4 || string(clientBytes[:2]) == "#!" {
		t.Fatal("installed acceptance requires the native executable rather than its shell wrapper")
	}
	identity := sha256.Sum256(clientBytes)
	clientBytes = nil
	t.Cleanup(func() {
		after, err := os.ReadFile(client)
		if err != nil || sha256.Sum256(after) != identity {
			t.Error("installed executable changed during harness acceptance")
		}
	})
	root, err := os.MkdirTemp("/private/tmp", "cq-dots-harness-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, name := range []string{"home", "codex", "work", "minimal-work", "dependency-work", "tmp", "cache", "config", "data"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, contents string) {
		t.Helper()
		name = filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("codex/skills/probe-skill/SKILL.md", "---\nname: probe-skill\ndescription: CQ_HARNESS_SKILL_MARKER\n---\nSynthetic standalone skill.\n")
	write("market/plugins/probe-plugin/skills/plugin-skill/SKILL.md", "---\nname: plugin-skill\ndescription: CQ_HARNESS_PLUGIN_SKILL_MARKER\n---\nSynthetic plugin skill.\n")
	mcp := map[string]any{"command": os.Args[0], "args": []string{"-test.run=^TestHarnessInstalledMCPHelper$"}, "env": map[string]string{"CQ_HARNESS_INSTALLED_MCP_HELPER": "1"}}
	manifest, _ := json.Marshal(map[string]any{"name": "probe-plugin", "version": "1.0.0", "description": "Synthetic capability plugin", "skills": "./skills/", "mcpServers": map[string]any{"pluginprobe": mcp}})
	write("market/plugins/probe-plugin/.codex-plugin/plugin.json", string(manifest))
	write("market/.claude-plugin/marketplace.json", `{"name":"cq-harness-market","owner":{"name":"CQ"},"plugins":[{"name":"probe-plugin","source":"./plugins/probe-plugin","description":"Synthetic plugin"}]}`)
	if selective {
		write("codex/skills/design-skill/SKILL.md", "---\nname: design-skill\ndescription: CQ_HARNESS_DESIGN_SKILL_MARKER\n---\nSynthetic design skill.\n")
		write("codex/skills/bundle-dependent/SKILL.md", "---\nname: bundle-dependent\ndescription: CQ_HARNESS_DEPENDENCY_SKILL_MARKER\n---\nSynthetic skill requiring a plugin MCP.\n")
		write("codex/skills/bundle-dependent/agents/openai.yaml", "dependencies:\n  tools:\n    - type: \"mcp\"\n      value: \"pluginprobe\"\n      description: \"Synthetic plugin MCP\"\n")
		write("codex/skills/curate-harness/SKILL.md", "---\nname: curate-harness\ndescription: CQ_HARNESS_CURATION_SKILL_MARKER\n---\nSynthetic curation entrypoint.\n")
		write("market/plugins/design-plugin/skills/design-plugin-skill/SKILL.md", "---\nname: design-plugin-skill\ndescription: CQ_HARNESS_DESIGN_PLUGIN_SKILL_MARKER\n---\nSynthetic design plugin skill.\n")
		manifest, _ := json.Marshal(map[string]any{"name": "design-plugin", "version": "1.0.0", "description": "Synthetic design plugin", "skills": "./skills/", "mcpServers": map[string]any{"designplugin": mcp}})
		write("market/plugins/design-plugin/.codex-plugin/plugin.json", string(manifest))
		write("market/.claude-plugin/marketplace.json", `{"name":"cq-harness-market","owner":{"name":"CQ"},"plugins":[{"name":"probe-plugin","source":"./plugins/probe-plugin","description":"Synthetic research plugin"},{"name":"design-plugin","source":"./plugins/design-plugin","description":"Synthetic design plugin"}]}`)
	}
	observed := make(chan harnessInstalledObservation, 32)
	var spawnIssued atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Error("unexpected synthetic provider route")
			http.Error(w, "unexpected route", http.StatusBadRequest)
			return
		}
		body, err := httputil.ReadBody(r.Body)
		if err != nil {
			t.Error("read synthetic request")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var request struct {
			Model string                        `json:"model"`
			Input []json.RawMessage             `json:"input"`
			Tools []struct{ Name, Type string } `json:"tools"`
		}
		if json.Unmarshal(body, &request) != nil {
			t.Error("decode synthetic request")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		observation := harnessInstalledObservation{Model: request.Model, InputItems: len(request.Input), RequestBytes: len(body)}
		observation.Identity = map[string]string{}
		for _, header := range []string{"Session_id", "X-Session-Id", "X-Client-Request-Id"} {
			if value := r.Header.Get(header); value != "" {
				observation.Identity["header."+header] = value
			}
		}
		var turnMetadata map[string]any
		if json.Unmarshal([]byte(r.Header.Get("X-Codex-Turn-Metadata")), &turnMetadata) == nil {
			harnessInstalledIdentityFields("header.X-Codex-Turn-Metadata", turnMetadata, observation.Identity)
		}
		var bodyFields map[string]any
		if json.Unmarshal(body, &bodyFields) == nil {
			harnessInstalledIdentityFields("body", bodyFields, observation.Identity)
			if reasoning, ok := bodyFields["reasoning"].(map[string]any); ok {
				observation.ReasoningEffort, _ = reasoning["effort"].(string)
			}
		}
		for _, tool := range request.Tools {
			name := tool.Name
			if name == "" {
				name = tool.Type
			}
			observation.Tools = append(observation.Tools, name)
		}
		for _, item := range request.Input {
			text := string(item)
			observation.SkillCatalogueItems += strings.Count(text, "CQ_HARNESS_SKILL_MARKER")
			observation.PluginCatalogueItems += strings.Count(text, "CQ_HARNESS_PLUGIN_SKILL_MARKER")
			observation.FirstUser = observation.FirstUser || strings.Contains(text, "CQ_HARNESS_FIRST_USER")
			observation.FirstReply = observation.FirstReply || strings.Contains(text, "CQ_HARNESS_REPLY")
			if observation.Markers == nil {
				observation.Markers = make(map[string]int)
			}
			for name, marker := range map[string]string{"research": "CQ_HARNESS_SKILL_MARKER", "research-plugin": "CQ_HARNESS_PLUGIN_SKILL_MARKER", "design": "CQ_HARNESS_DESIGN_SKILL_MARKER", "design-plugin": "CQ_HARNESS_DESIGN_PLUGIN_SKILL_MARKER", "curation": "CQ_HARNESS_CURATION_SKILL_MARKER", "developer": "CQ_HARNESS_DEVELOPER_MARKER", "dependency": "CQ_HARNESS_DEPENDENCY_SKILL_MARKER"} {
				observation.Markers[name] += strings.Count(text, marker)
			}
		}
		observed <- observation
		item := map[string]any{"id": "probe_message", "type": "message", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": "CQ_HARNESS_REPLY", "annotations": []any{}}}}
		if strings.Contains(string(body), "CQ_HARNESS_SPAWN_PARENT") && spawnIssued.CompareAndSwap(false, true) {
			arguments, _ := json.Marshal(map[string]any{"message": "CQ_HARNESS_CHILD_IDENTITY", "fork_context": false})
			item = map[string]any{"id": "probe_spawn", "type": "function_call", "namespace": "multi_agent_v1", "name": "spawn_agent", "arguments": string(arguments), "call_id": "probe_spawn_call", "status": "completed"}
		}
		response := map[string]any{"id": "probe_response", "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 32, "output_tokens": 1, "total_tokens": 33}}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []any{
			map[string]any{"type": "response.created", "response": map[string]any{"id": "probe_response", "object": "response", "status": "in_progress", "output": []any{}}},
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
			map[string]any{"type": "response.completed", "response": response},
		} {
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer upstream.Close()
	configText := fmt.Sprintf(`model = "glm-5.3"
model_provider = "cq_harness"
approval_policy = "never"
sandbox_mode = "read-only"
cli_auth_credentials_store = "file"
check_for_update_on_startup = false
[analytics]
enabled = false
[features]
plugins = true
enable_request_compression = false
[model_providers.cq_harness]
name = "Synthetic harness fixture"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
[mcp_servers.probe]
command = %q
args = ["-test.run=^TestHarnessInstalledMCPHelper$"]
startup_timeout_sec = 5
[mcp_servers.probe.env]
CQ_HARNESS_INSTALLED_MCP_HELPER = "1"
`, upstream.URL, os.Args[0])
	if selective {
		configText += fmt.Sprintf(`
[mcp_servers.design]
command = %q
args = ["-test.run=^TestHarnessInstalledMCPHelper$"]
startup_timeout_sec = 5
[mcp_servers.design.env]
CQ_HARNESS_INSTALLED_MCP_HELPER = "1"
`, os.Args[0])
	}
	write("codex/config.toml", configText)
	profile := `(version 1) (allow default) (deny network*) (deny file-write*) (allow network-outbound (remote ip "localhost:*")) (allow file-write* (literal "/dev/null")) (allow file-write* (subpath ` + fmt.Sprintf("%q", root) + `))`
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, client, "app-server", "--stdio", "--strict-config")
	command.Dir = filepath.Join(root, "work")
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin", "LANG=C", "LC_ALL=C", "TERM=dumb", "NO_COLOR=1", "CI=1", "SHELL=/bin/zsh", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "NO_PROXY=127.0.0.1,localhost"}
	for name, directory := range map[string]string{"HOME": "home", "CODEX_HOME": "codex", "TMPDIR": "tmp", "XDG_CACHE_HOME": "cache", "XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data"} {
		command.Env = append(command.Env, name+"="+filepath.Join(root, directory))
	}
	policyPath := filepath.Join(root, "policy", "harness.json")
	var fullInventory func(context.Context, string) (harness.Inventory, error)
	if selective {
		// Reuse NativeInventory's production protocol reader with an executable
		// wrapper that removes the test runner's environment and enforces the
		// same fixture HOME and egress sandbox as the native chat process.
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		script := "#!/bin/sh\nexec /usr/bin/env -i"
		for _, value := range command.Env {
			script += " " + quote(value)
		}
		script += " /usr/bin/sandbox-exec -p " + quote(profile) + " " + quote(client) + " \"$@\"\n"
		write("native-inventory", script)
		inventoryPath := filepath.Join(root, "native-inventory")
		if err := os.Chmod(inventoryPath, 0o700); err != nil {
			t.Fatal(err)
		}
		fullInventory = func(ctx context.Context, cwd string) (harness.Inventory, error) {
			return (harness.NativeInventory{Executable: inventoryPath}).Snapshot(ctx, cwd)
		}
	}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal("start installed native harness")
	}
	finish := func() {
		_ = input.Close()
		if err := command.Wait(); err != nil {
			t.Errorf("installed native harness exited unsuccessfully: %v", err)
		}
	}
	policy := harness.Policy{Version: 1, Enabled: true, Projects: map[string]harness.Profile{command.Dir: harness.Full}}
	var policyMu, nativeMu sync.Mutex
	var nativeRequests []cloudMessage
	receipts := harness.Receipts{FS: fsutil.OSFileSystem{}, Dir: filepath.Join(root, "receipts")}
	if bridgeEnabled {
		nativeInput, nativeOutput := input, output
		bridgeInput, clientInput := io.Pipe()
		clientOutput, bridgeOutput := io.Pipe()
		finished := make(chan error, 1)
		deps := cloudStdioDependencies{
			environment: command.Env, now: time.Now, statusInterval: 25 * time.Millisecond,
			harnessTimeout: 5 * time.Second, harnessReceipts: receipts,
			readRoots: func(string, string) ([]byte, error) { return nil, os.ErrNotExist },
			event: func(value map[string]any) {
				if value["harness_profile"] != nil {
					t.Logf("bridge-policy-event: profile=%v source=%v state=%v", value["harness_profile"], value["source"], value["state"])
				}
			},
			readHarnessPolicy: func() (harness.Policy, error) {
				policyMu.Lock()
				defer policyMu.Unlock()
				return policy, nil
			},
			launch: func(context.Context, string, []string, []string, io.Writer) (*cloudLocal, error) {
				return &cloudLocal{
					input: cloudTestWriter{write: func(raw []byte) (int, error) {
						var request cloudMessage
						if json.Unmarshal(raw, &request) != nil {
							return 0, errors.New("invalid native fixture request")
						}
						if request.method() == "thread/resume" || request.method() == "thread/unsubscribe" || request.method() == "thread/settings/update" {
							params := cloudHarnessObject(request["params"])
							t.Logf("bridge-native-control: method=%s chat=%s model=%s config=%s", request.method(), cloudThreadID(request["params"]), cloudHarnessModel(params), params["config"])
						}
						nativeMu.Lock()
						nativeRequests = append(nativeRequests, request)
						nativeMu.Unlock()
						return nativeInput.Write(raw)
					}},
					output: nativeOutput, wait: command.Wait,
					stop: func() { _ = nativeInput.Close(); _ = command.Process.Kill(); _ = nativeOutput.Close() },
				}, nil
			},
			stock: func(context.Context, string, []string, []string, io.Reader, io.Writer, io.Writer) error {
				return errors.New("installed harness fixture unexpectedly bypassed the bridge")
			},
			dial: func(context.Context, Account) (cloudSocket, int, error) {
				t.Error("installed harness fixture attempted external cloud socket access")
				return nil, 0, errors.New("external cloud sockets forbidden")
			},
			http: cloudTestHTTP(func(*http.Request) (*http.Response, error) {
				t.Error("installed harness fixture attempted external cloud HTTP access")
				return nil, errors.New("external cloud HTTP forbidden")
			}),
		}
		if selective {
			deps.readHarnessContextPolicy = func(cwd string) (harness.Policy, error) {
				return harness.LoadForContext(fsutil.OSFileSystem{}, policyPath, cwd)
			}
			deps.nativeHarnessInventory = fullInventory
		}
		go func() {
			defer func() {
				if recover() != nil {
					finished <- errors.New("installed harness fixture panicked")
				}
				_ = bridgeOutput.Close()
			}()
			finished <- runCloudStdio(ctx, &Settings{AppPath: "/synthetic/fixture.app", RuntimeRoot: root}, nil,
				[]string{"app-server", "-c", cloudAppMarker + "true"}, bridgeInput, bridgeOutput, io.Discard, deps)
		}()
		input, output = clientInput, clientOutput
		finish = func() {
			_ = clientInput.Close()
			select {
			case err := <-finished:
				if err != nil {
					t.Errorf("installed harness bridge failed: %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Error("installed harness bridge did not stop")
			}
			_ = bridgeInput.Close()
			_ = clientOutput.Close()
		}
	}
	defer func() { finish() }()
	rpc := harnessInstalledRPC{t: t, encoder: json.NewEncoder(input), decoder: json.NewDecoder(output)}
	rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cq-harness-installed-acceptance", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
	_ = rpc.encoder.Encode(map[string]string{"method": "initialized"})
	rpc.call("marketplace/add", map[string]string{"source": filepath.Join(root, "market")})
	rpc.call("plugin/install", map[string]string{"marketplacePath": filepath.Join(root, "market/.claude-plugin/marketplace.json"), "pluginName": "probe-plugin"})
	if selective {
		rpc.call("plugin/install", map[string]string{"marketplacePath": filepath.Join(root, "market/.claude-plugin/marketplace.json"), "pluginName": "design-plugin"})
	}
	if !bridgeEnabled {
		native := cloudHarnessObject(rpc.call("config/read", map[string]any{"cwd": command.Dir, "includeLayers": true}))
		logBaseline := func(label string, raw json.RawMessage) {
			config := cloudHarnessObject(raw)
			servers := cloudHarnessObject(config["mcp_servers"])
			probe := cloudHarnessObject(servers["probe"])
			t.Logf("native-baseline %s: skills.include_instructions=%s features.plugins=%s mcp.probe.present=%t mcp.probe.enabled=%s", label,
				cloudHarnessObject(config["skills"])["include_instructions"], cloudHarnessObject(config["features"])["plugins"], servers["probe"] != nil, probe["enabled"])
		}
		logBaseline("merged", native["config"])
		origins := cloudHarnessObject(native["origins"])
		for _, key := range []string{"skills.include_instructions", "features.plugins", "mcp_servers.probe.enabled", "skills", "features", "mcp_servers"} {
			t.Logf("native-baseline-origin %s=%s", key, origins[key])
		}
		var layers []cloudMessage
		_ = json.Unmarshal(native["layers"], &layers)
		for index, layer := range layers {
			t.Logf("native-baseline-layer-%d: name=%s version=%s", index, layer["name"], layer["version"])
			logBaseline(fmt.Sprintf("layer-%d", index), layer["config"])
		}
	}
	start := func(config map[string]any) string {
		t.Helper()
		result := rpc.call("thread/start", map[string]any{"cwd": command.Dir, "model": "glm-5.3", "modelProvider": "cq_harness", "config": config})
		var started struct {
			Thread struct{ ID, SessionID string }
		}
		if json.Unmarshal(result, &started) != nil || started.Thread.ID == "" {
			t.Fatal("decode installed thread ID")
		}
		t.Logf("native-start-identity: chat=%s tree-session=%s", started.Thread.ID, started.Thread.SessionID)
		return started.Thread.ID
	}
	turn := func(label, threadID, text string, extra map[string]any) harnessInstalledObservation {
		t.Helper()
		params := map[string]any{"threadId": threadID, "input": []map[string]string{{"type": "text", "text": text}}}
		for key, value := range extra {
			params[key] = value
		}
		rpc.call("turn/start", params)
		for {
			message := rpc.next()
			if message.Method == "turn/completed" && cloudThreadID(message.Params) == threadID {
				break
			}
		}
		select {
		case observation := <-observed:
			if observation.Identity["body.client_metadata.thread_id"] != threadID {
				t.Fatal("native request metadata identified a different chat")
			}
			t.Logf("%s: model=%s tools=%d standalone-schema=%t plugin-schema=%t mcp-resources=%t tool-search=%t skill-items=%d plugin-items=%d first-user=%t first-reply=%t input-items=%d bytes=%d", label, observation.Model, len(observation.Tools), slices.Contains(observation.Tools, "mcp__probe"), slices.Contains(observation.Tools, "mcp__pluginprobe"), slices.Contains(observation.Tools, "list_mcp_resources"), slices.Contains(observation.Tools, "tool_search"), observation.SkillCatalogueItems, observation.PluginCatalogueItems, observation.FirstUser, observation.FirstReply, observation.InputItems, observation.RequestBytes)
			identity, _ := json.Marshal(observation.Identity)
			t.Logf("%s identity: chat=%s fields=%s", label, threadID, identity)
			return observation
		default:
			t.Fatal("installed turn produced no synthetic provider request")
			return harnessInstalledObservation{}
		}
	}
	assertTools := func(observation harnessInstalledObservation, standalone, plugin bool) {
		t.Helper()
		if slices.Contains(observation.Tools, "list_mcp_resources") != (standalone || plugin) {
			t.Fatal("native MCP resource exposure did not match this chat's configuration")
		}
		// Native GPT models defer namespace schemas through tool_search. Their
		// resource tools still distinguish a full MCP assembly from minimal.
		if !strings.HasPrefix(observation.Model, "glm-") && slices.Contains(observation.Tools, "tool_search") {
			return
		}
		if slices.Contains(observation.Tools, "mcp__probe") != standalone || slices.Contains(observation.Tools, "mcp__pluginprobe") != plugin {
			t.Fatal("native capability set did not match this chat's configuration")
		}
	}
	if selective {
		const researchPlugin = "probe-plugin@cq-harness-market"
		const designPlugin = "design-plugin@cq-harness-market"
		const researchSkill = "user:probe-skill"
		const designSkill = "user:design-skill"
		const researchOwnedSkill = "plugin:" + researchPlugin + ":probe-plugin:plugin-skill"
		const designOwnedSkill = "plugin:" + designPlugin + ":design-plugin:design-plugin-skill"
		fsys := fsutil.OSFileSystem{}
		userPolicy := harness.Policy{Version: 2, Enabled: true,
			Profiles: map[string]harness.Definition{"research": {Changes: harness.Changes{
				Plugins: map[string]harness.Decision{researchPlugin: harness.Enabled}, MCP: map[string]harness.Decision{"probe": harness.Enabled}, Skills: map[string]harness.Decision{researchSkill: harness.Enabled, researchOwnedSkill: harness.Enabled},
			}}},
			Global: harness.Scope{
				All:    harness.Binding{Changes: harness.Changes{Plugins: map[string]harness.Decision{designPlugin: harness.Disabled}, MCP: map[string]harness.Decision{"design": harness.Disabled}, Skills: map[string]harness.Decision{designSkill: harness.Disabled, designOwnedSkill: harness.Disabled, "user:curate-harness": harness.Enabled}}},
				Models: map[string]harness.Binding{"glm-*": {Changes: harness.Changes{Plugins: map[string]harness.Decision{researchPlugin: harness.Disabled}, MCP: map[string]harness.Decision{"probe": harness.Disabled, "design": harness.Disabled}, Skills: map[string]harness.Decision{researchSkill: harness.Disabled, researchOwnedSkill: harness.Disabled}}}},
			},
		}
		if err := harness.Save(fsys, policyPath, userPolicy); err != nil {
			t.Fatal(err)
		}
		workspacePath := filepath.Join(command.Dir, ".cq", "harness.json")
		if _, err := harness.UpdateWorkspace(fsys, workspacePath, func(policy *harness.WorkspacePolicy) error {
			policy.Version = 2
			policy.Rules.All.Profiles = []string{"research"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var inventoryBaseline []string
		checkInventory := func(label string) {
			t.Helper()
			inventory, err := fullInventory(ctx, command.Dir)
			if err != nil {
				t.Fatalf("isolated full native inventory: %v", err)
			}
			want := map[string]bool{"plugin:" + researchPlugin: false, "plugin:" + designPlugin: false, "mcp:probe": false, "mcp:design": false, "skill:" + researchSkill: false, "skill:" + designSkill: false, "skill:" + researchOwnedSkill: false, "skill:" + designOwnedSkill: false, "skill:user:curate-harness": false}
			var current []string
			for _, capability := range inventory.Capabilities {
				key := string(capability.Kind) + ":" + capability.ID
				current = append(current, fmt.Sprintf("%s enabled=%t available=%t", key, capability.Enabled, capability.Available))
				t.Logf("selective full inventory row: key=%s owner=%s native=%s enabled=%t available=%t", key, capability.PluginID, capability.NativeID, capability.Enabled, capability.Available)
				if _, ok := want[key]; ok {
					if !capability.Enabled || !capability.Available {
						t.Fatalf("full inventory lost enabled fixture %s", key)
					}
					want[key] = true
				}
				if capability.Kind == harness.PluginKind && (capability.ID == researchPlugin || capability.ID == designPlugin) {
					raw, _ := json.Marshal(capability)
					var metadata struct {
						MCPServers []string `json:"mcp_servers"`
					}
					_ = json.Unmarshal(raw, &metadata)
					server := "pluginprobe"
					if capability.ID == designPlugin {
						server = "designplugin"
					}
					if !slices.Equal(metadata.MCPServers, []string{server}) {
						t.Fatalf("native plugin bundle ownership missing for %s", capability.ID)
					}
				}
			}
			for key, found := range want {
				if !found {
					t.Fatalf("full native inventory omitted fixture %s", key)
				}
			}
			slices.Sort(current)
			if inventoryBaseline == nil {
				inventoryBaseline = current
			} else if !slices.Equal(inventoryBaseline, current) {
				t.Fatal("chat filtering changed the complete native inventory")
			}
			t.Logf("selective inventory %s: fixture-capabilities=%d full-rows=%d issues=%v", label, len(want), len(inventory.Capabilities), inventory.Issues)
		}
		checkInventory("before-curation")
		startSelective := func(model, cwd string) string {
			t.Helper()
			result := rpc.call("thread/start", map[string]any{"cwd": cwd, "model": model, "modelProvider": "cq_harness", "developerInstructions": "CQ_HARNESS_DEVELOPER_MARKER", "config": map[string]any{"model_reasoning_effort": "low"}})
			var started struct{ Thread struct{ ID string } }
			if json.Unmarshal(result, &started) != nil || started.Thread.ID == "" {
				t.Fatal("decode selective native chat ID")
			}
			return started.Thread.ID
		}
		assertSelective := func(label string, observation harnessInstalledObservation, probe, design, pluginProbe, pluginDesign bool) {
			t.Helper()
			if observation.Markers["developer"] == 0 || observation.ReasoningEffort != "low" {
				t.Fatalf("selective reload lost original instructions/reasoning: marker=%d effort=%q", observation.Markers["developer"], observation.ReasoningEffort)
			}
			if slices.Contains(observation.Tools, "list_mcp_resources") != (probe || design || pluginProbe || pluginDesign) {
				t.Fatal("selective native MCP resources did not match the chosen subset")
			}
			if strings.HasPrefix(observation.Model, "glm-") {
				for namespace, wanted := range map[string]bool{"mcp__probe": probe, "mcp__design": design, "mcp__pluginprobe": pluginProbe, "mcp__designplugin": pluginDesign} {
					if slices.Contains(observation.Tools, namespace) != wanted {
						t.Fatalf("selective namespace %s did not match wanted=%t", namespace, wanted)
					}
				}
			}
			t.Logf("selective %s: tools=%v catalogue-markers=%v effort=%s", label, observation.Tools, observation.Markers, observation.ReasoningEffort)
		}
		assertFreshSkills := func(observation harnessInstalledObservation, research, design, pluginResearch, pluginDesign bool) {
			t.Helper()
			for name, wanted := range map[string]bool{"research": research, "design": design, "research-plugin": pluginResearch, "design-plugin": pluginDesign, "curation": true} {
				if (observation.Markers[name] > 0) != wanted {
					t.Fatalf("first-turn skill %s did not match wanted=%t: %v", name, wanted, observation.Markers)
				}
			}
		}
		outside := startSelective("gpt-5.5", filepath.Join(root, "minimal-work"))
		outsideFirst := turn("selective-global-all-model-design-denials", outside, "Synthetic global design denials", nil)
		assertSelective("global-gpt", outsideFirst, true, false, true, false)
		assertFreshSkills(outsideFirst, true, false, true, false)
		outsideGLM := startSelective("glm-5.3", filepath.Join(root, "minimal-work"))
		outsideRestricted := turn("selective-global-glm-restriction", outsideGLM, "Synthetic global model restriction", nil)
		assertSelective("global-glm", outsideRestricted, false, false, false, false)
		assertFreshSkills(outsideRestricted, false, false, false, false)
		chat := startSelective("glm-5.3", command.Dir)
		first := turn("selective-project-added-research", chat, "CQ_HARNESS_FIRST_USER", nil)
		assertSelective("project-research", first, true, false, true, false)
		assertFreshSkills(first, true, false, true, false)
		sibling := startSelective("glm-5.3", command.Dir)
		siblingFirst := turn("selective-project-sibling", sibling, "Synthetic selective sibling", nil)
		assertSelective("project-sibling", siblingFirst, true, false, true, false)
		updateTask := func(scope harness.Scope) {
			t.Helper()
			if _, err := harness.Update(fsys, policyPath, func(policy *harness.Policy) error {
				policy.Tasks = map[string]harness.Scope{chat: scope}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		task := harness.Scope{All: harness.Binding{Changes: harness.Changes{MCP: map[string]harness.Decision{"design": harness.Enabled}}}}
		updateTask(task)
		allOverride := turn("selective-task-all-beats-global-glm", chat, "Synthetic narrower all-model override", nil)
		assertSelective("task-all", allOverride, true, true, true, false)
		if !allOverride.FirstUser || !allOverride.FirstReply {
			t.Fatal("selective task reload lost original history")
		}
		task.Models = map[string]harness.Binding{"glm-*": {Changes: harness.Changes{MCP: map[string]harness.Decision{"probe": harness.Disabled}, Skills: map[string]harness.Decision{researchSkill: harness.Disabled}}}}
		updateTask(task)
		modelDelta := turn("selective-task-model-delta", chat, "Synthetic chat model delta", nil)
		assertSelective("task-glm-delta", modelDelta, false, true, true, false)
		if modelDelta.Markers["research"] != allOverride.Markers["research"] || modelDelta.Markers["research-plugin"] <= allOverride.Markers["research-plugin"] {
			t.Fatal("native standalone skill suppression did not keep the selected plugin-owned skill")
		}
		deltaReceipt, err := receipts.Load(chat)
		if err != nil || deltaReceipt.Fingerprint == "" || deltaReceipt.State != "applied" || len(deltaReceipt.Issues) != 0 || len(deltaReceipt.Unsupported) != 0 {
			t.Fatal("verified selective plugin bundles did not produce a complete application receipt")
		}
		gpt := turn("selective-model-change-restored-subset", chat, "Synthetic selective model change", map[string]any{"model": "gpt-5.5"})
		assertSelective("task-gpt", gpt, true, true, true, false)
		gptReceipt, err := receipts.Load(chat)
		if err != nil || gptReceipt.Fingerprint == deltaReceipt.Fingerprint || gptReceipt.Model != "gpt-5.5" || gpt.Markers["research"] <= modelDelta.Markers["research"] {
			t.Fatal("model change did not restore the subset and change its fingerprint")
		}
		back := turn("selective-model-return-restored-delta", chat, "Synthetic selective model return", map[string]any{"model": "glm-5.3"})
		assertSelective("task-glm-return", back, false, true, true, false)
		backReceipt, err := receipts.Load(chat)
		if err != nil || backReceipt.Fingerprint != deltaReceipt.Fingerprint || backReceipt.Model != "glm-5.3" {
			t.Fatal("model return did not restore the original selective fingerprint")
		}
		if back.Markers["research"] != gpt.Markers["research"] {
			t.Fatal("returning to GLM reattached a denied standalone skill")
		}
		siblingAfter := turn("selective-sibling-isolation", sibling, "Synthetic sibling after task override", nil)
		assertSelective("sibling-unchanged", siblingAfter, true, false, true, false)
		if siblingAfter.Markers["research"] != siblingFirst.Markers["research"] || siblingAfter.Markers["design"] != 0 {
			t.Fatal("chat override changed its sibling's skill catalogue")
		}
		nativeMu.Lock()
		beforeWorkspace := slices.Clone(nativeRequests)
		nativeMu.Unlock()
		for _, request := range beforeWorkspace {
			if (request.method() == "thread/resume" || request.method() == "thread/unsubscribe") && cloudThreadID(request["params"]) != chat {
				t.Fatal("task-only changes reloaded a sibling")
			}
		}
		if _, err := harness.UpdateWorkspace(fsys, workspacePath, func(policy *harness.WorkspacePolicy) error {
			policy.Rules.All.Changes.Skills = map[string]harness.Decision{designSkill: harness.Enabled}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		fileChanged := turn("selective-workspace-edit-before-next-turn", chat, "Synthetic workspace file change", nil)
		assertSelective("workspace-edit", fileChanged, false, true, true, false)
		fileReceipt, err := receipts.Load(chat)
		if err != nil || fileReceipt.Fingerprint == backReceipt.Fingerprint {
			t.Fatal("workspace edit did not change the applied fingerprint")
		}
		if fileChanged.Markers["design"] <= back.Markers["design"] || fileChanged.Markers["research"] != back.Markers["research"] || !fileChanged.FirstUser || !fileChanged.FirstReply {
			t.Fatal("workspace edit was not applied at the idle boundary with history intact")
		}
		workspaceSibling := turn("selective-workspace-edit-applied-to-matching-sibling", sibling, "Synthetic shared workspace update", nil)
		assertSelective("workspace-sibling", workspaceSibling, true, false, true, false)
		if workspaceSibling.Markers["design"] <= siblingAfter.Markers["design"] {
			t.Fatal("workspace policy did not reach its other matching chat")
		}
		updateTask(harness.Scope{All: harness.Binding{Profiles: []string{"full"}}})
		restored := turn("selective-full-restored-original-inventory", chat, "Synthetic full baseline restore", nil)
		assertSelective("restored-full", restored, true, true, true, true)
		for _, marker := range []string{"research", "design", "research-plugin", "design-plugin", "curation"} {
			if restored.Markers[marker] <= fileChanged.Markers[marker] {
				t.Fatalf("full restore omitted baseline skill %s", marker)
			}
		}
		checkInventory("after-filtered-chats")
		nativeMu.Lock()
		requests := slices.Clone(nativeRequests)
		nativeMu.Unlock()
		resumes, workspaceSiblingResumes := 0, 0
		for _, request := range requests {
			if request.method() == "thread/resume" || request.method() == "thread/unsubscribe" {
				id := cloudThreadID(request["params"])
				if id != chat && id != sibling {
					t.Fatal("workspace policy reloaded a chat outside its directory")
				}
				if request.method() == "thread/resume" {
					if id == chat {
						resumes++
					} else {
						workspaceSiblingResumes++
					}
				}
			}
		}
		if resumes < 6 {
			t.Fatalf("selective fingerprint changes did not reload the same chat: %d", resumes)
		}
		if _, err := harness.Update(fsys, policyPath, func(policy *harness.Policy) error {
			policy.Workspaces = map[string]harness.Scope{filepath.Join(root, "dependency-work"): {All: harness.Binding{Changes: harness.Changes{Reset: true, Default: harness.Disabled, Skills: map[string]harness.Decision{"user:bundle-dependent": harness.Enabled}}}}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		dependentChat := startSelective("glm-5.3", filepath.Join(root, "dependency-work"))
		dependent := turn("selective-skill-enabled-owning-plugin", dependentChat, "Synthetic bundled MCP dependency", nil)
		assertSelective("dependency-owner", dependent, false, false, true, false)
		if dependent.Markers["dependency"] == 0 {
			t.Fatal("selected dependent skill was absent from native catalogue")
		}
		assertFreshSkills(dependent, false, false, false, false)
		dependencyReceipt, err := receipts.Load(dependentChat)
		if err != nil || dependencyReceipt.State != "applied" || len(dependencyReceipt.Issues) != 0 || len(dependencyReceipt.Unsupported) != 0 {
			t.Fatal("native dependency closure was not completely applied")
		}
		if _, err := harness.Update(fsys, policyPath, func(policy *harness.Policy) error {
			policy.Tasks[outsideGLM] = harness.Scope{All: harness.Binding{Changes: harness.Changes{Reset: true, Default: harness.Disabled, Skills: map[string]harness.Decision{"user:bundle-dependent": harness.Enabled}}}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		lateDependency := turn("selective-skill-reenabled-excluded-plugin", outsideGLM, "Synthetic late bundled MCP dependency", nil)
		assertSelective("late-dependency-owner", lateDependency, false, false, true, false)
		if lateDependency.Markers["dependency"] <= outsideRestricted.Markers["dependency"] {
			t.Fatal("late dependency selection did not attach the selected skill")
		}
		lateReceipt, err := receipts.Load(outsideGLM)
		if err != nil || lateReceipt.State != "applied" || len(lateReceipt.Issues) != 0 || len(lateReceipt.Unsupported) != 0 {
			t.Fatal("late native dependency closure was not completely applied")
		}
		emptyChat := startSelective("glm-5.3", filepath.Join(root, "minimal-work"))
		if _, err := harness.Update(fsys, policyPath, func(policy *harness.Policy) error {
			policy.Tasks[emptyChat] = harness.Scope{All: harness.Binding{Changes: harness.Changes{Reset: true, Default: harness.Disabled, Skills: map[string]harness.Decision{"user:bundle-dependent": harness.Enabled}}}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		emptyFirst := turn("selective-empty-chat-kept-subscription", emptyChat, "CQ_HARNESS_FIRST_USER", nil)
		assertSelective("empty-chat-original-harness", emptyFirst, false, false, false, false)
		emptyDependency := turn("selective-empty-chat-enabled-plugin", emptyChat, "Synthetic empty chat dependency", nil)
		assertSelective("empty-chat-dependency-owner", emptyDependency, false, false, true, false)
		if !emptyDependency.FirstUser || !emptyDependency.FirstReply || emptyDependency.Markers["dependency"] == 0 {
			t.Fatal("deferred empty-chat reload lost its first turn or dependent skill")
		}
		emptyReceipt, err := receipts.Load(emptyChat)
		if err != nil || emptyReceipt.State != "applied" || len(emptyReceipt.Issues) != 0 || len(emptyReceipt.Unsupported) != 0 {
			t.Fatal("deferred empty-chat dependency was not completely applied")
		}
		t.Logf("selective installed accepted: same-chat-reloads=%d task-sibling-reloads=0 workspace-sibling-reloads=%d outsider-reloads=0 plugin-MCP-dependency=verified custom-receipt=applied external-cloud-requests=0", resumes, workspaceSiblingResumes)
		return
	}
	if bridgeEnabled {
		startAt := func(model, cwd string) string {
			t.Helper()
			result := rpc.call("thread/start", map[string]any{"cwd": cwd, "model": model, "modelProvider": "cq_harness"})
			var started struct{ Thread struct{ ID string } }
			if json.Unmarshal(result, &started) != nil || started.Thread.ID == "" {
				t.Fatal("decode bridge-managed thread ID")
			}
			return started.Thread.ID
		}
		setChat := func(id string, profile harness.Profile) {
			policyMu.Lock()
			policy = harness.Policy{Version: 1, Enabled: true, Projects: map[string]harness.Profile{command.Dir: harness.Full}, Chats: map[string]harness.Profile{id: profile}}
			policyMu.Unlock()
		}
		assertHistory := func(observation harnessInstalledObservation) {
			t.Helper()
			if !observation.FirstUser || !observation.FirstReply {
				t.Fatal("bridge reload lost the original user or assistant marker")
			}
		}
		full := start(nil)
		first := turn("bridge-project-full", full, "CQ_HARNESS_FIRST_USER", nil)
		assertTools(first, true, true)
		if first.SkillCatalogueItems != 1 || first.PluginCatalogueItems != 1 {
			t.Fatal("project full policy omitted native skill catalogues")
		}
		sibling := start(nil)
		mini := startAt("glm-5.3", filepath.Join(root, "minimal-work"))
		miniFirst := turn("bridge-default-glm-minimal", mini, "Synthetic default minimal turn", nil)
		assertTools(miniFirst, false, false)
		if miniFirst.SkillCatalogueItems != 0 || miniFirst.PluginCatalogueItems != 0 {
			t.Fatal("bridge did not suppress new GLM skill catalogues")
		}
		otherModel := startAt("gpt-5.5", filepath.Join(root, "minimal-work"))
		assertTools(turn("bridge-non-glm-full", otherModel, "Synthetic other model turn", nil), true, true)
		setChat(full, harness.Minimal)
		minimal := turn("bridge-chat-override-minimal", full, "Synthetic chat minimal override", nil)
		assertTools(minimal, false, false)
		assertHistory(minimal)
		if minimal.SkillCatalogueItems != first.SkillCatalogueItems || minimal.PluginCatalogueItems != first.PluginCatalogueItems {
			t.Fatal("bridge minimal reload added another skill catalogue")
		}
		modelReturn := turn("bridge-model-return-full", full, "Synthetic model return turn", map[string]any{"model": "gpt-5.5"})
		assertTools(modelReturn, true, true)
		assertHistory(modelReturn)
		if modelReturn.Model != "gpt-5.5" {
			t.Fatal("bridge did not preserve the requested non-GLM model")
		}
		modelBack := turn("bridge-model-back-minimal", full, "Synthetic GLM return turn", map[string]any{"model": "glm-5.3"})
		assertTools(modelBack, false, false)
		assertHistory(modelBack)
		setChat(full, harness.Full)
		restored := turn("bridge-chat-override-full", full, "Synthetic chat full override", nil)
		if receipt, err := receipts.Load(full); err == nil {
			t.Logf("bridge-final-receipt: profile=%s source=%s state=%s", receipt.Profile, receipt.Source, receipt.State)
		}
		assertTools(restored, true, true)
		assertHistory(restored)
		assertTools(turn("bridge-full-sibling-still-full", sibling, "Synthetic full sibling turn", nil), true, true)
		assertTools(turn("bridge-minimal-sibling-still-minimal", mini, "Synthetic minimal sibling turn", nil), false, false)
		receipt, err := receipts.Load(full)
		if err != nil || receipt.ChatID != full || receipt.Mode != "full" || receipt.Profile != harness.Full || receipt.Source != "chat" || receipt.State != "applied" {
			t.Fatal("bridge did not record the installed chat profile")
		}
		nativeMu.Lock()
		requests := slices.Clone(nativeRequests)
		nativeMu.Unlock()
		var unsubscribes, resumes int
		for _, request := range requests {
			if request.method() != "thread/unsubscribe" && request.method() != "thread/resume" {
				continue
			}
			if cloudThreadID(request["params"]) != full {
				t.Fatal("bridge reloaded a sibling chat")
			}
			if request.method() == "thread/unsubscribe" {
				unsubscribes++
			} else {
				resumes++
			}
		}
		if unsubscribes != 4 || resumes != 4 {
			t.Fatalf("bridge profile changes did not use four same-ID reloads: unsubscribes=%d resumes=%d", unsubscribes, resumes)
		}
		t.Logf("bridge policy accepted: same-chat-reloads=%d sibling-reloads=0 receipt=full/chat/applied external-cloud-requests=0", resumes)
		setChat(full, harness.Minimal)
		beforeRestart := turn("bridge-minimal-before-process-exit", full, "Synthetic final minimal turn", nil)
		assertTools(beforeRestart, false, false)
		assertHistory(beforeRestart)
		closeBridge := finish
		finish = func() {}
		closeBridge()
		restarted := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, client, "app-server", "--stdio", "--strict-config")
		restarted.Env, restarted.Dir, restarted.Stderr = command.Env, command.Dir, io.Discard
		restartedInput, err := restarted.StdinPipe()
		if err != nil {
			t.Fatal("open restarted native input")
		}
		restartedOutput, err := restarted.StdoutPipe()
		if err != nil {
			t.Fatal("open restarted native output")
		}
		if err := restarted.Start(); err != nil {
			t.Fatal("restart isolated native app-server")
		}
		finish = func() {
			_ = restartedInput.Close()
			if err := restarted.Wait(); err != nil {
				t.Errorf("restarted native app-server failed: %v", err)
			}
		}
		rpc = harnessInstalledRPC{t: t, encoder: json.NewEncoder(restartedInput), decoder: json.NewDecoder(restartedOutput)}
		rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cq-harness-stock-restart-acceptance", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
		_ = rpc.encoder.Encode(map[string]string{"method": "initialized"})
		resumedRaw := rpc.call("thread/resume", map[string]string{"threadId": full})
		var resumed struct{ Thread struct{ ID string } }
		if json.Unmarshal(resumedRaw, &resumed) != nil || resumed.Thread.ID != full {
			t.Fatal("stock native restart changed the synthetic chat ID")
		}
		afterRestart := turn("stock-process-restart-without-cq-overrides", full, "Synthetic stock restart turn", nil)
		assertTools(afterRestart, true, true)
		assertHistory(afterRestart)
		t.Log("native lifecycle accepted: same-chat stock restart restored normal global capabilities without CQ or config overrides")
		return
	}
	minimal := map[string]any{"skills.include_instructions": false, "features.plugins": false, "mcp_servers.probe.enabled": false}
	full := start(nil)
	first := turn("full-first", full, "CQ_HARNESS_FIRST_USER", nil)
	assertTools(first, true, true)
	if first.SkillCatalogueItems != 1 || first.PluginCatalogueItems != 1 {
		t.Fatal("full chat omitted fixture skill catalogues")
	}
	sibling := start(nil)
	mini := start(minimal)
	miniFirst := turn("minimal-new", mini, "Synthetic minimal turn", nil)
	assertTools(miniFirst, false, false)
	if miniFirst.SkillCatalogueItems != 0 || miniFirst.PluginCatalogueItems != 0 {
		t.Fatal("minimal chat retained a fixture skill catalogue")
	}
	rpc.call("thread/settings/update", map[string]any{"threadId": full, "disabledPluginIds": []string{"probe-plugin@cq-harness-market"}})
	assertTools(turn("loaded-plugin-disabled", full, "Synthetic plugin disable turn", nil), true, false)
	assertTools(turn("loaded-plugin-restored", full, "Synthetic plugin restore turn", map[string]any{"disabledPluginIds": []string{}}), true, true)
	rpc.call("thread/resume", map[string]any{"threadId": full, "config": minimal})
	beforeReload := turn("loaded-resume-kept-config", full, "Synthetic loaded resume turn", nil)
	assertTools(beforeReload, true, true)
	result := rpc.call("thread/unsubscribe", map[string]string{"threadId": full})
	var unsubscribed struct{ Status string }
	if json.Unmarshal(result, &unsubscribed) != nil || unsubscribed.Status != "unsubscribed" {
		t.Fatal("native sole-subscriber chat did not unsubscribe")
	}
	result = rpc.call("thread/resume", map[string]any{"threadId": full, "config": minimal})
	var resumed struct{ Thread struct{ ID string } }
	if json.Unmarshal(result, &resumed) != nil || resumed.Thread.ID != full {
		t.Fatal("native reconfiguration changed the chat ID")
	}
	reconfigured := turn("unsubscribed-resume-minimal", full, "Synthetic reconfigured turn", nil)
	assertTools(reconfigured, false, false)
	if !reconfigured.FirstUser || !reconfigured.FirstReply || reconfigured.SkillCatalogueItems != beforeReload.SkillCatalogueItems || reconfigured.PluginCatalogueItems != beforeReload.PluginCatalogueItems {
		t.Fatal("native reconfiguration lost history or added another skill catalogue")
	}
	assertTools(turn("full-sibling-still-full", sibling, "Synthetic full sibling turn", nil), true, true)
	assertTools(turn("minimal-sibling-still-minimal", mini, "Synthetic minimal sibling turn", nil), false, false)
	rpc.call("thread/unsubscribe", map[string]string{"threadId": full})
	rpc.call("thread/resume", map[string]any{"threadId": full, "config": map[string]any{"skills.include_instructions": true, "features.plugins": true, "mcp_servers.probe.enabled": true}})
	explicitFull := turn("same-model-explicit-full-restore", full, "Synthetic explicit full restore", nil)
	assertTools(explicitFull, true, true)
	if !explicitFull.FirstUser || !explicitFull.FirstReply {
		t.Fatal("explicit same-model full restore lost original history")
	}
	rpc.call("thread/unsubscribe", map[string]string{"threadId": full})
	rpc.call("thread/resume", map[string]any{"threadId": full, "config": minimal})
	assertTools(turn("same-model-minimal-restored", full, "Synthetic minimal restore", nil), false, false)
	changed := turn("minimal-model-switch", full, "Synthetic model switch turn", map[string]any{"model": "gpt-5.5"})
	assertTools(changed, false, false)
	if changed.Model != "gpt-5.5" || !changed.FirstUser || !changed.FirstReply {
		t.Fatal("native model switch changed history or capability isolation")
	}
	following := turn("model-switch-persisted", full, "Synthetic following turn", nil)
	assertTools(following, false, false)
	if following.Model != changed.Model {
		t.Fatal("native model switch did not persist for the following turn")
	}
	forkResult := rpc.call("thread/fork", map[string]any{"threadId": full, "model": "glm-5.3"})
	var forked struct {
		Thread struct{ ID, SessionID string }
	}
	if json.Unmarshal(forkResult, &forked) != nil || forked.Thread.ID == "" || forked.Thread.ID == full {
		t.Fatal("native fork did not create a distinct synthetic chat ID")
	}
	forkObservation := turn("native-fork-identity", forked.Thread.ID, "Synthetic fork identity turn", nil)
	if !forkObservation.FirstUser || !forkObservation.FirstReply {
		t.Fatal("native synthetic fork lost its inherited history")
	}
	t.Logf("native-fork-tree-identity: parent-chat=%s fork-chat=%s fork-tree-session=%s", full, forked.Thread.ID, forked.Thread.SessionID)
	spawnRoot := start(minimal)
	assertTools(turn("native-minimal-spawn-parent-identity", spawnRoot, "CQ_HARNESS_SPAWN_PARENT", nil), false, false)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	var child harnessInstalledObservation
	for child.Identity == nil {
		select {
		case observation := <-observed:
			if observation.Identity["body.client_metadata.thread_id"] != spawnRoot {
				child = observation
			}
		case <-deadline.C:
			t.Fatal("native synthetic spawn did not produce a child request")
		}
	}
	childID := child.Identity["body.client_metadata.thread_id"]
	assertTools(child, false, false)
	if child.SkillCatalogueItems != 0 || child.PluginCatalogueItems != 0 {
		t.Fatal("native child did not inherit its minimal parent's skill suppression")
	}
	childResult := rpc.call("thread/read", map[string]any{"threadId": childID, "includeTurns": false})
	var childThread struct {
		Thread struct{ ID, SessionID, ParentThreadID string }
	}
	if json.Unmarshal(childResult, &childThread) != nil || childThread.Thread.ID != childID || childThread.Thread.SessionID != spawnRoot || childThread.Thread.ParentThreadID != spawnRoot {
		t.Fatal("native child thread did not retain the shared tree session identity")
	}
	if childID == "" || childID == spawnRoot || child.Identity["body.client_metadata.session_id"] != spawnRoot || child.Identity["body.prompt_cache_key"] != spawnRoot || child.Identity["header.X-Codex-Turn-Metadata.thread_id"] != childID || child.Identity["header.X-Client-Request-Id"] != childID {
		t.Fatal("native subagent identity did not distinguish its chat from the tree session")
	}
	t.Logf("native-subagent-identity: root-chat=%s child-chat=%s shared-session=%s metadata-thread=%s request-id=%s inherited-minimal-tools=%d child-catalogues=0", spawnRoot, childID, childThread.Thread.SessionID, child.Identity["body.client_metadata.thread_id"], child.Identity["header.X-Client-Request-Id"], len(child.Tools))
	// Fixture-created agents remain open after their final response. Archive the
	// synthetic tree before closing stdio so native shutdown need not await it.
	rpc.call("thread/archive", map[string]string{"threadId": childID})
	rpc.call("thread/archive", map[string]string{"threadId": spawnRoot})
}

type harnessInstalledObservation struct {
	Model                string
	Tools                []string
	SkillCatalogueItems  int
	PluginCatalogueItems int
	InputItems           int
	RequestBytes         int
	FirstUser            bool
	FirstReply           bool
	Identity             map[string]string
	Markers              map[string]int
	ReasoningEffort      string
}

// Only structural identity fields from synthetic requests are retained. Prompt
// input, tool descriptions, authorisation headers and account data are excluded.
func harnessInstalledIdentityFields(prefix string, object map[string]any, output map[string]string) {
	for key, value := range object {
		name := prefix + "." + key
		switch key {
		case "thread_id", "session_id", "turn_id", "conversation_id", "parent_thread_id", "previous_response_id", "prompt_cache_key":
			if text, ok := value.(string); ok {
				output[name] = text
			}
		case "metadata", "client_metadata", "_meta", "x-codex-turn-metadata":
			if child, ok := value.(map[string]any); ok {
				harnessInstalledIdentityFields(name, child, output)
			}
		}
	}
}

type harnessInstalledMessage struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type harnessInstalledRPC struct {
	t       *testing.T
	encoder *json.Encoder
	decoder *json.Decoder
	id      int
}

func (r *harnessInstalledRPC) next() harnessInstalledMessage {
	r.t.Helper()
	var message harnessInstalledMessage
	if err := r.decoder.Decode(&message); err != nil {
		r.t.Fatalf("read installed app-server message: %v", err)
	}
	return message
}

func (r *harnessInstalledRPC) call(method string, params any) json.RawMessage {
	r.t.Helper()
	r.id++
	if err := r.encoder.Encode(map[string]any{"id": r.id, "method": method, "params": params}); err != nil {
		r.t.Fatal("write installed app-server request")
	}
	for {
		message := r.next()
		if message.ID != r.id {
			continue
		}
		if len(message.Error) != 0 && string(message.Error) != "null" {
			r.t.Fatalf("installed app-server rejected %s", method)
		}
		return message.Result
	}
}

func TestHarnessInstalledMCPHelper(t *testing.T) {
	if os.Getenv("CQ_HARNESS_INSTALLED_MCP_HELPER") != "1" {
		t.Skip("synthetic installed-harness MCP subprocess")
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		result := map[string]any{}
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "synthetic-harness", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "probe_marker_tool", "description": "Synthetic capability marker", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		case "resources/templates/list":
			result = map[string]any{"resourceTemplates": []any{}}
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
	os.Exit(0)
}
