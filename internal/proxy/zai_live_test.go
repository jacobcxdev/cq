//go:build !windows

package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
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
	forcedFirst := os.Getenv("CQ_ZAI_LIVE_FAILOVER_FIRST_ID")
	accounts, err := readZAILiveAccounts(file, forcedFirst)
	_ = file.Close()
	if err != nil {
		t.Fatal("read live Z.ai credential file")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	source := modelregistry.ZAISource{
		Client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Token:  func(context.Context) (string, error) { return accounts[0].APIKey, nil },
	}
	if forcedFirst != "" {
		source.Tokens = func(context.Context) ([]string, error) {
			keys := make([]string, len(accounts))
			for i, account := range accounts {
				keys[i] = account.APIKey
			}
			return keys, nil
		}
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
	for _, test := range []struct {
		name                string
		useWebSocket, drain bool
	}{{name: "http"}, {name: "websocket", useWebSocket: true}, {name: "websocket-drain", useWebSocket: true, drain: true}} {
		t.Run(test.name, func(t *testing.T) {
			useWebSocket := test.useWebSocket
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
			observer := &zaiLiveTransport{calls: make(map[string]zaiLiveCall), accounts: accounts, forcedFirst: forcedFirst, statuses: make(map[string][]int)}
			if forcedFirst != "" {
				defer observer.logAccountStatuses(t)
			}
			server := &Server{
				Config: &Config{LocalToken: localToken}, Catalog: modelregistry.NewCatalog(snapshot),
				ZAIAPIKey: func() (string, error) { return accounts[0].APIKey, nil }, ZAITransport: observer,
				CodexNativeHTTP: zaiForbiddenCodexHTTP{t},
				CodexRouting:    &CodexRoutingRuntime{WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce}},
			}
			if forcedFirst != "" {
				server.ZAIAccounts = func() ([]zaiprov.Account, error) { return accounts, nil }
			}
			var handler http.Handler
			var worker runtimeDrainHandler
			var err error
			if test.drain {
				handler, err = server.RuntimeHandler()
				if err == nil {
					worker = handler.(runtimeDrainHandler)
					handler = RuntimeUpgradeHTTPHandler(handler, worker.RuntimeUpgradeAdmission())
					defer worker.ResumeUpgrade(context.Background())
				}
			} else {
				handler, err = server.handler()
			}
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
			var drained, replayed atomic.Bool
			drainDone := make(chan error, 1)
			if test.drain {
				var once sync.Once
				originalSpawnID := ""
				observer.onCompleted = func() {
					observer.mu.Lock()
					spawnID := ""
					for id, call := range observer.calls {
						if strings.HasSuffix(call.name, "spawn_agent") && call.taskName == "fixture_reader" {
							spawnID = id
						}
					}
					observer.mu.Unlock()
					if spawnID == "" {
						return
					}
					once.Do(func() {
						originalSpawnID = spawnID
						if err := worker.PrepareUpgrade(context.Background()); err != nil {
							drainDone <- errors.New("pause isolated runtime")
							return
						}
						go func() {
							var drainErr error
							defer func() {
								if recover() != nil {
									drainErr = errors.New("isolated runtime drain observer panicked")
								}
								if drainErr == nil {
									drained.Store(true)
								}
								if err := worker.ResumeUpgrade(context.Background()); drainErr == nil && err != nil {
									drainErr = errors.New("resume isolated runtime")
								}
								drainDone <- drainErr
							}()
							wait, cancel := context.WithTimeout(t.Context(), 20*time.Second)
							defer cancel()
							drainErr = worker.AwaitUpgradeQuiescence(wait)
						}()
					})
				}
				observer.beforeRequest = func(request *http.Request) {
					if !drained.Load() || request.GetBody == nil {
						return
					}
					body, err := request.GetBody()
					if err != nil {
						return
					}
					defer body.Close()
					var submitted struct {
						Input []json.RawMessage `json:"input"`
					}
					if json.NewDecoder(io.LimitReader(body, maxRequestBody+1)).Decode(&submitted) != nil {
						return
					}
					retainedPrompt, retainedCall := false, false
					for _, raw := range submitted.Input {
						retainedPrompt = retainedPrompt || bytes.Contains(raw, []byte(prompt))
						var item struct {
							Type   string `json:"type"`
							CallID string `json:"call_id"`
						}
						if json.Unmarshal(raw, &item) == nil && item.Type == "function_call" && originalSpawnID != "" && item.CallID == originalSpawnID {
							retainedCall = true
						}
					}
					if retainedPrompt && retainedCall {
						replayed.Store(true)
					}
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, client, arguments...)
			command.Env, command.Dir, command.Stdin = environment, work, strings.NewReader(prompt)
			stdout, stderr := &codexAcceptanceDiagnosticBuffer{}, &codexAcceptanceDiagnosticBuffer{}
			command.Stdout, command.Stderr, command.WaitDelay = stdout, stderr, 2*time.Second
			if err := command.Run(); err != nil {
				t.Fatalf("native GLM subagent run failed (timeout=%t, stdout_bytes=%d, stderr_bytes=%d, upstream_calls=%d, drained=%t, replayed=%t, websockets=%d, http=%d, restart_1012=%t, history_rejected=%t, fallback=%t)", ctx.Err() != nil, len(stdout.data), len(stderr.data), observer.count(), drained.Load(), replayed.Load(), webSockets.Load(), httpRequests.Load(), bytes.Contains(stderr.data, []byte("1012")), bytes.Contains(stderr.data, []byte("previous response unavailable")), codexAcceptanceWebSocketTransportFailed(stderr.data))
			}
			if test.drain {
				select {
				case err := <-drainDone:
					if err != nil {
						t.Fatalf("isolated native GLM drain failed: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("isolated native GLM drain did not finish")
				}
				if !drained.Load() || !replayed.Load() || webSockets.Load() < 3 {
					t.Fatalf("native GLM reconnect history evidence incomplete (drained=%t, replayed=%t, websockets=%d)", drained.Load(), replayed.Load(), webSockets.Load())
				}
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
			if forcedFirst != "" {
				observer.assertFailover(t)
			}
			t.Logf("native GLM child spawn, fixture reads and same-child follow-up passed (http=%d, websockets=%d)", httpRequests.Load(), webSockets.Load())
			if test.drain {
				t.Log("native GLM continued after isolated upgrade drain with original prompt and spawn-call history replayed")
			}
		})
	}
}

func readZAILiveAccounts(reader io.Reader, forcedFirst string) ([]zaiprov.Account, error) {
	var credential struct {
		APIKey   string `json:"api_key"`
		Accounts []struct {
			ID     string `json:"id"`
			APIKey string `json:"api_key"`
		} `json:"accounts"`
	}
	if json.NewDecoder(io.LimitReader(reader, 64<<10)).Decode(&credential) != nil {
		return nil, errors.New("invalid live Z.ai credential file")
	}
	accountID := func(key string) string {
		digest := sha256.Sum256([]byte(key))
		return "zai_" + hex.EncodeToString(digest[:8])
	}
	if forcedFirst == "" {
		if strings.TrimSpace(credential.APIKey) == "" {
			return nil, errors.New("missing live Z.ai credential")
		}
		return []zaiprov.Account{{ID: accountID(credential.APIKey), APIKey: credential.APIKey}}, nil
	}
	accounts := make([]zaiprov.Account, 0, len(credential.Accounts))
	seen := make(map[string]bool)
	first := -1
	for _, stored := range credential.Accounts {
		key := strings.TrimSpace(stored.APIKey)
		if key == "" || stored.ID != accountID(key) || seen[stored.ID] {
			return nil, errors.New("invalid live Z.ai multi-account credentials")
		}
		seen[stored.ID] = true
		if stored.ID == forcedFirst {
			first = len(accounts)
		}
		accounts = append(accounts, zaiprov.Account{ID: stored.ID, APIKey: key})
	}
	if first < 0 || len(accounts) < 2 {
		return nil, errors.New("live Z.ai failover requires matching first ID and two accounts")
	}
	ordered := make([]zaiprov.Account, 0, len(accounts))
	ordered = append(ordered, accounts[first])
	ordered = append(ordered, accounts[:first]...)
	ordered = append(ordered, accounts[first+1:]...)
	return ordered, nil
}

func TestZAILiveAccountFixtureSelection(t *testing.T) {
	const first = `{"id":"zai_d2e6ac1e0f809e83","api_key":"first-fixture-key"}`
	const second = `{"id":"zai_982de655b82e05c8","api_key":"second-fixture-key"}`
	const third = `{"id":"zai_011ec432244130a9","api_key":"third-fixture-key"}`
	for _, test := range []struct {
		name, body, forced string
		wantIDs            []string
		wantErr            bool
	}{
		{name: "legacy", body: `{"api_key":"first-fixture-key"}`, wantIDs: []string{"zai_d2e6ac1e0f809e83"}},
		{name: "legacy ignores accounts without opt-in", body: `{"api_key":"first-fixture-key","accounts":[` + second + `]}`, wantIDs: []string{"zai_d2e6ac1e0f809e83"}},
		{name: "forced first preserves other order", body: `{"accounts":[` + first + `,` + second + `,` + third + `]}`, forced: "zai_982de655b82e05c8", wantIDs: []string{"zai_982de655b82e05c8", "zai_d2e6ac1e0f809e83", "zai_011ec432244130a9"}},
		{name: "force missing ID", body: `{"accounts":[` + first + `,` + second + `]}`, forced: "private-unknown-id", wantErr: true},
		{name: "force needs two accounts", body: `{"accounts":[` + first + `]}`, forced: "zai_d2e6ac1e0f809e83", wantErr: true},
		{name: "duplicate accounts", body: `{"accounts":[` + first + `,` + first + `]}`, forced: "zai_d2e6ac1e0f809e83", wantErr: true},
		{name: "invalid opaque ID", body: `{"accounts":[{"id":"private-key-id","api_key":"first-fixture-key"},` + second + `]}`, forced: "private-key-id", wantErr: true},
		{name: "empty legacy", body: `{}`, wantErr: true},
		{name: "invalid JSON", body: `{"api_key":"first-fixture-key",`, wantErr: true},
		{name: "bounded JSON", body: `{"api_key":"` + strings.Repeat("x", 65<<10) + `"}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts, err := readZAILiveAccounts(strings.NewReader(test.body), test.forced)
			if (err != nil) != test.wantErr {
				t.Fatalf("fixture error present=%t, want=%t", err != nil, test.wantErr)
			}
			if err != nil {
				if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "fixture-key") {
					t.Fatal("fixture credentials leaked through error")
				}
				return
			}
			if len(accounts) != len(test.wantIDs) {
				t.Fatalf("account count=%d, want=%d", len(accounts), len(test.wantIDs))
			}
			for i, account := range accounts {
				if account.ID != test.wantIDs[i] || account.APIKey == "" {
					t.Fatal("fixture account ordering or credential missing")
				}
			}
		})
	}
}

func TestZAILiveFailoverObservationRequiresRejectedThenAccepted(t *testing.T) {
	for _, test := range []struct {
		name     string
		attempts []struct {
			id     string
			status int
		}
		wantAccepted  bool
		wantUnmatched bool
	}{
		{name: "quota then accepted", attempts: []struct {
			id     string
			status int
		}{{"first", 429}, {"second", 200}}, wantAccepted: true},
		{name: "auth then accepted", attempts: []struct {
			id     string
			status int
		}{{"first", 401}, {"second", 201}}, wantAccepted: true},
		{name: "accepted before rejection", attempts: []struct {
			id     string
			status int
		}{{"second", 200}, {"first", 429}}},
		{name: "server error not rejection", attempts: []struct {
			id     string
			status int
		}{{"first", 500}, {"second", 200}}},
		{name: "same account accepted", attempts: []struct {
			id     string
			status int
		}{{"first", 429}, {"first", 200}}},
		{name: "unmatched credential", attempts: []struct {
			id     string
			status int
		}{{"", 200}}, wantUnmatched: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := &zaiLiveTransport{forcedFirst: "first", statuses: make(map[string][]int)}
			for _, attempt := range test.attempts {
				observer.observeStatus(attempt.id, attempt.status)
				if attempt.status >= 200 && attempt.status < 300 {
					observer.observeInitialEvent(attempt.id, attempt.status, []byte(`{"type":"response.created"}`))
				}
			}
			if observer.failoverAccepted != test.wantAccepted || observer.unmatchedAccount != test.wantUnmatched {
				t.Fatalf("accepted=%t unmatched=%t", observer.failoverAccepted, observer.unmatchedAccount)
			}
		})
	}
}

func TestZAILiveSSEFailoverObservation(t *testing.T) {
	const failed = `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded"}}}`
	const topError = `{"type":"error","code":"insufficient_quota"}`
	const created = `{"type":"response.created","response":{"id":"accepted"}}`
	for _, test := range []struct {
		name                       string
		first                      []string
		second                     []string
		secondBefore               bool
		wantRejected, wantAccepted bool
	}{
		{"initial failed then accepted", []string{failed}, []string{created}, false, true, true},
		{"initial top-level error then completed", []string{topError}, []string{`{"type":"response.completed","response":{"id":"completed"}}`}, false, true, true},
		{"accepted before rejection", []string{failed}, []string{created}, true, true, false},
		{"late failure after acceptance", []string{created, failed}, []string{created}, false, false, false},
		{"unknown event before failure", []string{`{"type":"future.event"}`, failed}, []string{created}, false, false, false},
		{"malformed event before failure", []string{`{"type":`, failed}, []string{created}, false, false, false},
		{"unknown failure code", []string{`{"type":"error","code":"future_error"}`}, []string{created}, false, false, false},
		{"failed response already has output", []string{`{"type":"response.failed","response":{"status":"failed","output":[{}],"error":{"code":"rate_limit_exceeded"}}}`}, []string{created}, false, false, false},
		{"both failed with HTTP200", []string{failed}, []string{failed}, false, true, false},
		{"duplicate type cannot prove rejection", []string{`{"type":"response.created","type":"error","code":"rate_limit_exceeded"}`}, []string{created}, false, false, false},
		{"duplicate code cannot prove rejection", []string{`{"type":"error","code":"future_error","code":"rate_limit_exceeded"}`}, []string{created}, false, false, false},
		{"duplicate error cannot prove rejection", []string{`{"type":"error","error":{"code":"future_error"},"error":{"code":"rate_limit_exceeded"}}`}, []string{created}, false, false, false},
		{"duplicate response cannot prove rejection", []string{`{"type":"response.failed","response":{"status":"in_progress","output":[{}]},"response":{"status":"failed","error":{"code":"rate_limit_exceeded"}}}`}, []string{created}, false, false, false},
		{"duplicate status cannot prove rejection", []string{`{"type":"response.failed","response":{"status":"in_progress","status":"failed","error":{"code":"rate_limit_exceeded"}}}`}, []string{created}, false, false, false},
		{"duplicate output cannot prove rejection", []string{`{"type":"response.failed","response":{"status":"failed","output":[{}],"output":[],"error":{"code":"rate_limit_exceeded"}}}`}, []string{created}, false, false, false},
		{"duplicate acceptance type cannot prove failover", []string{failed}, []string{`{"type":"error","type":"response.created","response":{"id":"ambiguous"}}`}, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &zaiLiveTransport{forcedFirst: "first", statuses: make(map[string][]int)}
			readResponse := func(id string, events []string) {
				transport.observeStatus(id, http.StatusOK)
				var data strings.Builder
				for _, event := range events {
					data.WriteString("data: " + event + "\n\n")
				}
				response := &zaiLiveResponseObserver{body: io.NopCloser(strings.NewReader(data.String())), parser: NewCodexSSEParser(1 << 20), transport: transport, accountID: id, status: http.StatusOK}
				if _, err := io.Copy(io.Discard, response); err != nil {
					t.Fatal("synthetic observer body read failed")
				}
			}
			if test.secondBefore {
				readResponse("second", test.second)
				readResponse("first", test.first)
			} else {
				readResponse("first", test.first)
				readResponse("second", test.second)
			}
			if transport.firstRejected != test.wantRejected || transport.failoverAccepted != test.wantAccepted {
				t.Fatalf("rejected=%t accepted=%t, want=%t/%t", transport.firstRejected, transport.failoverAccepted, test.wantRejected, test.wantAccepted)
			}
			if (transport.streamRejections["first"] > 0) != test.wantRejected {
				t.Fatal("HTTP200 rejection metadata not distinguished from status")
			}
		})
	}
}

type zaiLiveCall struct {
	name, taskName, forkTurns, target string
}

type zaiLiveTransport struct {
	mu               sync.Mutex
	calls            map[string]zaiLiveCall
	parseFailed      bool
	beforeRequest    func(*http.Request)
	onCompleted      func()
	accounts         []zaiprov.Account
	forcedFirst      string
	statuses         map[string][]int
	streamRejections map[string]int
	firstRejected    bool
	failoverAccepted bool
	unmatchedAccount bool
}

func (transport *zaiLiveTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.beforeRequest != nil {
		transport.beforeRequest(request)
	}
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, errors.New("live Z.ai upstream transport failed")
	}
	id := ""
	if transport.forcedFirst != "" {
		for _, account := range transport.accounts {
			if request.Header.Get("Authorization") == "Bearer "+account.APIKey {
				id = account.ID
				break
			}
		}
		transport.observeStatus(id, response.StatusCode)
	}
	response.Body = &zaiLiveResponseObserver{body: response.Body, parser: NewCodexSSEParser(1 << 20), transport: transport, accountID: id, status: response.StatusCode}
	return response, nil
}

func (transport *zaiLiveTransport) observeStatus(id string, status int) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if id == "" {
		transport.unmatchedAccount = true
		return
	}
	transport.statuses[id] = append(transport.statuses[id], status)
	if id == transport.forcedFirst && (status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests) {
		transport.firstRejected = true
	}
}

// Qualification observes the first upstream event independently of the proxy's
// retry classifier. A successful HTTP status alone does not prove acceptance.
func (transport *zaiLiveTransport) observeInitialEvent(id string, status int, data []byte) {
	if id == "" || status < 200 || status >= 300 {
		return
	}
	var event struct {
		Type  string `json:"type"`
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Response struct {
			Status string `json:"status"`
			Error  struct {
				Code string `json:"code"`
			} `json:"error"`
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if validateZAIRejectionAuthority(data) != nil || validateCodexLifecycleAuthority(data) != nil || json.Unmarshal(data, &event) != nil {
		return
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	code := ""
	if event.Type == "response.failed" && event.Response.Status == "failed" && len(event.Response.Output) == 0 {
		code = event.Response.Error.Code
	} else if event.Type == "error" {
		code = event.Code
		if code == "" {
			code = event.Error.Code
		}
	}
	if code == "rate_limit_exceeded" || code == "insufficient_quota" {
		if transport.streamRejections == nil {
			transport.streamRejections = make(map[string]int)
		}
		transport.streamRejections[id]++
		if id == transport.forcedFirst {
			transport.firstRejected = true
		}
		return
	}
	switch event.Type {
	case "response.created", "response.in_progress", "response.completed", "response.output_item.added", "response.output_text.delta":
		if id != transport.forcedFirst && transport.firstRejected {
			transport.failoverAccepted = true
		}
	}
}

func (transport *zaiLiveTransport) assertFailover(t *testing.T) {
	t.Helper()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if !transport.firstRejected || !transport.failoverAccepted || transport.unmatchedAccount {
		t.Fatalf("native Z.ai failover evidence incomplete (first_rejected=%t, other_accepted=%t, unmatched=%t)", transport.firstRejected, transport.failoverAccepted, transport.unmatchedAccount)
	}
	t.Log("native Z.ai forced-first rejection and different-account acceptance passed")
}

func (transport *zaiLiveTransport) logAccountStatuses(t *testing.T) {
	t.Helper()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	t.Logf("native Z.ai HTTP statuses by opaque account ID: %v; initial SSE rejections: %v", transport.statuses, transport.streamRejections)
}

type zaiLiveResponseObserver struct {
	body           io.ReadCloser
	parser         *CodexSSEParser
	transport      *zaiLiveTransport
	accountID      string
	status         int
	firstEventSeen bool
}

func (observer *zaiLiveResponseObserver) Read(buffer []byte) (int, error) {
	n, readErr := observer.body.Read(buffer)
	events, parseErr := observer.parser.Feed(buffer[:n])
	for _, event := range events {
		if !observer.firstEventSeen {
			observer.firstEventSeen = true
			if observer.transport.forcedFirst != "" && event.ParseError == nil {
				observer.transport.observeInitialEvent(observer.accountID, observer.status, event.Data)
			}
		}
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
		Type     string          `json:"type"`
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
	if event.Type == "response.completed" && transport.onCompleted != nil {
		transport.onCompleted()
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
