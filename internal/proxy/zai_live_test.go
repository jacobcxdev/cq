//go:build !windows

package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func TestZAILiveNativeSubagentsAndFollowup(t *testing.T) {
	if os.Getenv("CQ_ZAI_LIVE_TEST") != "1" {
		t.Skip("live Z.ai native subagent qualification requires explicit opt-in")
	}
	client := os.Getenv("CQ_ZAI_LIVE_CODEX_PATH")
	if !filepath.IsAbs(client) {
		t.Fatal("CQ_ZAI_LIVE_CODEX_PATH must select an absolute Codex 0.166 executable")
	}
	file, err := os.Open(os.Getenv("CQ_ZAI_LIVE_CREDENTIAL_FILE"))
	if err != nil {
		t.Fatal("open live Z.ai credential file")
	}
	var credential struct {
		APIKey string `json:"api_key"`
	}
	err = json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&credential)
	_ = file.Close()
	if err != nil || strings.TrimSpace(credential.APIKey) == "" {
		t.Fatal("read live Z.ai credential file")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	source := modelregistry.ZAISource{
		Client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Token:  func(context.Context) (string, error) { return credential.APIKey, nil },
	}
	models, err := source.Fetch(ctx)
	cancel()
	if err != nil {
		t.Fatalf("live Z.ai catalogue discovery: %v", err)
	}
	snapshot := modelregistry.Snapshot{Entries: models.Entries, CodexRawByID: models.CodexRawByID}
	catalogue, err := json.Marshal(modelregistry.CodexModelsResponse(snapshot))
	if err != nil {
		t.Fatal("encode private live Z.ai catalogue")
	}
	for _, useWebSocket := range []bool{false, true} {
		name := "http"
		if useWebSocket {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			home := filepath.Join(root, "home")
			codexHome := filepath.Join(root, "codex")
			tmp := filepath.Join(root, "tmp")
			for _, directory := range []string{work, home, codexHome, tmp} {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal("create isolated live Z.ai qualification directory")
				}
			}
			cataloguePath := filepath.Join(root, "models.json")
			if err := os.WriteFile(cataloguePath, catalogue, 0o600); err != nil {
				t.Fatal("write private live Z.ai catalogue")
			}
			first, second := "FIRST-"+rand.Text(), "SECOND-"+rand.Text()
			for filename, content := range map[string]string{"fixture-a.txt": first, "fixture-b.txt": second} {
				if err := os.WriteFile(filepath.Join(work, filename), []byte(content+"\n"), 0o600); err != nil {
					t.Fatal("write private live Z.ai fixture")
				}
			}
			localToken := "cq-zai-live-" + rand.Text()
			observer := &zaiLiveTransport{calls: make(map[string]zaiLiveCall)}
			server := &Server{
				Config: &Config{LocalToken: localToken}, Catalog: modelregistry.NewCatalog(snapshot),
				ZAIAPIKey: func() (string, error) { return credential.APIKey, nil }, ZAITransport: observer,
				CodexNativeHTTP: zaiForbiddenCodexHTTP{t},
				CodexRouting:    &CodexRoutingRuntime{WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce}},
			}
			handler, err := server.handler()
			if err != nil {
				t.Fatal("create isolated CQ listener")
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
			environment := append(codexAcceptanceBaseEnvironment(home, codexHome, tmp, "", ""),
				"CQ_ZAI_LIVE_LOCAL_TOKEN="+localToken, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
			versionCommand := exec.Command(client, "--version")
			versionCommand.Env = environment
			version, err := versionCommand.Output()
			if err != nil || !strings.HasPrefix(strings.TrimSpace(string(version)), "codex-cli 0.166.") {
				t.Fatal("live Z.ai qualification requires Codex 0.166")
			}
			outputPath := filepath.Join(root, "final.txt")
			provider := fmt.Sprintf(`{name="CQ live Z.ai",base_url=%s,wire_api="responses",env_key="CQ_ZAI_LIVE_LOCAL_TOKEN",supports_websockets=%t,request_max_retries=0,stream_max_retries=0}`, strconv.Quote(listener.URL), useWebSocket)
			arguments := []string{"exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--color", "never", "-s", "read-only", "-C", work, "-o", outputPath, "-m", "glm-5.3",
				"-c", `approval_policy="never"`, "-c", `model_reasoning_effort="low"`, "-c", `model_provider="cq_zai_live"`, "-c", "model_providers.cq_zai_live=" + provider,
				"-c", "model_catalog_json=" + strconv.Quote(cataloguePath), "-c", "features.plugins=false", "-c", "features.apps=false", "-c", "features.multi_agent_v2=true", "-c", "analytics.enabled=false", "-"}
			prompt := "You are the root coordinator. Do not read, list, search or open files yourself. Do not invoke shell, MCP, file or web tools yourself. Use only collaboration tools. " +
				"Spawn exactly one child with task_name fixture_reader and fork_turns none. Tell that child to read only fixture-a.txt in the current directory using its shell tool and reply with exactly the file contents. Wait for its final reply. " +
				"Then call followup_task on the SAME child /root/fixture_reader. Tell it to read only fixture-b.txt using its shell tool and reply with exactly those file contents. Wait for that final reply. " +
				"Do not spawn another child or close the child. Do not override the child's model or reasoning effort. Your final reply must be exactly the first file contents, then |, then the second file contents, without spaces, explanation or formatting."
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, client, arguments...)
			command.Env, command.Dir, command.Stdin = environment, work, strings.NewReader(prompt)
			stdout, stderr := &codexAcceptanceDiagnosticBuffer{}, &codexAcceptanceDiagnosticBuffer{}
			command.Stdout, command.Stderr, command.WaitDelay = stdout, stderr, 2*time.Second
			if err := command.Run(); err != nil {
				t.Fatalf("native GLM subagent run failed (timeout=%t, stdout_bytes=%d, stderr_bytes=%d, upstream_calls=%d)", ctx.Err() != nil, len(stdout.data), len(stderr.data), observer.count())
			}
			for _, line := range bytes.Split(stdout.data, []byte("\n")) {
				var event struct {
					Item struct {
						Type string
					} `json:"item"`
				}
				if json.Unmarshal(line, &event) != nil {
					continue
				}
				switch event.Item.Type {
				case "command_execution", "mcp_tool_call", "file_change", "web_search":
					t.Fatalf("root invoked forbidden %s tool", event.Item.Type)
				}
			}
			observer.assertChildFollowup(t)
			final, err := os.ReadFile(outputPath)
			if err != nil || strings.TrimSpace(string(final)) != first+"|"+second {
				t.Fatalf("native child fixture result mismatch (readable=%t, final_bytes=%d)", err == nil, len(final))
			}
			if useWebSocket && (webSockets.Load() == 0 || httpRequests.Load() != 0 || codexAcceptanceWebSocketTransportFailed(stderr.data)) {
				t.Fatalf("native WebSocket transport fell back (websockets=%d, http=%d)", webSockets.Load(), httpRequests.Load())
			}
			if !useWebSocket && (httpRequests.Load() == 0 || webSockets.Load() != 0) {
				t.Fatalf("native HTTP transport evidence incomplete (http=%d, websockets=%d)", httpRequests.Load(), webSockets.Load())
			}
			t.Logf("native GLM child spawn, fixture reads and same-child follow-up passed (http=%d, websockets=%d)", httpRequests.Load(), webSockets.Load())
		})
	}
}

type zaiLiveCall struct {
	name, taskName, forkTurns, target string
}

type zaiLiveTransport struct {
	mu          sync.Mutex
	calls       map[string]zaiLiveCall
	parseFailed bool
}

func (transport *zaiLiveTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, errors.New("live Z.ai upstream transport failed")
	}
	response.Body = &zaiLiveResponseObserver{body: response.Body, parser: NewCodexSSEParser(1 << 20), transport: transport}
	return response, nil
}

type zaiLiveResponseObserver struct {
	body      io.ReadCloser
	parser    *CodexSSEParser
	transport *zaiLiveTransport
}

func (observer *zaiLiveResponseObserver) Read(buffer []byte) (int, error) {
	n, readErr := observer.body.Read(buffer)
	events, parseErr := observer.parser.Feed(buffer[:n])
	for _, event := range events {
		observer.transport.observe(event.Data)
	}
	if readErr == io.EOF && parseErr == nil {
		_, parseErr = observer.parser.Finish()
	}
	if parseErr != nil {
		observer.transport.mu.Lock()
		observer.transport.parseFailed = true
		observer.transport.mu.Unlock()
	}
	return n, readErr
}

func (observer *zaiLiveResponseObserver) Close() error {
	return observer.body.Close()
}

func (transport *zaiLiveTransport) observe(data []byte) {
	var event struct {
		Item     json.RawMessage `json:"item"`
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	for _, raw := range append(event.Response.Output, event.Item) {
		var item struct {
			Type, Name, Arguments string
			CallID                string `json:"call_id"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "function_call" {
			continue
		}
		var arguments struct {
			TaskName  string `json:"task_name"`
			ForkTurns string `json:"fork_turns"`
			Target    string `json:"target"`
		}
		if json.Unmarshal([]byte(item.Arguments), &arguments) != nil {
			continue
		}
		transport.mu.Lock()
		transport.calls[item.CallID] = zaiLiveCall{item.Name, arguments.TaskName, arguments.ForkTurns, arguments.Target}
		transport.mu.Unlock()
	}
}

func (transport *zaiLiveTransport) count() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return len(transport.calls)
}

func (transport *zaiLiveTransport) assertChildFollowup(t *testing.T) {
	t.Helper()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.parseFailed {
		t.Fatal("live Z.ai tool observation parser failed")
	}
	spawned, followed := false, false
	spawns := 0
	for _, call := range transport.calls {
		if strings.HasSuffix(call.name, "spawn_agent") {
			spawns++
			spawned = call.taskName == "fixture_reader" && call.forkTurns == "none"
		}
		if strings.HasSuffix(call.name, "followup_task") && (call.target == "/root/fixture_reader" || call.target == "fixture_reader") {
			followed = true
		}
	}
	if spawns != 1 || !spawned || !followed {
		t.Fatalf("native same-child follow-up evidence incomplete (spawns=%d, spawned=%t, followed=%t, tool_calls=%d)", spawns, spawned, followed, len(transport.calls))
	}
}
