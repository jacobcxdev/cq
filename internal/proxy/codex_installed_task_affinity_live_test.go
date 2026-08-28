package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
	"golang.org/x/sys/unix"
)

func TestCodexInstalledSupervisorSupportsRemoteCompaction(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE") != "1" {
		t.Skip("installed Codex supervisor acceptance requires explicit opt-in")
	}
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("open authoritative validation runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Errorf("close authoritative validation runtime: %v", err)
		}
	})
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	core.continuity.Store().mu.Lock()
	core.continuity.Store().modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1, 149, 151}}
	core.continuity.Store().mu.Unlock()
	httpPlanner := &CodexHTTPRequestPlanFactory{
		Inventory: core.inventory, Capacity: core.capacity, Routes: core.continuity, Runtime: core.leaseRuntime,
		DefaultAccountKey: codexInstalledHTTPValidationDefault,
		Authority: CodexLeaseAuthorityPolicy{
			ModeEpoch: 151, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{149},
		},
		Headroom: codexInstalledHTTPValidationHeadroom{}, HeadroomMode: HeadroomModeToken,
		TransportKind: "http",
		Now:           time.Now,
	}
	httpHandler, err := NewCodexNativeHTTPHandler(httpPlanner, &CodexHTTPRequestSession{
		Executor: &CodexAttemptExecutor{
			Inventory: core.inventory,
			Secrets:   core.inventory,
			Transport: &CodexTokenTransport{Inner: newCodexInstalledHTTPValidationRoundTripper(core.upstream.address)},
		},
		Refresher: core.inventory,
		Capacity:  core.capacity,
	}, "http://"+core.upstream.address)
	if err != nil {
		t.Fatalf("construct HTTP acceptance handler: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := httpHandler.CloseAndDrain(ctx); err != nil {
			t.Errorf("close HTTP acceptance handler: %v", err)
		}
	})
	server := &Server{
		Config: &Config{
			LocalToken:     localToken,
			ClaudeUpstream: "http://" + core.upstream.address,
			CodexUpstream:  "http://" + core.upstream.address,
		},
		CodexRouting: &CodexRoutingRuntime{
			HTTP:      CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 149, AuthoritativeEpoch: 149},
			WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 151, AuthoritativeEpoch: 151},
		},
		CodexNativeHTTP: httpHandler,
		Catalog:         modelregistry.NewCatalog(modelregistry.Snapshot{}),
	}
	traffic := &codexInstalledWebSocketTraffic{}
	webSocketUpstream, webSocketServer, webSocketErrors, err := startCodexAcceptanceHTTP(http.HandlerFunc(traffic.serveUpstream))
	if err != nil {
		t.Fatalf("start WebSocket acceptance upstream: %v", err)
	}
	t.Cleanup(func() { shutdownCodexAcceptanceServer(webSocketServer) })
	webSocketPlanner := &CodexHTTPRequestPlanFactory{
		Inventory: core.inventory, Capacity: core.capacity, Routes: core.continuity, Runtime: core.leaseRuntime,
		DefaultAccountKey: codexInstalledHTTPValidationDefault,
		Authority: CodexLeaseAuthorityPolicy{
			ModeEpoch: 151, Authoritative: true, RetainedAuthoritativeEpochs: []uint64{149},
		},
		TransportKind: "websocket",
		Now:           time.Now,
	}
	webSocketExecutor := NewCodexWebSocketAttemptExecutor(core.inventory, core.inventory)
	webSocketExecutor.Dialer.Proxy = nil
	server.CodexWebSocketBroker, err = NewCodexTerminatingWebSocketHandler(webSocketPlanner, webSocketExecutor, "http://"+webSocketUpstream.Addr().String())
	if err != nil {
		t.Fatalf("construct WebSocket acceptance handler: %v", err)
	}
	handler, err := server.RuntimeHandler()
	if err != nil {
		t.Fatalf("construct candidate runtime handler: %v", err)
	}
	listener, _ := newCodexRuntimeSupervisorAcceptanceServerWithCredential(t, handler, NormalCallerCredentialV1{
		Domain: NormalCallerCodex, Bearer: localToken, SubjectID: "validation-codex",
		ValidUntil: time.Now().Add(time.Hour),
	})

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
	runner := codexTaskAffinityAcceptanceRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptanceTurnForTransport(ctx, runner, clientProof, listener.URL, isolation, false, true, "PONG"); err != nil {
		t.Fatalf("first Codex WebSocket turn: %v", err)
	}
	core.upstream.mu.Lock()
	responsesAfterWebSocket, compactAfterWebSocket := core.upstream.responses, core.upstream.compact
	core.upstream.mu.Unlock()
	if responsesAfterWebSocket != 0 || compactAfterWebSocket != 0 {
		t.Fatalf("HTTP traffic after WebSocket turn = responses %d compact %d, want 0/0", responsesAfterWebSocket, compactAfterWebSocket)
	}
	if err := runCodexCompactionAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "Reply with exactly PONG and no other text.", "PONG"); err != nil {
		t.Fatalf("resumed Codex HTTP turn: %v", err)
	}
	core.upstream.mu.Lock()
	responsesAfterResume, compactAfterResume := core.upstream.responses, core.upstream.compact
	core.upstream.mu.Unlock()
	if responsesAfterResume != 1 || compactAfterResume != 0 {
		t.Fatalf("HTTP traffic after resumed turn = responses %d compact %d, want 1/0", responsesAfterResume, compactAfterResume)
	}
	if err := runCodexCompactionAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "Reply with exactly PONG and no other text.", "PONG"); err != nil {
		t.Fatalf("resumed Codex HTTP compaction turn: %v", err)
	}
	shutdownCodexAcceptanceServer(webSocketServer)
	if err := codexAcceptanceServeError(webSocketErrors); err != nil {
		t.Fatalf("WebSocket acceptance upstream: %v", err)
	}
	core.upstream.mu.Lock()
	responses, compact := core.upstream.responses, core.upstream.compact
	core.upstream.mu.Unlock()
	if traffic.webSocketRequests.Load() == 0 || responses != 2 || compact != 1 {
		t.Fatalf("installed Codex cross-transport requests = WebSocket %d responses %d compact %d, want positive/2/1", traffic.webSocketRequests.Load(), responses, compact)
	}
}

func TestCodexInstalledRescuePassesThroughCurrentClient(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE") != "1" {
		t.Skip("installed Codex rescue acceptance requires explicit opt-in")
	}
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	upstreamToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	workerHandler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "normal worker used during rescue", http.StatusInternalServerError)
	})
	listener, supervisor := newCodexRuntimeSupervisorAcceptanceServer(t, workerHandler, localToken)

	var upstreamMu sync.Mutex
	upstreamModels := 0
	upstreamResponses := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+upstreamToken {
			http.Error(writer, "unexpected authentication", http.StatusUnauthorized)
			return
		}
		upstreamMu.Lock()
		defer upstreamMu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/backend-api/codex/models":
			upstreamModels++
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"models":[]}`)
		case request.Method == http.MethodPost && request.URL.Path == "/backend-api/codex/responses":
			upstreamResponses++
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header()["X-Codex-Secondary-Reset-At"] = []string{""}
			_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"rescue-validation-response\"}}\n\n")
			_, _ = io.WriteString(writer, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"rescue-validation-message\",\"content\":[{\"type\":\"output_text\",\"text\":\"PONG\"}]}}\n\n")
			_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"rescue-validation-response\",\"end_turn\":true,\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	relay := &RescueRelay{
		Transport: &codexRescueAcceptanceTransport{target: target, inner: http.DefaultTransport},
		Origin:    origin,
	}
	if err := supervisor.ConfigureRescue(context.Background(), relay, &runtimeEvidenceTestStore{}); err != nil {
		t.Fatal(err)
	}
	enter, err := http.NewRequest(http.MethodPost, listener.URL+RuntimeRescueEnterPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	enter.Header.Set("Authorization", "Bearer "+localToken)
	enterResponse, err := http.DefaultClient.Do(enter)
	if err != nil {
		t.Fatal(err)
	}
	_ = enterResponse.Body.Close()
	if enterResponse.StatusCode != http.StatusOK {
		t.Fatalf("enter rescue = %d mode %q", enterResponse.StatusCode, supervisor.TrafficMode())
	}
	waitForRuntimeMode(t, supervisor, TrafficModeRescue)

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, upstreamToken)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, codexTaskAffinityAcceptanceRunner{}, clientProof, listener.URL, isolation, false, "PONG"); err != nil {
		t.Fatalf("Codex rescue turn: %v", err)
	}
	upstreamMu.Lock()
	models := upstreamModels
	responses := upstreamResponses
	upstreamMu.Unlock()
	if models == 0 || responses != 1 {
		t.Fatalf("rescue upstream calls = models %d responses %d", models, responses)
	}
}

func TestCodexInstalledRescueTaskResumesInNormal(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE") != "1" {
		t.Skip("installed Codex rescue handoff acceptance requires explicit opt-in")
	}
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("open authoritative validation runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Errorf("close authoritative validation runtime: %v", err)
		}
	})
	controlToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	const callerToken = "validation-token-a"
	server := &Server{
		Config: &Config{
			LocalToken:     controlToken,
			ClaudeUpstream: "http://" + core.upstream.address,
			CodexUpstream:  "http://" + core.upstream.address,
		},
		CodexRouting: &CodexRoutingRuntime{
			HTTP: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 1, AuthoritativeEpoch: 1},
		},
		CodexNativeHTTP: core.nativeHTTPHandler(),
		Catalog:         modelregistry.NewCatalog(modelregistry.Snapshot{}),
	}
	handler, err := server.RuntimeHandler()
	if err != nil {
		t.Fatalf("construct candidate runtime handler: %v", err)
	}
	listener, supervisor := newCodexRuntimeSupervisorAcceptanceServerWithCredentials(t, handler, []NormalCallerCredentialV1{
		{Domain: NormalCallerLocal, Bearer: controlToken, SubjectID: "validation-local"},
		{Domain: NormalCallerCodex, Bearer: callerToken, SubjectID: string(codexInstalledHTTPValidationAccountA) + "\x00validation-candidate-a\x00validation-revision-1"},
	})
	target, err := url.Parse("http://" + core.upstream.address)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if err := supervisor.ConfigureRescue(context.Background(), &RescueRelay{
		Transport: &codexRescueValidationTransport{
			codexRescueAcceptanceTransport: codexRescueAcceptanceTransport{target: target, inner: transport},
		},
		Origin: origin,
	}, &runtimeEvidenceTestStore{}); err != nil {
		t.Fatal(err)
	}
	rescueControlRequest(t, listener.URL, RuntimeRescueEnterPath, controlToken)
	waitForRuntimeMode(t, supervisor, TrafficModeRescue)

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, callerToken)
	runner := codexTaskAffinityAcceptanceRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, false, "PONG"); err != nil {
		t.Fatalf("Codex rescue turn: %v", err)
	}
	rescueControlRequest(t, listener.URL, RuntimeRescueExitPath, controlToken)
	waitForRuntimeMode(t, supervisor, TrafficModeNormal)
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "PONG"); err != nil {
		t.Fatalf("Codex normal resume after rescue: %v", err)
	}
}

func rescueControlRequest(t *testing.T, baseURL, path, token string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("rescue control %s = %d", path, response.StatusCode)
	}
}

func TestCodexInstalledRescuePassesThroughLiveUpstream(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_LIVE_UPSTREAM_ACCEPTANCE") != "1" {
		t.Skip("live Codex rescue acceptance requires explicit opt-in")
	}
	authPath := os.Getenv("CQ_CODEX_LIVE_AUTH_FILE")
	if !filepath.IsAbs(authPath) {
		t.Fatal("live Codex auth file must be absolute")
	}
	auth, err := os.ReadFile(authPath)
	if err != nil || len(auth) == 0 || len(auth) > 1<<20 || !json.Valid(auth) {
		t.Fatal("live Codex auth file is unavailable or invalid")
	}
	defer clearBytes(auth)
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	workerHandler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "normal worker used during rescue", http.StatusInternalServerError)
	})
	listener, supervisor := newCodexRuntimeSupervisorAcceptanceServer(t, workerHandler, localToken)
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	relay := &RescueRelay{
		Transport: http.DefaultTransport,
		Origin:    origin,
	}
	if err := supervisor.ConfigureRescue(context.Background(), relay, &runtimeEvidenceTestStore{}); err != nil {
		t.Fatal(err)
	}
	enter, err := http.NewRequest(http.MethodPost, listener.URL+RuntimeRescueEnterPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	enter.Header.Set("Authorization", "Bearer "+localToken)
	enterResponse, err := http.DefaultClient.Do(enter)
	if err != nil {
		t.Fatal(err)
	}
	_ = enterResponse.Body.Close()
	if enterResponse.StatusCode != http.StatusOK {
		t.Fatalf("enter rescue = %d mode %q", enterResponse.StatusCode, supervisor.TrafficMode())
	}
	waitForRuntimeMode(t, supervisor, TrafficModeRescue)

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
	isolationAuth := filepath.Join(isolation.codexHome, "auth.json")
	if err := os.WriteFile(isolationAuth, auth, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(isolationAuth, nil, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("clear isolated live Codex auth: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptanceTurnForTransport(ctx, codexTaskAffinityAcceptanceRunner{}, clientProof, listener.URL, isolation, false, true, "PONG"); err != nil {
		t.Fatalf("live Codex rescue turn: %v", err)
	}
}

func TestCodexInstalledNormalPassesThroughLiveUpstream(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_LIVE_UPSTREAM_ACCEPTANCE") != "1" {
		t.Skip("live Codex normal acceptance requires explicit opt-in")
	}
	authPath := os.Getenv("CQ_CODEX_LIVE_AUTH_FILE")
	credential, err := readCodexLiveAcceptanceCredential(authPath)
	if err != nil {
		t.Fatal(err)
	}
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	listener, supervisor, traffic := newCodexLiveNormalAcceptanceServer(t, credential)
	if listener.URL == "http://127.0.0.1:19280" || supervisor.TrafficMode() != TrafficModeNormal || !supervisor.AdmissionReady() {
		t.Fatal("live Codex normal acceptance did not use isolated normal proxy")
	}

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, credential.localToken)
	runner := codexTaskAffinityAcceptanceRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for _, turn := range []struct {
		resume    bool
		webSocket bool
		output    string
	}{
		{webSocket: true, output: "LIVE-WS-PONG"},
		{resume: true, webSocket: true, output: "LIVE-WS-RESUME-PONG"},
		{resume: true, output: "LIVE-HTTP-PONG"},
	} {
		if err := runCodexTaskAffinityAcceptanceTurnForTransport(ctx, runner, clientProof, listener.URL, isolation, turn.resume, turn.webSocket, turn.output); err != nil {
			t.Fatalf("live Codex normal turn %s: %v", turn.output, err)
		}
	}
	compactionIsolation := newCodexTaskAffinityAcceptanceIsolation(t, credential.localToken)
	if err := runCodexAppServerCompactionAcceptance(ctx, clientProof, listener.URL, compactionIsolation); err != nil {
		t.Fatalf("live Codex normal compaction: %v", err)
	}
	if supervisor.TrafficMode() != TrafficModeNormal || !supervisor.AdmissionReady() {
		t.Fatal("live Codex normal acceptance lost candidate worker")
	}
	webSockets, responses, compactions := traffic.snapshot()
	if webSockets != 2 || responses != 2 || compactions < 1 {
		t.Fatalf("live Codex normal traffic = websocket %d responses %d compactions %d, want 2/2/positive", webSockets, responses, compactions)
	}
}

func TestCodexInstalledNormalContinuesAfterLiveToolCall(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_LIVE_UPSTREAM_ACCEPTANCE") != "1" {
		t.Skip("live Codex normal tool continuation acceptance requires explicit opt-in")
	}
	authPath := os.Getenv("CQ_CODEX_LIVE_AUTH_FILE")
	credential, err := readCodexLiveAcceptanceCredential(authPath)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := os.ReadFile(authPath)
	if err != nil || len(auth) == 0 || len(auth) > 1<<20 || !json.Valid(auth) {
		t.Fatal("live Codex auth file is unavailable or invalid")
	}
	defer clearBytes(auth)
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	listener, supervisor, traffic, inventory, callerRotation := newCodexLiveNormalAcceptanceServerWithInventory(t, credential, []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: credential.material.AccessToken,
		SubjectID:  string(codexLiveNormalAccount) + "\x00live-normal-candidate\x00live-normal-revision",
		ValidUntil: time.Now().Add(10 * time.Minute),
	}})
	if listener.URL == "http://127.0.0.1:19280" || supervisor.TrafficMode() != TrafficModeNormal || !supervisor.AdmissionReady() {
		t.Fatal("live Codex tool acceptance did not use isolated normal proxy")
	}

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, credential.material.AccessToken)
	if err := os.WriteFile(filepath.Join(isolation.codexHome, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	const output = "LIVE-TOOL-PONG"
	messagePath := filepath.Join(isolation.work, "message.txt")
	if err := unix.Mkfifo(messagePath, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rotationDone := make(chan error, 1)
	go func() {
		for {
			fd, openErr := unix.Open(messagePath, unix.O_WRONLY|unix.O_NONBLOCK, 0)
			if openErr == nil {
				rotationErr := callerRotation.rotate([]NormalCallerCredentialV1{{
					Domain: NormalCallerCodex, Bearer: "live-normal-rotated-bearer",
					SubjectID:  string(codexLiveNormalAccount) + "\x00live-normal-candidate\x00live-normal-revision-after-tool-call",
					ValidUntil: time.Now().Add(10 * time.Minute),
				}})
				inventory.setRevision("live-normal-revision-after-tool-call")
				_, writeErr := unix.Write(fd, []byte(output+"\n"))
				closeErr := unix.Close(fd)
				rotationDone <- errors.Join(rotationErr, writeErr, closeErr)
				return
			}
			if !errors.Is(openErr, unix.ENXIO) {
				rotationDone <- openErr
				return
			}
			select {
			case <-ctx.Done():
				rotationDone <- ctx.Err()
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	prompt := "Use the shell to read `message.txt`, then reply with only its contents and no other text."
	if err := runCodexTaskAffinityAcceptancePromptForTransport(ctx, codexReadOnlyTaskAffinityAcceptanceRunner{}, clientProof, listener.URL, isolation, false, true, true, prompt, output); err != nil {
		actual, _ := os.ReadFile(filepath.Join(isolation.root, strings.ToLower(output)+".txt"))
		actual = actual[:min(len(actual), 256)]
		t.Fatalf("live Codex tool continuation: %v; bounded output %q", err, strings.TrimSpace(string(actual)))
	}
	if err := <-rotationDone; err != nil {
		t.Fatalf("rotate live Codex credential revision: %v", err)
	}
	if err := runCodexTaskAffinityAcceptanceTurnForTransport(ctx, codexReadOnlyTaskAffinityAcceptanceRunner{}, clientProof, listener.URL, isolation, true, false, "LIVE-ROTATED-CALLER-PONG"); err != nil {
		t.Fatalf("live Codex HTTP resume after caller rotation: %v", err)
	}
	if supervisor.TrafficMode() != TrafficModeNormal || !supervisor.AdmissionReady() {
		t.Fatal("live Codex tool continuation lost candidate worker")
	}
	webSockets, responses, compactions := traffic.snapshot()
	if webSockets != 1 || responses != 1 || compactions != 0 {
		t.Fatalf("live Codex tool traffic = websocket %d responses %d compactions %d, want 1/1/0", webSockets, responses, compactions)
	}
}

func TestCodexInstalledLiveRescueTaskResumesInNormal(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_LIVE_UPSTREAM_ACCEPTANCE") != "1" {
		t.Skip("live Codex rescue handoff acceptance requires explicit opt-in")
	}
	authPath := os.Getenv("CQ_CODEX_LIVE_AUTH_FILE")
	credential, err := readCodexLiveAcceptanceCredential(authPath)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := os.ReadFile(authPath)
	if err != nil || len(auth) == 0 || len(auth) > 1<<20 || !json.Valid(auth) {
		t.Fatal("live Codex auth file is unavailable or invalid")
	}
	defer clearBytes(auth)
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	now := time.Now()
	listener, supervisor, _ := newCodexLiveNormalAcceptanceServerWithCallers(t, credential, []NormalCallerCredentialV1{
		{Domain: NormalCallerLocal, Bearer: credential.localToken, SubjectID: "live-control"},
		{
			Domain: NormalCallerCodex, Bearer: credential.material.AccessToken,
			SubjectID:  string(codexLiveNormalAccount) + "\x00live-normal-candidate\x00live-normal-revision",
			ValidUntil: now.Add(10 * time.Minute),
		},
	})
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if err := supervisor.ConfigureRescue(context.Background(), &RescueRelay{Transport: transport, Origin: origin}, &runtimeEvidenceTestStore{}); err != nil {
		t.Fatal(err)
	}
	rescueControlRequest(t, listener.URL, RuntimeRescueEnterPath, credential.localToken)
	waitForRuntimeMode(t, supervisor, TrafficModeRescue)

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, credential.material.AccessToken)
	isolationAuth := filepath.Join(isolation.codexHome, "auth.json")
	if err := os.WriteFile(isolationAuth, auth, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runner := codexTaskAffinityAcceptanceRunner{}
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, false, "LIVE-RESCUE-PONG"); err != nil {
		t.Fatalf("live Codex rescue turn: %v", err)
	}
	rescueControlRequest(t, listener.URL, RuntimeRescueExitPath, credential.localToken)
	waitForRuntimeMode(t, supervisor, TrafficModeNormal)
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "LIVE-NORMAL-PONG"); err != nil {
		t.Fatalf("live Codex normal resume after rescue: %v", err)
	}
}

type codexRescueAcceptanceTransport struct {
	target *url.URL
	inner  http.RoundTripper
}

type codexRescueValidationTransport struct {
	codexRescueAcceptanceTransport
}

func (transport *codexRescueValidationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL = new(url.URL)
	*clone.URL = *request.URL
	clone.URL.Path = strings.TrimPrefix(clone.URL.Path, "/backend-api/codex")
	clone.Header.Set("ChatGPT-Account-ID", "validation-upstream-a")
	return transport.codexRescueAcceptanceTransport.RoundTrip(clone)
}

func (transport *codexRescueAcceptanceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL = new(url.URL)
	*clone.URL = *request.URL
	clone.URL.Scheme = transport.target.Scheme
	clone.URL.Host = transport.target.Host
	clone.Host = transport.target.Host
	return transport.inner.RoundTrip(clone)
}

type codexRuntimeSupervisorAcceptanceWorker struct {
	holder  LifecycleHolderProof
	handler http.Handler
	exited  chan struct{}
}

type codexRuntimeSupervisorAcceptanceRefreshingWorker struct {
	*codexRuntimeSupervisorAcceptanceWorker
	callerState *runtimeCallerCredentialState
}

func (worker *codexRuntimeSupervisorAcceptanceRefreshingWorker) CallerIndex(ctx context.Context) (NormalCallerIndexV1, error) {
	if worker == nil || worker.callerState == nil {
		return NormalCallerIndexV1{}, ErrNormalCallerAuthUnavailable
	}
	_, index, err := worker.callerState.snapshot(ctx)
	return index, err
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) Boot(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error) {
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.holder}, nil
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) BeginDrain(context.Context, TrafficMode, uint64) error {
	return nil
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) AwaitQuiescence(context.Context, uint64) (RuntimeQuiescenceAckV1, error) {
	return RuntimeQuiescenceAckV1{SchemaVersion: 1, Quiescent: true}, nil
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) StopAndReap(context.Context) (RuntimeWorkerReleaseV1, error) {
	return RuntimeWorkerReleaseV1{
		ProcessIdentityDigest:         "validation-process",
		ProcessTreeAbsenceProofDigest: "validation-absence",
		HolderReleaseProofDigest:      "validation-release",
	}, nil
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) HolderProof() LifecycleHolderProof {
	return worker.holder
}
func (worker *codexRuntimeSupervisorAcceptanceWorker) Exited() <-chan struct{} { return worker.exited }
func (worker *codexRuntimeSupervisorAcceptanceWorker) ExecuteHTTP(context.Context, RuntimeHTTPRequestV1) (RuntimeHTTPResponseV1, error) {
	return RuntimeHTTPResponseV1{}, ErrRuntimeWorkerUnavailable
}
func (worker *codexRuntimeSupervisorAcceptanceWorker) ServeHTTP(writer http.ResponseWriter, request *http.Request, caller RuntimeCallerAuthorityV1) error {
	worker.handler.ServeHTTP(writer, request.WithContext(withRuntimeCallerAuthority(request.Context(), caller)))
	return nil
}

type codexRuntimeSupervisorAcceptanceLauncher struct {
	worker RuntimeWorkerProcess
}

func (launcher *codexRuntimeSupervisorAcceptanceLauncher) Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error) {
	return launcher.worker, nil
}

type codexRuntimeSupervisorAcceptanceConsumer struct {
	mu       sync.Mutex
	consumed map[string]struct{}
}

type codexRuntimeSupervisorAcceptanceCallerRotation struct {
	mu          sync.RWMutex
	key         []byte
	credentials []NormalCallerCredentialV1
}

func (rotation *codexRuntimeSupervisorAcceptanceCallerRotation) source(ctx context.Context) ([]NormalCallerCredentialV1, error) {
	if ctx == nil {
		return nil, ErrNormalCallerAuthUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rotation.mu.RLock()
	defer rotation.mu.RUnlock()
	return append([]NormalCallerCredentialV1(nil), rotation.credentials...), nil
}

func (rotation *codexRuntimeSupervisorAcceptanceCallerRotation) rotate(credentials []NormalCallerCredentialV1) error {
	if rotation == nil || len(credentials) == 0 {
		return ErrNormalCallerAuthUnavailable
	}
	rotation.mu.RLock()
	key := append([]byte(nil), rotation.key...)
	rotation.mu.RUnlock()
	if _, err := bindRuntimeCallerCredentials(key, credentials); err != nil {
		return err
	}
	rotation.mu.Lock()
	rotation.credentials = append(rotation.credentials[:0], credentials...)
	rotation.mu.Unlock()
	return nil
}

func (consumer *codexRuntimeSupervisorAcceptanceConsumer) Consume(_ context.Context, consumption ProviderBranchAdmissionConsumptionV1) error {
	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if _, exists := consumer.consumed[consumption.AdmissionID]; exists {
		return ErrNormalCallerAdmissionReplayed
	}
	consumer.consumed[consumption.AdmissionID] = struct{}{}
	return nil
}

func newCodexRuntimeSupervisorAcceptanceServer(t *testing.T, handler http.Handler, localToken string) (*httptest.Server, *RuntimeSupervisor) {
	t.Helper()
	credential := NormalCallerCredentialV1{Domain: NormalCallerLocal, Bearer: localToken, SubjectID: "validation-local"}
	return newCodexRuntimeSupervisorAcceptanceServerWithCredential(t, handler, credential)
}

func newCodexRuntimeSupervisorAcceptanceServerWithCredential(t *testing.T, handler http.Handler, credential NormalCallerCredentialV1) (*httptest.Server, *RuntimeSupervisor) {
	return newCodexRuntimeSupervisorAcceptanceServerWithCredentials(t, handler, []NormalCallerCredentialV1{credential})
}

func newCodexRuntimeSupervisorAcceptanceServerWithCredentials(t *testing.T, handler http.Handler, credentials []NormalCallerCredentialV1) (*httptest.Server, *RuntimeSupervisor) {
	listener, supervisor, _ := newCodexRuntimeSupervisorAcceptanceServerWithRotatingCredentials(t, handler, credentials)
	return listener, supervisor
}

func newCodexRuntimeSupervisorAcceptanceServerWithRotatingCredentials(t *testing.T, handler http.Handler, credentials []NormalCallerCredentialV1) (*httptest.Server, *RuntimeSupervisor, *codexRuntimeSupervisorAcceptanceCallerRotation) {
	t.Helper()
	if len(credentials) == 0 {
		t.Fatal("Codex acceptance caller credentials are empty")
	}
	key := sha256.Sum256([]byte(credentials[0].Bearer))
	rotation := &codexRuntimeSupervisorAcceptanceCallerRotation{
		key:         append([]byte(nil), key[:]...),
		credentials: append([]NormalCallerCredentialV1(nil), credentials...),
	}
	callerState, err := newRuntimeCallerCredentialState(key[:], rotation.source)
	if err != nil {
		t.Fatal(err)
	}
	boundCredentials, err := bindRuntimeCallerCredentials(key[:], credentials)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	worker := &codexRuntimeSupervisorAcceptanceRefreshingWorker{codexRuntimeSupervisorAcceptanceWorker: &codexRuntimeSupervisorAcceptanceWorker{
		holder:  runtimeHolder("validation-worker"),
		handler: normalWorkerHandlerWithSource(handler, callerState.credentials),
		exited:  make(chan struct{}),
	}, callerState: callerState}
	supervisor, err := NewRuntimeSupervisor(
		&runtimeTestListener{},
		runtimeHolder("validation-supervisor"),
		&codexRuntimeSupervisorAcceptanceLauncher{worker: worker},
		&runtimeTestCheckpointStore{events: &events},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "validation-artifact"}); err != nil {
		t.Fatal(err)
	}
	authority, err := NewNormalCallerAuthority(
		key[:],
		1,
		boundCredentials,
		&codexRuntimeSupervisorAcceptanceConsumer{consumed: make(map[string]struct{})},
		time.Now,
		rand.Reader,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAuthority(authority); err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(supervisor)
	listenerURL, err := url.Parse(listener.URL)
	if err != nil || listenerURL.Port() == "" || listenerURL.Port() == "19280" {
		listener.Close()
		t.Fatal("Codex acceptance listener did not use an isolated alternate port")
	}
	t.Cleanup(listener.Close)
	return listener, supervisor, rotation
}

func TestCodexInstalledTaskAffinityUsesHardLimitOnlyFailover(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE") != "1" {
		t.Skip("installed Codex task-affinity acceptance requires explicit opt-in")
	}
	clientPath, err := resolveCodexInstalledClientExecutable()
	if err != nil {
		t.Skip("installed Codex client unavailable")
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("open authoritative validation runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Errorf("close authoritative validation runtime: %v", err)
		}
	})
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Config: &Config{
			LocalToken:     localToken,
			ClaudeUpstream: "http://" + core.upstream.address,
			CodexUpstream:  "http://" + core.upstream.address,
		},
		CodexNativeHTTP: core.nativeHTTPHandler(),
		Catalog:         modelregistry.NewCatalog(modelregistry.Snapshot{}),
	}
	handler, err := server.handler()
	if err != nil {
		t.Fatalf("construct candidate handler: %v", err)
	}
	listener := httptest.NewServer(handler)
	t.Cleanup(listener.Close)

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
	runner := codexTaskAffinityAcceptanceRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, false, "PONG"); err != nil {
		t.Fatalf("first Codex task turn: %v", err)
	}
	if err := core.upstream.armNextNewTurnHardReplay(); err != nil {
		t.Fatalf("arm hard-limit replay: %v", err)
	}
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "PONG"); err != nil {
		t.Fatalf("second Codex task turn: %v", err)
	}
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "PONG"); err != nil {
		t.Fatalf("third Codex task turn: %v", err)
	}

	routes := core.upstream.routeHistory()
	if len(routes) != 4 {
		t.Fatalf("upstream route count = %d, want 4", len(routes))
	}
	if routes[0].AccountID != "validation-upstream-a" || routes[0].Status != 200 ||
		routes[1].AccountID != "validation-upstream-a" || routes[1].Status != 429 ||
		routes[2].AccountID != "validation-upstream-b" || routes[2].Status != 200 ||
		routes[3].AccountID != "validation-upstream-b" || routes[3].Status != 200 {
		t.Fatal("upstream route sequence did not match A/200 A/429 B/200 B/200")
	}
	sessions := []string{routes[0].Metadata.SessionID, routes[1].Metadata.SessionID, routes[2].Metadata.SessionID, routes[3].Metadata.SessionID}
	threads := []string{routes[0].Metadata.ThreadID, routes[1].Metadata.ThreadID, routes[2].Metadata.ThreadID, routes[3].Metadata.ThreadID}
	if !allCodexTaskAffinityValuesEqual(sessions) || !allCodexTaskAffinityValuesEqual(threads) {
		t.Fatal("resumed task session or thread identity changed")
	}
	turns := []string{routes[0].Metadata.TurnID, routes[1].Metadata.TurnID, routes[2].Metadata.TurnID, routes[3].Metadata.TurnID}
	if turns[0] == turns[1] || turns[1] != turns[2] || turns[2] == turns[3] {
		t.Fatal("turn identity sequence did not preserve failover retry identity")
	}
}

type codexTaskAffinityAcceptanceRunner struct{}

type codexAcceptanceDiagnosticBuffer struct {
	data []byte
}

func TestCodexAcceptanceDiagnosticsAreBoundedAndRedacted(t *testing.T) {
	buffer := &codexAcceptanceDiagnosticBuffer{}
	input := make([]byte, 20<<10)
	if written, err := buffer.Write(input); err != nil || written != len(input) || len(buffer.data) != 16<<10 {
		t.Fatalf("diagnostic write = %d/%d bytes, error %v", written, len(buffer.data), err)
	}
	diagnostic := sanitiseCodexAcceptanceDiagnostic("safe detail\nAuthorization: secret\nBearer secret\naccess_token=secret")
	if !strings.Contains(diagnostic, "safe detail") || strings.Contains(diagnostic, "secret") || strings.Count(diagnostic, "[redacted credential diagnostic]") != 3 {
		t.Fatalf("diagnostic was not safely sanitised: %q", diagnostic)
	}
}

func (buffer *codexAcceptanceDiagnosticBuffer) Write(value []byte) (int, error) {
	const limit = 16 << 10
	remaining := limit - len(buffer.data)
	if remaining > 0 {
		buffer.data = append(buffer.data, value[:min(len(value), remaining)]...)
	}
	return len(value), nil
}

func resolveCodexAcceptanceClientExecutable() (string, error) {
	path := os.Getenv("CQ_CODEX_ACCEPTANCE_EXECUTABLE")
	if path == "" {
		return resolveCodexInstalledClientExecutable()
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("Codex acceptance executable must be absolute")
	}
	return path, nil
}

func sanitiseCodexAcceptanceDiagnostic(value string) string {
	lines := strings.Split(value, "\n")
	kept := lines[:0]
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "authorization") || strings.Contains(lower, "bearer") || strings.Contains(lower, "token") {
			kept = append(kept, "[redacted credential diagnostic]")
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func (runner codexTaskAffinityAcceptanceRunner) Run(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
	if runtime.GOOS != "darwin" || command.executable == "" || !command.expectedExecutable.valid() || !command.loopbackOnly {
		return nil, errors.New("Codex task-affinity runner unavailable")
	}
	before, err := captureCodexInstalledExecutable(command.executable)
	if err != nil || before != command.expectedExecutable {
		return nil, errors.New("Codex task-affinity executable changed")
	}
	profile, err := codexAcceptanceSandboxProfile(command)
	if err != nil {
		return nil, errors.New("Codex task-affinity sandbox unavailable")
	}
	arguments := append([]string{"-p", profile, command.executable}, command.args...)
	child := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", arguments...)
	child.Env = append([]string(nil), command.env...)
	child.Dir = command.dir
	child.Stdin = strings.NewReader("")
	child.Stdout = io.Discard
	stderr := &codexAcceptanceDiagnosticBuffer{}
	child.Stderr = stderr
	child.WaitDelay = 2 * time.Second
	if err := child.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Codex task-affinity command timed out")
		}
		diagnostic := sanitiseCodexAcceptanceDiagnostic(string(stderr.data))
		if diagnostic == "" {
			return nil, fmt.Errorf("Codex task-affinity command failed: %w", err)
		}
		return nil, fmt.Errorf("Codex task-affinity command failed: %w: %s", err, diagnostic)
	}
	after, err := captureCodexInstalledExecutable(command.executable)
	if err != nil || after != command.expectedExecutable {
		return nil, errors.New("Codex task-affinity executable changed")
	}
	return append([]byte(nil), stderr.data...), nil
}

type codexReadOnlyTaskAffinityAcceptanceRunner struct{}

func (codexReadOnlyTaskAffinityAcceptanceRunner) Run(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
	if command.executable == "" || !command.expectedExecutable.valid() || !command.loopbackOnly {
		return nil, errors.New("Codex read-only task-affinity runner unavailable")
	}
	if _, err := codexAcceptanceSandboxLoopbackAddress(command.endpoint); err != nil {
		return nil, errors.New("Codex read-only task-affinity endpoint unavailable")
	}
	before, err := captureCodexInstalledExecutable(command.executable)
	if err != nil || before != command.expectedExecutable {
		return nil, errors.New("Codex task-affinity executable changed")
	}
	child := exec.CommandContext(ctx, command.executable, command.args...)
	child.Env = append([]string(nil), command.env...)
	child.Dir = command.dir
	child.Stdin = strings.NewReader("")
	stdout := &codexAcceptanceDiagnosticBuffer{}
	child.Stdout = stdout
	stderr := &codexAcceptanceDiagnosticBuffer{}
	child.Stderr = stderr
	child.WaitDelay = 2 * time.Second
	if err := child.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Codex task-affinity command timed out")
		}
		diagnostic := sanitiseCodexAcceptanceDiagnostic(string(stderr.data))
		if diagnostic == "" {
			return nil, fmt.Errorf("Codex task-affinity command failed: %w", err)
		}
		return nil, fmt.Errorf("Codex task-affinity command failed: %w: %s", err, diagnostic)
	}
	after, err := captureCodexInstalledExecutable(command.executable)
	if err != nil || after != command.expectedExecutable {
		return nil, errors.New("Codex task-affinity executable changed")
	}
	return codexAcceptanceCombinedOutput(stdout.data, stderr.data), nil
}

func codexAcceptanceCombinedOutput(stdout []byte, stderr []byte) []byte {
	output := append([]byte(nil), stdout...)
	if len(output) > 0 && len(stderr) > 0 && output[len(output)-1] != '\n' {
		output = append(output, '\n')
	}
	return append(output, stderr...)
}

type codexTaskAffinityAcceptanceIsolation struct {
	root      string
	home      string
	codexHome string
	work      string
	tmp       string
	cache     string
	config    string
	data      string
}

func newCodexTaskAffinityAcceptanceIsolation(t *testing.T, localToken string) codexTaskAffinityAcceptanceIsolation {
	t.Helper()
	shortTempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(shortTempRoot, "cq-codex-task-affinity-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	isolation := codexTaskAffinityAcceptanceIsolation{
		root:      root,
		home:      filepath.Join(root, "home"),
		codexHome: filepath.Join(root, "codex-home"),
		work:      filepath.Join(root, "work"),
		tmp:       filepath.Join(root, "tmp"),
		cache:     filepath.Join(root, "cache"),
		config:    filepath.Join(root, "config"),
		data:      filepath.Join(root, "data"),
	}
	for _, directory := range []string{isolation.home, isolation.codexHome, isolation.work, isolation.tmp, isolation.cache, isolation.config, isolation.data} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCodexAcceptanceAuthWithToken(filepath.Join(isolation.codexHome, "auth.json"), localToken); err != nil {
		t.Fatal(err)
	}
	return isolation
}

func runCodexTaskAffinityAcceptanceTurn(
	ctx context.Context,
	runner codexAcceptanceRunner,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
	resume bool,
	output string,
) error {
	return runCodexTaskAffinityAcceptanceTurnForTransport(ctx, runner, client, baseURL, isolation, resume, false, output)
}

func runCodexTaskAffinityAcceptanceTurnForTransport(
	ctx context.Context,
	runner codexAcceptanceRunner,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
	resume bool,
	webSocket bool,
	output string,
) error {
	return runCodexTaskAffinityAcceptancePromptForTransport(
		ctx,
		runner,
		client,
		baseURL,
		isolation,
		resume,
		webSocket,
		false,
		"Reply with exactly "+output+" and no other text.",
		output,
	)
}

func runCodexTaskAffinityAcceptancePromptForTransport(
	ctx context.Context,
	runner codexAcceptanceRunner,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
	resume bool,
	webSocket bool,
	allowTools bool,
	prompt string,
	output string,
) error {
	outputPath := filepath.Join(isolation.root, strings.ToLower(output)+".txt")
	args := codexAcceptanceExecArgumentsForTransport(baseURL, isolation.work, outputPath, webSocket)
	if allowTools {
		args = slices.Insert(args, 1, "--json")
		args = slices.Insert(args, len(args)-1, "-c", "features.apps=false")
	}
	args = slices.DeleteFunc(args, func(value string) bool { return value == "--ephemeral" })
	args[len(args)-1] = prompt
	if resume {
		args = slices.Insert(args, 1, "resume", "--last")
		args = removeCodexTaskAffinityUnsupportedResumeArguments(args)
	}
	environment := append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config),
		"XDG_DATA_HOME="+isolation.data,
		"OPENAI_BASE_URL="+baseURL,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	events, err := runner.Run(ctx, codexAcceptanceCommand{
		executable:         client.path,
		expectedExecutable: client,
		args:               args,
		env:                environment,
		dir:                isolation.work,
		endpoint:           baseURL + legacyCodexResponsesPath,
		outputPath:         outputPath,
		sandboxWriteRoot:   isolation.root,
		captureOutput:      allowTools,
		loopbackOnly:       true,
	})
	if err != nil {
		return err
	}
	if webSocket && codexAcceptanceWebSocketTransportFailed(events) {
		return errors.New("Codex task-affinity WebSocket transport fell back or disconnected before completion")
	}
	if allowTools && !codexAcceptanceCompletedCommand(events) {
		return fmt.Errorf("Codex task-affinity shell command did not complete: %s", sanitiseCodexAcceptanceDiagnostic(string(events)))
	}
	result, err := os.ReadFile(outputPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result)) != output {
		return fmt.Errorf("Codex task-affinity output mismatch: %s", sanitiseCodexAcceptanceDiagnostic(string(events)))
	}
	return nil
}

func codexAcceptanceWebSocketTransportFailed(output []byte) bool {
	diagnostic := strings.ToLower(string(output))
	return strings.Contains(diagnostic, "falling back from websockets to https transport") ||
		strings.Contains(diagnostic, "stream disconnected before completion") ||
		strings.Contains(diagnostic, "websocket closed by server before response.completed")
}

func codexAcceptanceCompletedCommand(events []byte) bool {
	for _, line := range strings.Split(string(events), "\n") {
		var event struct {
			Item struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Item.Type == "command_execution" && event.Item.Status == "completed" {
			return true
		}
	}
	return false
}

func TestCodexTaskAffinityWebSocketRejectsFallbackDespiteValidOutput(t *testing.T) {
	for _, diagnostic := range []string{
		"Falling back from WebSockets to HTTPS transport...",
		"stream disconnected before completion: websocket closed by server before response.completed",
	} {
		t.Run(diagnostic, func(t *testing.T) {
			isolation := codexTaskAffinityAcceptanceIsolation{
				root: t.TempDir(),
				work: t.TempDir(),
			}
			runner := testCodexAcceptanceRunner(func(_ context.Context, command codexAcceptanceCommand) ([]byte, error) {
				if err := os.WriteFile(command.outputPath, []byte("PONG\n"), 0o600); err != nil {
					return nil, err
				}
				return []byte(diagnostic), nil
			})

			err := runCodexTaskAffinityAcceptancePromptForTransport(
				context.Background(), runner, codexInstalledExecutableProof{},
				"http://127.0.0.1:29280", isolation, false, true, false,
				"Reply with exactly PONG and no other text.", "PONG",
			)
			if err == nil || !strings.Contains(err.Error(), "WebSocket transport") {
				t.Fatalf("WebSocket fallback result = %v, want transport failure", err)
			}
		})
	}
}

func runCodexCompactionAcceptanceTurn(
	ctx context.Context,
	runner codexAcceptanceRunner,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
	resume bool,
	prompt string,
	output string,
) error {
	outputPath := filepath.Join(isolation.root, "compaction-"+strings.ToLower(output)+".txt")
	args := codexAcceptanceExecArguments(baseURL, isolation.work, outputPath)
	args = slices.DeleteFunc(args, func(value string) bool { return value == "--ephemeral" })
	args = slices.Insert(args, len(args)-1,
		"-c", "model_context_window=4096",
		"-c", "model_auto_compact_token_limit=2048",
	)
	args[len(args)-1] = prompt
	if resume {
		args = slices.Insert(args, 1, "resume", "--last")
		args = removeCodexTaskAffinityUnsupportedResumeArguments(args)
	}
	environment := append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config),
		"XDG_DATA_HOME="+isolation.data,
		"OPENAI_BASE_URL="+baseURL,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	_, err := runner.Run(ctx, codexAcceptanceCommand{
		executable:         client.path,
		expectedExecutable: client,
		args:               args,
		env:                environment,
		dir:                isolation.work,
		endpoint:           baseURL + legacyCodexResponsesPath,
		outputPath:         outputPath,
		sandboxWriteRoot:   isolation.root,
		loopbackOnly:       true,
	})
	if err != nil {
		return err
	}
	result, err := os.ReadFile(outputPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result)) != output {
		return errors.New("Codex compaction output mismatch")
	}
	return nil
}

func removeCodexTaskAffinityUnsupportedResumeArguments(args []string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == "--color" || args[index] == "-s" || args[index] == "-C" {
			index++
			continue
		}
		result = append(result, args[index])
	}
	return result
}

func allCodexTaskAffinityValuesEqual(values []string) bool {
	return len(values) > 0 && values[0] != "" && slices.Equal(values, slices.Repeat([]string{values[0]}, len(values)))
}
