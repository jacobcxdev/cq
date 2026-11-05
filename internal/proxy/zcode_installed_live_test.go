//go:build !windows

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode/messages"
)

// TestZCodeInstalledLive exercises the real installed Codex harness. Only the
// model-request budget is clamped: tools, state, compaction and replies remain
// owned by Codex and the real ZCode service. Each selected case permits at most
// three real model calls, capped at 512 Flash / 2,048 flagship output tokens.
func TestZCodeInstalledLive(t *testing.T) {
	if os.Getenv("CQ_ZCODE_NATIVE_LIVE") != "1" {
		t.Skip("set CQ_ZCODE_NATIVE_LIVE=1 for installed Codex ZCode qualification")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("installed Codex qualification requires the macOS loopback sandbox")
	}
	model, effort, outputCap := "glm-5.3-flash", "low", 512
	if selected := os.Getenv("CQ_ZCODE_NATIVE_MODEL"); selected != "" {
		if selected != "glm-5.3" {
			t.Fatal("qualification model must be glm-5.3 or default Flash")
		}
		model, effort, outputCap = selected, "max", 2048
	}
	config := os.Getenv("CQ_ZCODE_LIVE_CONFIG")
	if !filepath.IsAbs(config) || filepath.Clean(config) == "/" {
		t.Fatal("CQ_ZCODE_LIVE_CONFIG must select an existing private OAuth profile")
	}
	accounts, err := zaiprov.ReadAccounts(fsutil.OSFileSystem{}, func(string) string { return "" }, config)
	start := os.Getenv("CQ_ZCODE_NATIVE_START") == "1"
	if err != nil || len(accounts) == 0 || accounts[0].OAuth == nil || accounts[0].Transport != "zcode" && !(start && accounts[0].Transport == "zcode-start") {
		t.Fatal("qualification requires the preferred ZCode OAuth account; no API-key fallback")
	}
	account := accounts[0]
	if start {
		// Select Start only in this test's account copy; keep the saved route intact.
		account.Transport = "zcode-start"
	}
	device := account.OAuth.DeviceMid
	if device == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal("resolve existing ZCode device identity")
		}
		data, err := os.ReadFile(filepath.Join(home, ".zcode", "v2", "telemetry-state.json"))
		var state struct {
			DeviceMid string `json:"deviceMid"`
		}
		if err != nil || len(data) > httputil.MaxResponseBody || json.Unmarshal(data, &state) != nil || state.DeviceMid == "" {
			t.Fatal("existing ZCode device identity unavailable")
		}
		device = state.DeviceMid
	}
	const clientPath = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex"
	proof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatal("capture actual installed Codex executable")
	}
	for _, transport := range []struct {
		name      string
		websocket bool
	}{{"http", false}, {"websocket", true}} {
		t.Run(transport.name, func(t *testing.T) {
			for _, mode := range []string{"tools", "compaction"} {
				t.Run(mode, func(t *testing.T) {
					localToken := "cq-native-zcode-" + rand.Text()
					isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
					budget := &zcodeInstalledBudgetTransport{base: http.DefaultTransport, outputCap: outputCap}
					vendor := httputil.NewClientWithTransport(45*time.Second, "zcode-native-qualification", budget)
					projectTokens := &zcode.ProjectTokenClient{HTTP: vendor, Version: "3.17.2"}
					defer projectTokens.Clear()
					catalog, catalogue := zcodeInstalledLiveCatalogue(t, model, effort)
					cataloguePath := filepath.Join(isolation.root, "models.json")
					if err := os.WriteFile(cataloguePath, catalogue, 0o600); err != nil {
						t.Fatal("write isolated model metadata")
					}
					server := &Server{
						Config: &Config{LocalToken: localToken}, Catalog: catalog,
						ZAIAccounts:      func() ([]zaiprov.Account, error) { return []zaiprov.Account{account}, nil },
						ZAIProjectTokens: projectTokens,
						ZAIGateway:       &zcode.Gateway{HTTP: vendor, Signer: &zcode.Signer{HTTP: vendor, Version: zcode.ClientVersion}, Version: zcode.ClientVersion, DeviceMid: device, Catalogue: &zcode.ModelCatalogue{HTTP: vendor, Version: zcode.ClientVersion}},
						CodexNativeHTTP:  zaiForbiddenCodexHTTP{t},
						CodexRouting:     &CodexRoutingRuntime{WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce}},
					}
					handler, err := server.handler()
					if err != nil {
						t.Fatal("build isolated CQ handler")
					}
					var httpCalls, wsCalls atomic.Int64
					var localStatus atomic.Int64
					var nativeShape atomic.Value
					defer func() {
						shape, _ := nativeShape.Load().(string)
						failure, _ := budget.failure.Load().(string)
						t.Logf("status=%d vendor_code=%d local_status=%d auth_calls=%d auth_status=%d signing_calls=%d model_calls=%d model_attempts=%d tool_continuations=%d compactions=%d native_http=%d native_websocket=%d output_tokens_cap=%d transport_failure=%q request_shape=%s", budget.status.Load(), budget.vendorCode.Load(), localStatus.Load(), budget.authCalls.Load(), budget.authStatus.Load(), budget.signingCalls.Load(), budget.calls.Load(), budget.attempts.Load(), budget.toolContinuations.Load(), budget.compactions.Load(), httpCalls.Load(), wsCalls.Load(), outputCap, failure, shape)
					}()
					listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if websocket.IsWebSocketUpgrade(r) {
							wsCalls.Add(1)
						} else if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") {
							httpCalls.Add(1)
							nativeShape.Store(zcodeInstalledRequestShape(r))
						}
						handler.ServeHTTP(&zcodeInstalledStatusWriter{ResponseWriter: w, status: &localStatus}, r)
					}))
					defer listener.Close()
					args := append(codexLiveAppServerArguments(listener.URL, transport.websocket),
						"-c", `model_providers.cq_acceptance.capabilities.remote_compaction="v2"`,
						"-c", "model_reasoning_effort="+strconv.Quote(effort),
						"-c", "model_catalog_json="+strconv.Quote(cataloguePath),
						"-c", "features.responses_websockets="+strconv.FormatBool(transport.websocket),
						"-c", "features.responses_websockets_v2="+strconv.FormatBool(transport.websocket),
					)
					command := codexAcceptanceCommand{executable: clientPath, expectedExecutable: proof, args: args,
						env: append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config), "XDG_DATA_HOME="+isolation.data, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost"),
						dir: isolation.work, endpoint: listener.URL + "/responses", sandboxWriteRoot: isolation.root, loopbackOnly: true,
					}
					profile, err := codexAcceptanceSandboxProfile(command)
					if err != nil {
						t.Fatal("construct native-client containment")
					}
					// Real credentials stay in the parent test process. The harness
					// receives only a dummy local bearer and cannot read their source.
					profile += ` (deny file-read* (subpath ` + strconv.Quote(filepath.Clean(config)) + `))`
					if originalHome, err := os.UserHomeDir(); err == nil {
						profile += ` (deny file-read* (subpath ` + strconv.Quote(filepath.Join(originalHome, ".codex")) + `))`
					}
					ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
					defer cancel()
					versionCommand := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, clientPath, "--version")
					versionCommand.Env, versionCommand.Dir = command.env, command.dir
					version, err := versionCommand.Output()
					if err != nil || !strings.HasPrefix(strings.TrimSpace(string(version)), "codex-cli 0.166.") {
						t.Fatal("qualification requires installed Codex 0.166")
					}
					child := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", append([]string{"-p", profile, clientPath}, args...)...)
					child.Env, child.Dir, child.WaitDelay = command.env, command.dir, 2*time.Second
					child.Stderr = &codexAcceptanceDiagnosticBuffer{}
					stdin, err := child.StdinPipe()
					if err != nil {
						t.Fatal("open isolated app-server input")
					}
					stdout, err := child.StdoutPipe()
					if err != nil {
						_ = stdin.Close()
						t.Fatal("open isolated app-server output")
					}
					if err := child.Start(); err != nil {
						_ = stdin.Close()
						t.Fatal("launch actual installed Codex")
					}
					defer func() {
						_ = stdin.Close()
						if t.Failed() {
							cancel()
						}
						if child.Wait() != nil && !t.Failed() {
							t.Error("isolated installed Codex exited unsuccessfully")
						}
						if after, err := captureCodexInstalledExecutable(clientPath); err != nil || after != proof {
							t.Error("installed Codex identity changed during qualification")
						}
					}()
					encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
					if encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "cq-zcode-native-qualification", "version": "1"}}}) != nil {
						t.Fatal("initialise native app-server")
					}
					if _, err := awaitCodexAppServerResponse(decoder, 1); err != nil {
						t.Fatal("native initialisation failed")
					}
					_ = encoder.Encode(map[string]string{"method": "initialized"})
					if encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{
						"approvalPolicy": "never", "cwd": isolation.work, "ephemeral": true, "model": model, "modelProvider": "cq_acceptance", "sandbox": "danger-full-access",
						"developerInstructions": "Follow the harmless qualification prompt exactly. Use tools only for the single requested printf command. Never read files, change files, access network, or delegate work.",
					}}) != nil {
						t.Fatal("start isolated native thread")
					}
					started, err := awaitCodexAppServerResponse(decoder, 2)
					var thread struct{ Thread struct{ ID string } }
					if err != nil || json.Unmarshal(started, &thread) != nil || thread.Thread.ID == "" {
						t.Fatal("native thread creation failed")
					}
					marker := "ZCODE_NATIVE_" + rand.Text()
					if mode == "tools" {
						prompt := "Use the shell tool exactly once to run /usr/bin/printf '%s\\n' '" + marker + "'. Do not call other tools or inspect files. After it succeeds, reply exactly " + marker + " with no formatting."
						if encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": prompt}}}}) != nil {
							t.Fatal("start native tool task")
						}
						if awaitCodexAppServerToolTurn(decoder, 3, thread.Thread.ID, marker) != nil {
							t.Fatal("native shell execution and tool-result continuation failed")
						}
						if budget.toolContinuations.Load() == 0 || budget.calls.Load() < 2 {
							t.Error("real upstream tool-result continuation missing")
						}
					} else {
						if encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Retain marker " + marker + ". Reply exactly STORED. Do not call tools."}}}}) != nil {
							t.Fatal("start native marker turn")
						}
						if awaitCodexAppServerTextTurn(decoder, 3, thread.Thread.ID, "STORED") != nil {
							t.Fatal("native marker turn failed")
						}
						if encoder.Encode(map[string]any{"id": 4, "method": "thread/compact/start", "params": map[string]string{"threadId": thread.Thread.ID}}) != nil {
							t.Fatal("request actual native compaction")
						}
						if awaitCodexAppServerTurn(decoder, 4, thread.Thread.ID) != nil {
							t.Fatal("native remote compaction failed")
						}
						if encoder.Encode(map[string]any{"id": 5, "method": "turn/start", "params": map[string]any{"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Return the retained marker only. Do not call tools."}}}}) != nil {
							t.Fatal("start native post-compaction turn")
						}
						if awaitCodexAppServerTextTurn(decoder, 5, thread.Thread.ID, marker) != nil {
							t.Fatal("native post-compaction continuation failed")
						}
						if budget.compactions.Load() != 1 || budget.calls.Load() != 3 {
							t.Error("real upstream compaction sequence incomplete")
						}
					}
					if transport.websocket && wsCalls.Load() == 0 || !transport.websocket && (wsCalls.Load() != 0 || httpCalls.Load() == 0) {
						t.Error("installed client used unexpected transport")
					}
				})
			}
		})
	}
}

func zcodeInstalledLiveCatalogue(t *testing.T, model, effort string) (*modelregistry.Catalog, []byte) {
	t.Helper()
	base, _ := zaiInstalledCompactionCatalogue(t)
	snapshot := base.Snapshot()
	raw := snapshot.Entries[0].Raw
	if model == "glm-5.3-flash" {
		raw = bytes.ReplaceAll(raw, []byte(`"glm-5.3"`), []byte(`"glm-5.3-flash"`))
		raw = bytes.ReplaceAll(raw, []byte(`"GLM-5.3"`), []byte(`"GLM-5.3-Flash"`))
	}
	var record map[string]json.RawMessage
	_ = json.Unmarshal(raw, &record)
	record["default_reasoning_level"], _ = json.Marshal(effort)
	record["supported_reasoning_levels"] = json.RawMessage(`[{"effort":"low","description":"Low"},{"effort":"high","description":"High"},{"effort":"max","description":"Max"}]`)
	raw, _ = json.Marshal(record)
	snapshot.Entries[0].ID, snapshot.Entries[0].Raw = model, raw
	snapshot.CodexRawByID = map[string]json.RawMessage{model: raw}
	catalogue, err := json.Marshal(modelregistry.CodexModelsResponse(snapshot))
	if err != nil {
		t.Fatal("encode isolated Flash model metadata")
	}
	return modelregistry.NewCatalog(snapshot), catalogue
}

// Budget guard affects only this isolated qualification's real Messages POSTs.
// It never supplies a model reply, tool result, compaction or continuation.
type zcodeInstalledBudgetTransport struct {
	base                                                    http.RoundTripper
	outputCap                                               int
	attempts, calls, toolContinuations, compactions, status atomic.Int64
	authCalls, authStatus, signingCalls, vendorCode         atomic.Int64
	failure                                                 atomic.Value
}

func (b *zcodeInstalledBudgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	model := req.URL.Host == "zcode.z.ai" && strings.HasSuffix(req.URL.Path, "/anthropic/v1/messages")
	if !model {
		b.authCalls.Add(1)
		if req.URL.Path == "/api/paas/c1f3a7e2/v2/client" {
			b.signingCalls.Add(1)
		}
	}
	if model {
		if b.attempts.Add(1) > 3 {
			return nil, errors.New("isolated model request budget exhausted")
		}
		if req.Body == nil {
			return nil, errors.New("isolated model request missing body")
		}
		data, err := httputil.ReadBodyLimit(req.Body, maxRequestBody)
		_ = req.Body.Close()
		if err != nil {
			return nil, errors.New("read isolated model request")
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(data, &body) != nil {
			return nil, errors.New("decode isolated model request")
		}
		var maximum int
		if json.Unmarshal(body["max_tokens"], &maximum) != nil || maximum <= 0 || maximum > b.outputCap {
			body["max_tokens"] = json.RawMessage(strconv.Itoa(b.outputCap))
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("bound isolated model request")
		}
		if bytes.Contains(encoded, []byte(`"type":"tool_result"`)) {
			b.toolContinuations.Add(1)
		}
		compact, _ := json.Marshal(zaiCompactionPrompt)
		if bytes.Contains(encoded, compact[1:len(compact)-1]) {
			b.compactions.Add(1)
		}
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(encoded))
		req.ContentLength = int64(len(encoded))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
		req.Header.Del("Content-Length")
		b.calls.Add(1)
	}
	rsp, err := b.base.RoundTrip(req)
	if err != nil {
		kind := "network"
		var dns *net.DNSError
		var timeout net.Error
		switch {
		case errors.Is(err, context.Canceled):
			kind = "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			kind = "deadline"
		case errors.As(err, &dns):
			kind = "dns"
		case errors.As(err, &timeout) && timeout.Timeout():
			kind = "timeout"
		}
		b.failure.Store(kind)
	}
	if model && rsp != nil {
		b.status.Store(int64(rsp.StatusCode))
	} else if rsp != nil {
		b.authStatus.Store(int64(rsp.StatusCode))
	}
	if rsp != nil && rsp.Body != nil && (!model || rsp.StatusCode < 200 || rsp.StatusCode >= 300) {
		body, readErr := httputil.ReadBody(rsp.Body)
		_ = rsp.Body.Close()
		if readErr != nil {
			body = nil
			b.failure.Store("diagnostic_read_limit_or_error")
		}
		rsp.Body = io.NopCloser(bytes.NewReader(body))
		var envelope struct {
			Code  json.RawMessage
			Error struct{ Code json.RawMessage }
		}
		if json.Unmarshal(body, &envelope) == nil {
			code := envelope.Code
			if len(code) == 0 {
				code = envelope.Error.Code
			}
			var text string
			if json.Unmarshal(code, &text) != nil {
				text = string(code)
			}
			if value, err := strconv.ParseInt(text, 10, 64); err == nil {
				b.vendorCode.Store(value)
			}
		}
	}
	return rsp, err
}

type zcodeInstalledStatusWriter struct {
	http.ResponseWriter
	status *atomic.Int64
}

func (w *zcodeInstalledStatusWriter) WriteHeader(status int) {
	w.status.Store(int64(status))
	w.ResponseWriter.WriteHeader(status)
}
func (w *zcodeInstalledStatusWriter) Write(data []byte) (int, error) {
	if w.status.Load() == 0 {
		w.status.Store(200)
	}
	return w.ResponseWriter.Write(data)
}
func (w *zcodeInstalledStatusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
func (w *zcodeInstalledStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *zcodeInstalledStatusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		w.status.Store(101)
		return hijacker.Hijack()
	}
	return nil, nil, errors.New("isolated writer cannot hijack")
}

func zcodeInstalledRequestShape(req *http.Request) string {
	shape := map[string]any{}
	if req.Body == nil {
		return `{"body":false}`
	}
	raw, err := httputil.ReadBodyLimit(req.Body, maxRequestBody)
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return `{"body_read_failed":true}`
	}
	encoding, err := parseCodexContentEncoding(req.Header)
	if err != nil {
		return `{"encoding_invalid":true}`
	}
	decoded := raw
	if encoding == "zstd" || encoding == "" && codexZstdFrame(raw) {
		shape["zstd"] = true
		request, err := DecodeCodexRequest(raw, encoding, codexHTTPZstdLimits())
		if err != nil {
			return `{"zstd_decode_failed":true}`
		}
		decoded = request.Decoded()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(decoded, &fields) != nil {
		return `{"json_invalid":true}`
	}
	var generate bool
	if value, ok := fields["generate"]; ok && json.Unmarshal(value, &generate) == nil {
		shape["generate_false"] = !generate
	}
	var input []json.RawMessage
	if json.Unmarshal(fields["input"], &input) == nil {
		shape["input_array"] = true
		shape["input_items"] = len(input)
	} else {
		shape["input_array"] = false
	}
	kinds := map[string]int{}
	var count func([]json.RawMessage)
	count = func(tools []json.RawMessage) {
		for _, raw := range tools {
			var tool struct {
				Type  string
				Tools []json.RawMessage
			}
			if json.Unmarshal(raw, &tool) != nil {
				kinds["invalid"]++
				continue
			}
			kind := "other"
			switch tool.Type {
			case "function", "custom", "namespace", "tool_search", "web_search", "local_shell", "image_generation":
				kind = tool.Type
			}
			kinds[kind]++
			if len(tool.Tools) > 0 {
				count(tool.Tools)
			}
		}
	}
	var tools []json.RawMessage
	_ = json.Unmarshal(fields["tools"], &tools)
	count(tools)
	shape["tool_types"] = kinds
	normal, normalErr := normaliseZAIRequest(decoded)
	if normalErr != nil {
		shape["normalise_ok"] = false
	} else {
		shape["normalise_ok"] = true
		_, _, projectErr := messages.Project(normal)
		shape["project_ok"] = projectErr == nil
		if projectErr != nil {
			kind := "request"
			text := projectErr.Error()
			switch {
			case strings.Contains(text, "tool"):
				kind = "tool"
			case strings.Contains(text, "reasoning"):
				kind = "reasoning"
			case strings.Contains(text, "history"):
				kind = "history"
			case strings.Contains(text, "input") || strings.Contains(text, "content"):
				kind = "input"
			}
			shape["project_failure"] = kind
			hash := sha256.Sum256([]byte(text))
			shape["project_error_id"] = hex.EncodeToString(hash[:4])
		}
	}
	encoded, _ := json.Marshal(shape)
	return string(encoded)
}
