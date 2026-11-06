//go:build !windows

package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/provider/grok"
)

// This opt-in test spends subscription quota. The installed Codex process only
// receives a temporary CQ loopback token; Grok credentials stay in CQ's owner.
func TestGrokLiveInstalledToolsCompactionAndContinuation(t *testing.T) {
	if os.Getenv("CQ_GROK_LIVE_TEST") != "1" {
		t.Skip("live Grok subscription qualification requires explicit opt-in")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("installed Grok qualification uses the macOS loopback sandbox")
	}
	config := os.Getenv("CQ_GROK_LIVE_CONFIG")
	if !filepath.IsAbs(config) {
		t.Fatal("CQ_GROK_LIVE_CONFIG must select CQ's absolute credential directory")
	}
	client := os.Getenv("CQ_GROK_LIVE_CODEX_PATH")
	if client == "" {
		var err error
		client, err = resolveCodexInstalledClientExecutable()
		if err != nil {
			t.Fatal("resolve installed Codex executable")
		}
	}
	if !filepath.IsAbs(client) {
		t.Fatal("CQ_GROK_LIVE_CODEX_PATH must name an absolute Codex executable")
	}
	proof, err := captureCodexInstalledExecutable(client)
	if err != nil {
		t.Fatal("capture installed Codex executable")
	}
	authority := grok.NewAuthority(grok.Store{FS: fsutil.OSFileSystem{}, ConfigDir: config}, &http.Client{Timeout: 90 * time.Second}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	source := modelregistry.GrokSource{Authority: authority}
	models, err := source.Fetch(ctx)
	cancel()
	if err != nil {
		t.Fatalf("live Grok subscription catalogue unavailable: %v", err)
	}
	model := os.Getenv("CQ_GROK_LIVE_MODEL")
	if model == "" && len(models.Entries) > 0 {
		model = models.Entries[0].ID
	}
	found := false
	for _, entry := range models.Entries {
		found = found || entry.ID == model
	}
	if !found {
		t.Fatal("live Grok blocker: no entitled model advertises a Responses backend; inference was not attempted")
	}
	snapshot := modelregistry.Snapshot{Entries: models.Entries, CodexRawByID: models.CodexRawByID}
	catalogue, err := json.Marshal(modelregistry.CodexModelsResponse(snapshot))
	if err != nil {
		t.Fatal("encode isolated Grok catalogue")
	}
	for _, useWebSocket := range []bool{false, true} {
		name := "http"
		if useWebSocket {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			localToken := "cq-grok-live-" + rand.Text()
			isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
			marker, fixture := "MARKER-"+rand.Text(), "FIXTURE-"+rand.Text()
			if err := os.WriteFile(filepath.Join(isolation.work, "fixture.txt"), []byte(fixture+"\n"), 0o600); err != nil {
				t.Fatal("write isolated Grok tool fixture")
			}
			cataloguePath := filepath.Join(isolation.root, "models.json")
			if err := os.WriteFile(cataloguePath, catalogue, 0o600); err != nil {
				t.Fatal("write isolated Grok catalogue")
			}
			observer := &grokLiveAuthority{Authority: authority, marker: marker, fixture: fixture}
			defer observer.report(t)
			server := &Server{
				Config: &Config{LocalToken: localToken}, Catalog: modelregistry.NewCatalog(snapshot),
				GrokAuthority: observer, CodexNativeHTTP: zaiForbiddenCodexHTTP{t},
				CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce}},
			}
			handler, err := server.handler()
			if err != nil {
				t.Fatal("create isolated Grok listener")
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
			args := append(codexLiveAppServerArguments(listener.URL, useWebSocket),
				"-c", `model_providers.cq_acceptance.capabilities.remote_compaction="v2"`,
				"-c", `model_reasoning_effort="low"`,
				"-c", "model_catalog_json="+strconv.Quote(cataloguePath))
			command := codexAcceptanceCommand{
				executable: client, expectedExecutable: proof, args: args,
				env: append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config),
					"XDG_DATA_HOME="+isolation.data, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost"),
				dir: isolation.work, endpoint: listener.URL + "/responses", sandboxWriteRoot: isolation.root, loopbackOnly: true,
			}
			profile, err := codexAcceptanceSandboxProfile(command)
			if err != nil {
				t.Fatal("build installed Grok client sandbox")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			child := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", append([]string{"-p", profile, client}, args...)...)
			child.Env, child.Dir, child.WaitDelay = command.env, command.dir, 2*time.Second
			stderr := &codexAcceptanceDiagnosticBuffer{}
			child.Stderr = stderr
			stdin, err := child.StdinPipe()
			if err != nil {
				t.Fatal("open installed Grok client input")
			}
			stdout, err := child.StdoutPipe()
			if err != nil {
				_ = stdin.Close()
				t.Fatal("open installed Grok client output")
			}
			if err := child.Start(); err != nil {
				_ = stdin.Close()
				t.Fatal("start installed Grok client")
			}
			defer func() {
				_ = stdin.Close()
				if err := child.Wait(); err != nil {
					t.Errorf("installed Grok client failed (timeout=%t, stderr_bytes=%d)", ctx.Err() != nil, len(stderr.data))
				}
				if useWebSocket && codexAcceptanceWebSocketTransportFailed(stderr.data) {
					t.Error("installed Grok client fell back from WebSocket transport")
				}
				if after, err := captureCodexInstalledExecutable(client); err != nil || after != proof {
					t.Error("installed Codex executable changed during Grok qualification")
				}
				if !t.Failed() {
					t.Logf("Grok local tools before/after CQ compaction and recompaction passed: websocket=%d HTTP=%d", webSockets.Load(), httpRequests.Load())
				}
			}()
			encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
			send := func(value any) {
				t.Helper()
				if err := encoder.Encode(value); err != nil {
					t.Fatal("write installed Grok app-server request")
				}
			}
			send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "cq-grok-live", "version": "1"}}})
			if _, err := awaitCodexAppServerResponse(decoder, 1); err != nil {
				t.Fatal(err)
			}
			send(map[string]string{"method": "initialized"})
			send(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{
				"approvalPolicy": "never", "cwd": isolation.work, "ephemeral": true,
				"model": model, "modelProvider": "cq_acceptance", "sandbox": "danger-full-access",
				"developerInstructions": "Follow exact output instructions. Only read fixture.txt when asked; never modify files or use web tools.",
			}})
			started, err := awaitCodexAppServerResponse(decoder, 2)
			if err != nil {
				t.Fatal(err)
			}
			var thread struct {
				Thread struct{ ID string } `json:"thread"`
			}
			if json.Unmarshal(started, &thread) != nil || thread.Thread.ID == "" {
				t.Fatal("decode installed Grok thread")
			}
			send(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{
				"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Remember marker " + marker + ". Use your shell tool to read fixture.txt in the current directory. Your final reply must be exactly the single token in that file, without whitespace, quotes, Markdown or explanation. Keep the marker and file token in memory for the next turn; do not mention the marker in this reply."}},
			}})
			if err := awaitCodexAppServerToolTurn(decoder, 3, thread.Thread.ID, fixture); err != nil {
				t.Fatal(err)
			}
			send(map[string]any{"id": 4, "method": "thread/compact/start", "params": map[string]string{"threadId": thread.Thread.ID}})
			if err := awaitCodexAppServerTurn(decoder, 4, thread.Thread.ID); err != nil {
				t.Fatal(err)
			}
			send(map[string]any{"id": 5, "method": "turn/start", "params": map[string]any{
				"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Without calling tools, return the remembered marker, then |, then the remembered file contents. No spaces or formatting."}},
			}})
			if err := awaitCodexAppServerTextTurn(decoder, 5, thread.Thread.ID, marker+"|"+fixture); err != nil {
				t.Fatal(err)
			}

			followup := "FOLLOWUP-" + rand.Text()
			if err := os.WriteFile(filepath.Join(isolation.work, "fixture.txt"), []byte(followup+"\n"), 0o600); err != nil {
				t.Fatal("update isolated Grok tool fixture")
			}
			send(map[string]any{"id": 6, "method": "turn/start", "params": map[string]any{
				"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": "The contents of fixture.txt have changed since your previous read. Call your shell tool again to read the new value. Your old remembered value is stale. Reply exactly with the new single file token, without whitespace, quotes, Markdown or explanation. Keep the original marker for the next turn."}},
			}})
			if err := awaitCodexAppServerToolTurn(decoder, 6, thread.Thread.ID, followup); err != nil {
				t.Fatal(err)
			}
			send(map[string]any{"id": 7, "method": "thread/compact/start", "params": map[string]string{"threadId": thread.Thread.ID}})
			if err := awaitCodexAppServerTurn(decoder, 7, thread.Thread.ID); err != nil {
				t.Fatal(err)
			}
			send(map[string]any{"id": 8, "method": "turn/start", "params": map[string]any{
				"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Without calling tools, return the original remembered marker, then |, then the most recent file token you read after its contents changed. No spaces, quotes, Markdown or explanation."}},
			}})
			if err := awaitCodexAppServerTextTurn(decoder, 8, thread.Thread.ID, marker+"|"+followup); err != nil {
				t.Fatal(err)
			}

			observer.mu.Lock()
			compacted, retained, compactions := observer.compacted, observer.compactRetained, observer.compactions
			observer.mu.Unlock()
			if !compacted || !retained || compactions != 2 {
				t.Fatalf("CQ Grok compaction evidence incomplete: successful=%t retained_marker_and_tool_result=%t", compacted, retained)
			}
			if useWebSocket && (webSockets.Load() == 0 || httpRequests.Load() != 0) || !useWebSocket && (httpRequests.Load() == 0 || webSockets.Load() != 0) {
				t.Fatalf("native Grok transport mismatch: websocket=%d HTTP=%d", webSockets.Load(), httpRequests.Load())
			}
		})
	}
}

type grokLiveAuthority struct {
	grok.Authority
	mu                         sync.Mutex
	marker, fixture            string
	statuses                   []grokLiveStatus
	compacted, compactRetained bool
	compactions                int
}
type grokLiveStatus struct {
	method, path string
	status       int
}

func (a *grokLiveAuthority) Do(ctx context.Context, id grok.AccountID, request *http.Request) (*http.Response, error) {
	compact, retained := false, false
	if request.Method == http.MethodPost && request.GetBody != nil {
		body, err := request.GetBody()
		if err == nil {
			data, readErr := httputil.ReadBodyLimit(body, maxRequestBody)
			_ = body.Close()
			compact = readErr == nil && bytes.Contains(data, []byte(zaiCompactionPrompt))
			retained = compact && bytes.Contains(data, []byte(a.marker)) && bytes.Contains(data, []byte(a.fixture))
		}
	}
	response, err := a.Authority.Do(ctx, id, request)
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.statuses = append(a.statuses, grokLiveStatus{request.Method, request.URL.Path, status})
	if compact {
		a.compacted = err == nil && status >= 200 && status < 300
		if a.compactions == 0 {
			a.compactRetained = retained
		} else {
			a.compactRetained = a.compactRetained && retained
		}
		a.compactions++
	}
	return response, err
}
func (a *grokLiveAuthority) report(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, status := range a.statuses {
		t.Logf("Grok upstream: method=%s path=%s status=%d", status.method, status.path, status.status)
	}
}
