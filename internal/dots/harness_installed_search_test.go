package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

// Exercise actual connector execution and continuation with the compiled
// minimal profile and the CQ model catalogue, using only synthetic auth.
func TestHarnessInstalledCuratedWebSearch(t *testing.T) {
	if os.Getenv("CQ_DOTS_HARNESS_INSTALLED_ACCEPTANCE") != "1" || runtime.GOOS != "darwin" {
		t.Skip("requires opt-in installed macOS acceptance")
	}
	client := os.Getenv("CQ_DOTS_HARNESS_INSTALLED_CODEX")
	if !filepath.IsAbs(client) {
		t.Fatal("CQ_DOTS_HARNESS_INSTALLED_CODEX must name the native executable")
	}
	const model = "glm-5.3"
	const searchResult = "CQ_SEARCH_RESULT https://example.invalid"
	var calls, continuations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := httputil.ReadBody(r.Body)
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/api/codex/ps/mcp", "/backend-api/ps/mcp":
			request := cloudHarnessObject(body)
			params := cloudHarnessObject(request["params"])
			result := map[string]any{}
			switch request.method() {
			case "initialize":
				result = map[string]any{
					"protocolVersion": cloudHarnessString(params, "protocolVersion"),
					"capabilities":    map[string]any{"tools": map[string]bool{}},
					"serverInfo":      map[string]string{"name": "search-fixture", "version": "1"},
				}
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
				return
			case "tools/list":
				result["tools"] = []any{
					map[string]any{
						"name": "search_service.web_run", "description": "Search the web",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
						"annotations": map[string]bool{"readOnlyHint": true},
						"_meta": map[string]any{
							"connector_id": cloudHarnessSearchApp, "connector_name": "search_service",
							"_codex_apps": map[string]any{"invocation_context": map[string]any{"config": map[string]string{"tool_type": "fixture"}}},
						},
					},
					map[string]any{
						"name": "calendar.list_events", "description": "CQ_UNSELECTED_APP",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
						"_meta":       map[string]string{"connector_id": "connector_calendar", "connector_name": "calendar"},
					},
				}
			case "tools/call":
				if cloudHarnessString(params, "name") != "search_service.web_run" || !bytes.Contains(params["_meta"], []byte("invocation_context")) {
					t.Error("unexpected Search connector call")
				}
				calls.Add(1)
				result["content"] = []map[string]string{{"type": "text", "text": searchResult}}
			default:
				http.Error(w, "unexpected MCP method", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": result})
		case "/responses":
			request := cloudHarnessObject(body)
			if cloudHarnessString(request, "model") != model {
				t.Error("model changed for search")
			}
			if bytes.Contains(request["tools"], []byte("CQ_UNSELECTED_APP")) {
				t.Error("minimal profile exposed an unrelated app tool")
			}
			item := map[string]any{"id": "reply", "type": "message", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": "Search fixture finished.", "annotations": []any{}}}}
			if bytes.Contains(request["input"], []byte(searchResult)) {
				continuations.Add(1)
			} else if namespace, name := harnessInstalledSearchTool(request["tools"], ""); name != "" {
				item = map[string]any{"id": "search", "type": "function_call", "name": name, "arguments": "{}", "call_id": "search-call", "status": "completed"}
				if namespace != "" {
					item["namespace"] = namespace
				}
			} else {
				t.Errorf("no Search function in curated model request: %s", request["tools"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": "fixture", "output": []any{}}},
				{"type": "response.output_item.done", "output_index": 0, "item": item},
				{"type": "response.completed", "response": map[string]any{"id": "fixture", "output": []any{item}}},
			} {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", cloudHarnessRaw(event))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "NO_PROXY=localhost,127.0.0.1", "CI=1"}
	for key, name := range map[string]string{"HOME": "home", "CODEX_HOME": "codex", "TMPDIR": "tmp", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		env = append(env, key+"="+path)
	}
	auth := catalogueTestDocument("search-fixture", time.Now().Add(time.Hour).Unix())
	auth["auth_mode"] = "chatgpt"
	auth["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	catalogueTestWrite(t, filepath.Join(root, "codex", "auth.json"), auth)
	catalog := filepath.Join(root, "models.json")
	catalogueTestWrite(t, catalog, modelregistry.CodexModelsResponse(modelregistry.Snapshot{Entries: []modelregistry.Entry{
		{ID: model, Provider: modelregistry.ProviderZAI, ContextWindow: 1000000, Source: modelregistry.SourceNative, Raw: json.RawMessage(`{"slug":"glm-5.3","display_name":"Fixture","description":"Synthetic model","base_instructions":"Search when asked.","supported_reasoning_levels":[],"shell_type":"shell_command","visibility":"list","supported_in_api":true,"priority":1,"availability_nux":null,"upgrade":null,"support_verbosity":false,"default_verbosity":null,"apply_patch_tool_type":null,"truncation_policy":{"mode":"tokens","limit":10000},"experimental_supported_tools":[],"context_window":1000000,"supports_search_tool":false}`)},
	}}))
	config := fmt.Sprintf("model = %q\nmodel_provider = \"fixture\"\ncli_auth_credentials_store = \"file\"\nchatgpt_base_url = %q\nmodel_catalog_json = %q\n[analytics]\nenabled = false\n[features]\napps = true\nenable_request_compression = false\n[model_providers.fixture]\nname = \"Search fixture\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = true\nrequest_max_retries = 0\nstream_max_retries = 0\n", model, upstream.URL, catalog, upstream.URL)
	if err := os.WriteFile(filepath.Join(root, "codex", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	sandbox := `(version 1) (allow default) (deny network*) (deny file-write*) (allow network-outbound (remote ip "localhost:*")) (allow file-write* (literal "/dev/null")) (allow file-write* (subpath ` + strconv.Quote(root) + `))`
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", sandbox, client, "app-server", "--stdio", "--strict-config")
	cmd.Dir, cmd.Env, cmd.Stderr = root, env, os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	rpc := &harnessInstalledRPC{t: t, encoder: json.NewEncoder(input), decoder: json.NewDecoder(output)}
	rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cq-search-fixture", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
	_ = rpc.encoder.Encode(map[string]string{"method": "initialized"})
	plan := cloudHarnessCompileProfile(harness.Effective{Changes: harness.Changes{Default: harness.Disabled}}, harness.Inventory{}, nil, nil)
	thread := cloudThreadID(rpc.call("thread/start", map[string]any{"cwd": root, "model": model, "config": plan.config}))
	if thread == "" {
		t.Fatal("missing native thread ID")
	}
	rpc.call("turn/start", map[string]any{"threadId": thread, "input": []map[string]string{{"type": "text", "text": "Search the web."}}})
	for {
		notice := rpc.next()
		if notice.Method == "turn/completed" && cloudThreadID(notice.Params) == thread {
			break
		}
	}
	if calls.Load() != 1 || continuations.Load() != 1 {
		t.Fatalf("expected one Search connector call and one model continuation, got %d and %d", calls.Load(), continuations.Load())
	}
}

func harnessInstalledSearchTool(raw json.RawMessage, namespace string) (string, string) {
	var tools []cloudMessage
	_ = json.Unmarshal(raw, &tools)
	for _, tool := range tools {
		name := cloudHarnessString(tool, "name")
		switch cloudHarnessString(tool, "type") {
		case "namespace":
			if ns, name := harnessInstalledSearchTool(tool["tools"], name); name != "" {
				return ns, name
			}
		case "function":
			if strings.Contains(namespace+"."+name, "search_service") && strings.HasSuffix(name, "web_run") {
				return namespace, name
			}
		}
	}
	return "", ""
}
