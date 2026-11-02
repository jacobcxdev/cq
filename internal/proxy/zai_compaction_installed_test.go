//go:build !windows

package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
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

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

// Exercise the installed client protocol rather than manufacturing a compact
// request. Both credentials and model responses are local test fixtures.
func TestZAIInstalledCompactionAndContinuation(t *testing.T) {
	if os.Getenv("CQ_ZAI_COMPACTION_ACCEPTANCE") != "1" {
		t.Skip("installed Z.ai compaction acceptance requires explicit opt-in")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("installed compaction acceptance uses the macOS loopback sandbox")
	}
	client := os.Getenv("CQ_ZAI_LIVE_CODEX_PATH")
	if !filepath.IsAbs(client) {
		t.Fatal("CQ_ZAI_LIVE_CODEX_PATH must name an absolute Codex 0.166 executable")
	}
	proof, err := captureCodexInstalledExecutable(client)
	if err != nil {
		t.Fatal("capture installed Codex executable")
	}
	for _, webSocket := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "websocket"}[webSocket], func(t *testing.T) {
			localToken := "cq-compact-acceptance-" + rand.Text()
			isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
			marker := "RETAINED-" + rand.Text()
			oldNoise := "DISCARDED-" + rand.Text()
			summary := "Continuation summary: the retained marker is " + marker + ". Reply with that marker when asked."
			var calls, compactRequests, continuations atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := httputil.ReadBodyLimit(r.Body, maxRequestBody)
				if err != nil {
					t.Error("read fake Z.ai request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer fake-zai-compaction-key" {
					t.Error("unexpected fake Z.ai route or credential")
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				var request map[string]json.RawMessage
				if json.Unmarshal(body, &request) != nil {
					t.Error("decode fake Z.ai request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				switch calls.Add(1) {
				case 1:
					if !bytes.Contains(request["input"], []byte(marker)) {
						t.Error("initial native turn did not retain marker")
					}
					// Native compaction preserves the original user request. Put
					// disposable history in assistant output so reduction is real.
					writeZAIInstalledCompactionSSE(w, "initial-response", "STORED", oldNoise+strings.Repeat(" disposable-history", 2048))
				case 2:
					compactRequests.Add(1)
					if !bytes.Contains(request["input"], []byte(marker)) || !bytes.Contains(request["input"], []byte(oldNoise)) || !bytes.Contains(request["input"], []byte(zaiCompactionPrompt)) {
						t.Error("compaction request lost history or summary instruction")
					}
					if bytes.Contains(body, []byte("compaction_trigger")) || len(request["tools"]) != 0 || string(request["tool_choice"]) != `"none"` {
						t.Error("compaction trigger or tools reached fake Z.ai")
					}
					writeZAIInstalledCompactionSSE(w, "summary-response", summary, "")
				case 3:
					continuations.Add(1)
					if !bytes.Contains(request["input"], []byte(summary)) {
						t.Error("follow-up did not restore the retained summary")
					}
					if bytes.Contains(body, []byte(zaiCompactionPrefix)) || bytes.Contains(body, []byte(oldNoise)) || bytes.Contains(body, []byte("compaction_trigger")) {
						t.Errorf("follow-up retained cipher=%t old-noise=%t trigger=%t", bytes.Contains(body, []byte(zaiCompactionPrefix)), bytes.Contains(body, []byte(oldNoise)), bytes.Contains(body, []byte("compaction_trigger")))
						var items []json.RawMessage
						_ = json.Unmarshal(request["input"], &items)
						for i, raw := range items {
							if bytes.Contains(raw, []byte(oldNoise)) {
								var item struct{ Type, Role string }
								_ = json.Unmarshal(raw, &item)
								t.Logf("retained old-noise item: index=%d/%d type=%q role=%q", i, len(items), item.Type, item.Role)
							}
						}
					}
					writeZAIInstalledCompactionSSE(w, "continued-response", marker, "")
				default:
					t.Error("unexpected additional fake Z.ai request")
					http.Error(w, "unexpected request", http.StatusBadRequest)
				}
			}))
			defer upstream.Close()
			catalog, catalogue := zaiInstalledCompactionCatalogue(t)
			cataloguePath := filepath.Join(isolation.root, "models.json")
			if err := os.WriteFile(cataloguePath, catalogue, 0o600); err != nil {
				t.Fatal("write isolated model catalogue")
			}
			server := &Server{
				Config: &Config{LocalToken: localToken}, Catalog: catalog,
				ZAIAPIKey: func() (string, error) { return "fake-zai-compaction-key", nil }, ZAIUpstream: upstream.URL,
				CodexNativeHTTP: zaiForbiddenCodexHTTP{t},
				CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{
					Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce,
				}},
			}
			handler, err := server.handler()
			if err != nil {
				t.Fatal("create isolated CQ compaction handler")
			}
			var httpRequests, webSockets atomic.Int64
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					webSockets.Add(1)
				} else if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") {
					httpRequests.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			defer listener.Close()
			args := append(codexLiveAppServerArguments(listener.URL, webSocket),
				"-c", `model_providers.cq_acceptance.capabilities.remote_compaction="v2"`,
				"-c", `model_reasoning_effort="low"`,
				"-c", "model_catalog_json="+strconv.Quote(cataloguePath),
			)
			command := codexAcceptanceCommand{
				executable: client, expectedExecutable: proof, args: args,
				env: append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config),
					"XDG_DATA_HOME="+isolation.data, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost"),
				dir: isolation.work, endpoint: listener.URL + "/responses", sandboxWriteRoot: isolation.root, loopbackOnly: true,
			}
			profile, err := codexAcceptanceSandboxProfile(command)
			if err != nil {
				t.Fatal("build installed compaction sandbox")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			versionCommand := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, client, "--version")
			versionCommand.Env, versionCommand.Dir = command.env, command.dir
			version, err := versionCommand.Output()
			if err != nil || !strings.HasPrefix(strings.TrimSpace(string(version)), "codex-cli 0.166.") {
				t.Fatal("installed compaction acceptance requires Codex 0.166")
			}
			child := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", append([]string{"-p", profile, client}, args...)...)
			child.Env, child.Dir, child.WaitDelay = command.env, command.dir, 2*time.Second
			stderr := &codexAcceptanceDiagnosticBuffer{}
			child.Stderr = stderr
			stdin, err := child.StdinPipe()
			if err != nil {
				t.Fatal("open installed compaction input")
			}
			stdout, err := child.StdoutPipe()
			if err != nil {
				_ = stdin.Close()
				t.Fatal("open installed compaction output")
			}
			if err := child.Start(); err != nil {
				_ = stdin.Close()
				t.Fatal("start installed compaction client")
			}
			defer func() {
				_ = stdin.Close()
				if err := child.Wait(); err != nil {
					t.Errorf("installed compaction process: %v; %s", err, sanitiseCodexAcceptanceDiagnostic(string(stderr.data)))
				}
				if after, err := captureCodexInstalledExecutable(client); err != nil || after != proof {
					t.Error("installed Codex executable changed during acceptance")
				}
			}()
			encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
			if err := encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "cq-zai-compaction-acceptance", "version": "1"}}}); err != nil {
				t.Fatal("initialise installed compaction client")
			}
			if _, err := awaitCodexAppServerResponse(decoder, 1); err != nil {
				t.Fatal(err)
			}
			_ = encoder.Encode(map[string]string{"method": "initialized"})
			if err := encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{
				"approvalPolicy": "never", "cwd": isolation.work, "ephemeral": true,
				"model": "glm-5.3", "modelProvider": "cq_acceptance", "sandbox": "danger-full-access",
				"developerInstructions": "Keep the requested marker. Do not call tools.",
			}}); err != nil {
				t.Fatal("start installed compaction thread")
			}
			started, err := awaitCodexAppServerResponse(decoder, 2)
			if err != nil {
				t.Fatal(err)
			}
			var thread struct {
				Thread struct{ ID string } `json:"thread"`
			}
			if json.Unmarshal(started, &thread) != nil || thread.Thread.ID == "" {
				t.Fatal("decode installed compaction thread")
			}
			initial := "Retain marker " + marker + ". Reply exactly STORED."
			if err := encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{
				"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": initial}},
			}}); err != nil {
				t.Fatal("start installed initial turn")
			}
			if err := awaitCodexAppServerTextTurn(decoder, 3, thread.Thread.ID, "STORED"); err != nil {
				t.Fatal(err)
			}
			if err := encoder.Encode(map[string]any{"id": 4, "method": "thread/compact/start", "params": map[string]string{"threadId": thread.Thread.ID}}); err != nil {
				t.Fatal("start installed compaction")
			}
			if err := awaitCodexAppServerTurn(decoder, 4, thread.Thread.ID); err != nil {
				t.Fatal(err)
			}
			if err := encoder.Encode(map[string]any{"id": 5, "method": "turn/start", "params": map[string]any{
				"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Return the retained marker only."}},
			}}); err != nil {
				t.Fatal("start installed post-compaction turn")
			}
			if err := awaitCodexAppServerTextTurn(decoder, 5, thread.Thread.ID, marker); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 3 || compactRequests.Load() != 1 || continuations.Load() != 1 {
				t.Fatalf("vendor traffic: requests=%d compactions=%d continuations=%d", calls.Load(), compactRequests.Load(), continuations.Load())
			}
			if webSocket && webSockets.Load() == 0 || !webSocket && (webSockets.Load() != 0 || httpRequests.Load() != 3) {
				t.Fatalf("native transport: websocket=%d HTTP=%d", webSockets.Load(), httpRequests.Load())
			}
			if !t.Failed() {
				t.Logf("native compact-and-continue accepted: upstream=3, compaction=1, restored-summary=1, websocket=%d, HTTP=%d", webSockets.Load(), httpRequests.Load())
			}
		})
	}
}

func zaiInstalledCompactionCatalogue(t *testing.T) (*modelregistry.Catalog, []byte) {
	t.Helper()
	raw := json.RawMessage(`{"slug":"glm-5.3","display_name":"GLM-5.3","description":"Local compaction acceptance fixture","default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low","description":"Low"}],"shell_type":"shell_command","visibility":"list","supported_in_api":true,"priority":0,"base_instructions":"","supports_reasoning_summaries":true,"default_reasoning_summary":"none","support_verbosity":false,"apply_patch_tool_type":"freeform","truncation_policy":{"mode":"bytes","limit":10000},"context_window":1048576,"max_context_window":1048576,"effective_context_window_percent":95,"supports_parallel_tool_calls":true,"experimental_supported_tools":[],"input_modalities":["text"]}`)
	snapshot := modelregistry.Snapshot{
		Entries:      []modelregistry.Entry{{Provider: modelregistry.ProviderZAI, ID: "glm-5.3", Source: modelregistry.SourceNative, Raw: raw}},
		CodexRawByID: map[string]json.RawMessage{"glm-5.3": raw},
	}
	catalogue, err := json.Marshal(modelregistry.CodexModelsResponse(snapshot))
	if err != nil {
		t.Fatal("encode installed compaction catalogue")
	}
	return modelregistry.NewCatalog(snapshot), catalogue
}

func writeZAIInstalledCompactionSSE(w http.ResponseWriter, id, text, discarded string) {
	item := map[string]any{"id": id + "_message", "type": "message", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}}
	output := []map[string]any{item}
	if discarded != "" {
		noise := map[string]any{"id": id + "_commentary", "type": "message", "role": "assistant", "phase": "commentary", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": discarded, "annotations": []any{}}}}
		output = append([]map[string]any{noise}, item)
	}
	response := map[string]any{"id": id, "object": "response", "status": "completed", "output": output, "usage": map[string]int{"input_tokens": 32, "output_tokens": 1, "total_tokens": 33}}
	w.Header().Set("Content-Type", "text/event-stream")
	events := []map[string]any{{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "output": []any{}}}}
	for index, item := range output {
		events = append(events, map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	}
	events = append(events, map[string]any{"type": "response.completed", "response": response})
	for _, event := range events {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
}
