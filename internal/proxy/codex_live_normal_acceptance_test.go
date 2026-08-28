package proxy

import (
	"bytes"
	"context"
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
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexLiveNormalAccount codex.AccountKey = "live-normal-account"

const codexLiveAcceptanceMinimumCredentialLifetime = 15 * time.Minute

type codexLiveAcceptanceCredential struct {
	identity   codex.AccountIdentity
	material   codex.CredentialMaterial
	localToken string
}

type codexLiveNormalTraffic struct {
	webSockets  atomic.Uint64
	responses   atomic.Uint64
	compactions atomic.Uint64
}

type codexLiveRotatingInventory struct {
	mu       sync.RWMutex
	base     *codexInstalledHTTPValidationInventory
	revision codex.Revision
}

func (inventory *codexLiveRotatingInventory) List(ctx context.Context) (codex.Inventory, error) {
	view, err := inventory.base.List(ctx)
	if err != nil {
		return codex.Inventory{}, err
	}
	inventory.mu.RLock()
	revision := inventory.revision
	inventory.mu.RUnlock()
	for accountIndex := range view.Accounts {
		for candidateIndex := range view.Accounts[accountIndex].Candidates {
			view.Accounts[accountIndex].Candidates[candidateIndex].Revision = revision
		}
	}
	return view, nil
}

func (inventory *codexLiveRotatingInventory) ResolveExact(ctx context.Context, planned codex.PlannedCandidate) (codex.CredentialMaterial, error) {
	view, err := inventory.List(ctx)
	if err != nil {
		return codex.CredentialMaterial{}, err
	}
	for _, account := range view.Accounts {
		if account.Key != planned.Ref.AccountKey || account.Identity.AccountID != planned.Identity.AccountID || account.Identity.UserID != planned.Identity.UserID {
			continue
		}
		for _, candidate := range account.Candidates {
			if candidate.Ref != planned.Ref || candidate.Revision != planned.Revision || candidate.Source != planned.Source || !candidate.Routable || candidate.DispatchBlocked {
				continue
			}
			material, ok := inventory.base.material[account.Key]
			if ok {
				return material, nil
			}
		}
	}
	return codex.CredentialMaterial{}, codex.ErrStaleRevision
}

func (inventory *codexLiveRotatingInventory) setRevision(revision codex.Revision) {
	inventory.mu.Lock()
	inventory.revision = revision
	inventory.mu.Unlock()
}

func (traffic *codexLiveNormalTraffic) serveHTTP(next http.Handler, writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == legacyCodexResponsesPath && websocket.IsWebSocketUpgrade(request):
		traffic.webSockets.Add(1)
	case request.Method == http.MethodPost && request.URL.Path == legacyCodexResponsesPath:
		body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody+1))
		_ = request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(body))
		if err == nil && len(body) <= maxRequestBody && codexLiveRequestIsCompaction(body, request.Header) {
			traffic.compactions.Add(1)
		} else {
			traffic.responses.Add(1)
		}
		defer clearBytes(body)
	case request.Method == http.MethodPost && (request.URL.Path == legacyCodexCompactResponsesPath || request.URL.Path == codexCompactResponsesPath):
		traffic.compactions.Add(1)
	}
	next.ServeHTTP(writer, request)
}

func codexLiveRequestIsCompaction(body []byte, header http.Header) bool {
	decoded, err := DecodeCodexRequest(body, header.Get("Content-Encoding"), codexTransportRewriteLimits())
	if err != nil {
		return false
	}
	protocol, err := ParseCodexProtocolRequest(decoded.Decoded(), header.Get(codexTurnMetadataKey), nil)
	return err == nil && protocol.Metadata.Strong && protocol.Metadata.Metadata.RequestKind == CodexRequestCompaction
}

func (traffic *codexLiveNormalTraffic) snapshot() (uint64, uint64, uint64) {
	return traffic.webSockets.Load(), traffic.responses.Load(), traffic.compactions.Load()
}

func readCodexLiveAcceptanceCredential(path string) (codexLiveAcceptanceCredential, error) {
	var result codexLiveAcceptanceCredential
	if !filepath.IsAbs(path) {
		return result, errors.New("live Codex auth file must be absolute")
	}
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return result, errors.New("live Codex auth file is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return result, errors.New("open live Codex auth file")
	}
	body, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(body) == 0 || len(body) > 1<<20 {
		return result, errors.New("read live Codex auth file")
	}
	defer clearBytes(body)
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		return result, errors.New("live Codex auth file changed during read")
	}
	var document struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return result, errors.New("decode live Codex auth file")
	}
	claims := auth.DecodeCodexClaims(document.Tokens.IDToken)
	accessClaims := auth.DecodeCodexClaims(document.Tokens.AccessToken)
	if document.Tokens.AccessToken == "" || document.Tokens.IDToken == "" || claims.AccountID == "" || claims.UserID == "" ||
		(document.Tokens.AccountID != "" && document.Tokens.AccountID != claims.AccountID) ||
		accessClaims.ExpiresAt == 0 || time.Unix(accessClaims.ExpiresAt, 0).Before(time.Now().Add(codexLiveAcceptanceMinimumCredentialLifetime)) {
		return result, errors.New("live Codex auth credential is incomplete or expired")
	}
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		return result, err
	}
	result.identity = codex.AccountIdentity{
		AccountID: claims.AccountID,
		UserID:    claims.UserID,
		Email:     claims.Email,
		PlanType:  claims.PlanType,
		RecordKey: claims.RecordKey(),
	}
	result.material = codex.CredentialMaterial{
		AccessToken: document.Tokens.AccessToken,
		IDToken:     document.Tokens.IDToken,
		AccountID:   claims.AccountID,
	}
	result.localToken = localToken
	return result, nil
}

func newCodexLiveNormalAcceptanceServer(t *testing.T, credential codexLiveAcceptanceCredential) (*httptest.Server, *RuntimeSupervisor, *codexLiveNormalTraffic) {
	return newCodexLiveNormalAcceptanceServerWithCallers(t, credential, []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: credential.localToken,
		SubjectID:  string(codexLiveNormalAccount) + "\x00live-normal-candidate\x00live-normal-revision",
		ValidUntil: time.Now().Add(10 * time.Minute),
	}})
}

func newCodexLiveNormalAcceptanceServerWithCallers(t *testing.T, credential codexLiveAcceptanceCredential, callers []NormalCallerCredentialV1) (*httptest.Server, *RuntimeSupervisor, *codexLiveNormalTraffic) {
	listener, supervisor, traffic, _, _ := newCodexLiveNormalAcceptanceServerWithInventory(t, credential, callers)
	return listener, supervisor, traffic
}

func newCodexLiveNormalAcceptanceServerWithInventory(t *testing.T, credential codexLiveAcceptanceCredential, callers []NormalCallerCredentialV1) (*httptest.Server, *RuntimeSupervisor, *codexLiveNormalTraffic, *codexLiveRotatingInventory, *codexRuntimeSupervisorAcceptanceCallerRotation) {
	t.Helper()
	modelSnapshot, err := codexLiveAcceptanceModelSnapshot(os.Getenv("CQ_CODEX_LIVE_AUTH_FILE"), "gpt-5.6-sol")
	if err != nil {
		t.Fatalf("load live Codex model catalogue: %v", err)
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("open live Codex normal runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Errorf("close live Codex normal runtime: %v", err)
		}
	})
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	if err := core.handler.CloseAndDrain(drainCtx); err != nil {
		cancelDrain()
		t.Fatalf("replace synthetic Codex handler: %v", err)
	}
	cancelDrain()

	now := time.Now()
	candidate := codex.CredentialCandidate{
		Ref:             codex.CandidateRef{AccountKey: codexLiveNormalAccount, CandidateID: "live-normal-candidate"},
		Revision:        "live-normal-revision",
		Source:          codex.SourceExternal,
		AccessExpiresAt: now.Add(time.Hour),
		Routable:        true,
	}
	core.inventory.inventory = codex.Inventory{Accounts: []codex.LogicalAccount{{
		Key:        codexLiveNormalAccount,
		Identity:   credential.identity,
		Candidates: []codex.CredentialCandidate{candidate},
		Routable:   true,
	}}}
	core.inventory.material = map[codex.AccountKey]codex.CredentialMaterial{codexLiveNormalAccount: credential.material}
	inventory := &codexLiveRotatingInventory{base: core.inventory, revision: candidate.Revision}
	stream := core.capacity.NewObservationStream()
	if !core.capacity.Observe(stream.Stamp(CapacityFact{
		AccountKey: codexLiveNormalAccount, Bucket: CapacityBucketBase, RemainingPct: 100,
		Source: CapacitySourceLiveRateLimits, ObservedAt: now, ResetAt: now.Add(time.Hour), Confidence: CapacityConfidenceAuthoritative,
	})) {
		t.Fatal("seed live Codex normal capacity")
	}

	planner := &CodexHTTPRequestPlanFactory{
		Inventory: inventory, Capacity: core.capacity, Routes: core.continuity, Runtime: core.leaseRuntime,
		DefaultAccountKey: codexLiveNormalAccount,
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               time.Now,
	}
	httpHandler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{
		Executor: &CodexAttemptExecutor{
			Inventory: inventory,
			Secrets:   inventory,
			Transport: &CodexTokenTransport{Inner: http.DefaultTransport},
		},
		Refresher: core.inventory,
		Capacity:  core.capacity,
	}, DefaultCodexUpstream)
	if err != nil {
		t.Fatalf("construct live Codex normal HTTP handler: %v", err)
	}
	core.handler = httpHandler

	webSocketExecutor := NewCodexWebSocketAttemptExecutor(inventory, inventory)
	webSocketExecutor.Dialer.Proxy = nil
	webSocketBroker, err := NewCodexTerminatingWebSocketHandler(planner, webSocketExecutor, DefaultCodexUpstream)
	if err != nil {
		t.Fatalf("construct live Codex normal WebSocket handler: %v", err)
	}
	server := &Server{
		Config: &Config{
			LocalToken: credential.localToken, ClaudeUpstream: DefaultUpstream, CodexUpstream: DefaultCodexUpstream,
		},
		CodexRouting: &CodexRoutingRuntime{
			HTTP:      CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 1, AuthoritativeEpoch: 1},
			WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 1, AuthoritativeEpoch: 1},
		},
		CodexNativeHTTP:      httpHandler,
		CodexWebSocketBroker: webSocketBroker,
		Catalog:              modelregistry.NewCatalog(modelSnapshot),
	}
	handler, err := server.RuntimeHandler()
	if err != nil {
		t.Fatalf("construct live Codex normal candidate: %v", err)
	}
	traffic := &codexLiveNormalTraffic{}
	listener, supervisor, callerRotation := newCodexRuntimeSupervisorAcceptanceServerWithRotatingCredentials(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		traffic.serveHTTP(handler, writer, request)
	}), callers)
	return listener, supervisor, traffic, inventory, callerRotation
}

func codexLiveAcceptanceModelSnapshot(authPath, model string) (modelregistry.Snapshot, error) {
	cachePath := filepath.Join(filepath.Dir(authPath), "models_cache.json")
	before, err := os.Lstat(cachePath)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return modelregistry.Snapshot{}, errors.New("live Codex model cache is unavailable")
	}
	file, err := os.Open(cachePath)
	if err != nil {
		return modelregistry.Snapshot{}, errors.New("open live Codex model cache")
	}
	body, readErr := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(body) == 0 || len(body) > 4<<20 {
		return modelregistry.Snapshot{}, errors.New("read live Codex model cache")
	}
	defer clearBytes(body)
	after, err := os.Lstat(cachePath)
	if err != nil || !os.SameFile(before, after) {
		return modelregistry.Snapshot{}, errors.New("live Codex model cache changed during read")
	}
	var envelope struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return modelregistry.Snapshot{}, errors.New("decode live Codex model cache")
	}
	for _, raw := range envelope.Models {
		var info struct {
			Slug             string `json:"slug"`
			DisplayName      string `json:"display_name"`
			Description      string `json:"description"`
			ContextWindow    int    `json:"context_window"`
			MaxContextWindow int    `json:"max_context_window"`
			Priority         int    `json:"priority"`
			Visibility       string `json:"visibility"`
		}
		if json.Unmarshal(raw, &info) != nil || info.Slug != model {
			continue
		}
		return modelregistry.Snapshot{
			Entries: []modelregistry.Entry{{
				Provider: modelregistry.ProviderCodex, ID: info.Slug, DisplayName: info.DisplayName,
				Description: info.Description, ContextWindow: info.ContextWindow, MaxContextWindow: info.MaxContextWindow,
				Priority: info.Priority, Visibility: info.Visibility, Source: modelregistry.SourceNative,
			}},
			CodexRawByID: map[string]json.RawMessage{info.Slug: append(json.RawMessage(nil), raw...)},
			FetchedAt:    time.Now(),
		}, nil
	}
	return modelregistry.Snapshot{}, errors.New("live Codex model is unavailable")
}

func runCodexAppServerContinuityAcceptance(
	ctx context.Context,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
	webSocket bool,
) (returnErr error) {
	const initialText = "LIVE-APP-SERVER-STARTING"
	nonce, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		return errors.New("generate Codex app-server tool input")
	}
	finalText := "LIVE-APP-SERVER-" + nonce[:16]
	if err := os.WriteFile(filepath.Join(isolation.work, "message.txt"), []byte(finalText+"\n"), 0o600); err != nil {
		return errors.New("write Codex app-server tool input")
	}
	command := codexAcceptanceCommand{
		executable:         client.path,
		expectedExecutable: client,
		args:               codexLiveAppServerArguments(baseURL, webSocket),
		env: append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config),
			"XDG_DATA_HOME="+isolation.data,
			"OPENAI_BASE_URL="+baseURL,
			"NO_PROXY=127.0.0.1,localhost",
			"no_proxy=127.0.0.1,localhost",
		),
		dir:              isolation.work,
		endpoint:         baseURL + legacyCodexResponsesPath,
		sandboxWriteRoot: isolation.root,
		loopbackOnly:     true,
	}
	before, err := captureCodexInstalledExecutable(command.executable)
	if err != nil || before != client {
		return errors.New("Codex app-server executable changed")
	}
	profile, err := codexAcceptanceSandboxProfile(command)
	if err != nil {
		return errors.New("Codex app-server sandbox unavailable")
	}
	arguments := append([]string{"-p", profile, command.executable}, command.args...)
	child := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", arguments...)
	child.Env = append([]string(nil), command.env...)
	child.Dir = command.dir
	child.WaitDelay = 2 * time.Second
	stderr := &codexAcceptanceDiagnosticBuffer{}
	child.Stderr = stderr
	stdin, err := child.StdinPipe()
	if err != nil {
		return errors.New("open Codex app-server input")
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return errors.New("open Codex app-server output")
	}
	if err := child.Start(); err != nil {
		_ = stdin.Close()
		return errors.New("start Codex app-server")
	}
	defer func() {
		_ = stdin.Close()
		if err := child.Wait(); err != nil && returnErr == nil {
			diagnostic := sanitiseCodexAcceptanceDiagnostic(string(stderr.data))
			if diagnostic == "" {
				returnErr = errors.New("Codex app-server failed")
			} else {
				returnErr = fmt.Errorf("Codex app-server failed: %s", diagnostic)
			}
		}
		after, err := captureCodexInstalledExecutable(command.executable)
		if (err != nil || after != client) && returnErr == nil {
			returnErr = errors.New("Codex app-server executable changed")
		}
	}()

	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(stdout)
	if err := encoder.Encode(map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]string{"name": "cq-release-validation", "version": "1"}},
	}); err != nil {
		return errors.New("initialise Codex app-server")
	}
	if _, err := awaitCodexAppServerResponse(decoder, 1); err != nil {
		return err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		return errors.New("acknowledge Codex app-server initialisation")
	}
	if err := encoder.Encode(map[string]any{
		"id": 2, "method": "thread/start",
		"params": map[string]any{
			"approvalPolicy": "never", "cwd": isolation.work, "ephemeral": true,
			// The app-server process is already confined by the outer sandbox-exec
			// profile. A nested Codex sandbox fails with sandbox_apply EPERM on macOS.
			"model": "gpt-5.6-sol", "modelProvider": "cq_acceptance", "sandbox": "danger-full-access",
			"developerInstructions": "When asked to read a file, use the shell tool before answering. Never guess file contents.",
		},
	}); err != nil {
		return errors.New("start Codex app-server thread")
	}
	started, err := awaitCodexAppServerResponse(decoder, 2)
	if err != nil {
		return err
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(started, &thread); err != nil || thread.Thread.ID == "" {
		return errors.New("decode Codex app-server thread")
	}
	if err := encoder.Encode(map[string]any{
		"id": 3, "method": "turn/start",
		"params": map[string]any{
			"threadId": thread.Thread.ID,
			"input":    []map[string]string{{"type": "text", "text": "Reply with exactly " + initialText + " and no other text."}},
		},
	}); err != nil {
		return errors.New("start Codex app-server text turn")
	}
	if err := awaitCodexAppServerTextTurn(decoder, 3, thread.Thread.ID, initialText); err != nil {
		return err
	}
	if err := encoder.Encode(map[string]any{
		"id": 4, "method": "turn/start",
		"params": map[string]any{
			"threadId": thread.Thread.ID,
			"input": []map[string]string{{
				"type": "text",
				"text": "Run `/bin/cat message.txt` using the shell tool now. Do not guess its contents. After the tool returns, reply with exactly its output and no other text.",
			}},
		},
	}); err != nil {
		return errors.New("start Codex app-server tool turn")
	}
	if err := awaitCodexAppServerToolTurn(decoder, 4, thread.Thread.ID, finalText); err != nil {
		return err
	}
	if err := encoder.Encode(map[string]any{
		"id": 5, "method": "thread/compact/start", "params": map[string]string{"threadId": thread.Thread.ID},
	}); err != nil {
		return errors.New("start Codex app-server compaction")
	}
	return awaitCodexAppServerTurn(decoder, 5, thread.Thread.ID)
}

func codexLiveAppServerArguments(baseURL string, webSocket bool) []string {
	provider := "model_providers.cq_acceptance."
	return []string{
		"app-server", "--stdio", "--strict-config",
		"-c", `model_provider="cq_acceptance"`,
		"-c", provider + `name="OpenAI"`,
		"-c", provider + "base_url=" + strconv.Quote(baseURL),
		"-c", provider + `wire_api="responses"`,
		"-c", provider + `requires_openai_auth=true`,
		"-c", provider + "supports_websockets=" + strconv.FormatBool(webSocket),
		"-c", provider + `supports_standalone_web_search=false`,
		"-c", provider + `request_max_retries=0`,
		"-c", provider + `stream_max_retries=0`,
		"-c", `approval_policy="never"`,
		"-c", `analytics.enabled=false`,
		"-c", `cli_auth_credentials_store="file"`,
		"-c", `check_for_update_on_startup=false`,
		"-c", `features.enable_request_compression=true`,
		"-c", `features.plugins=false`,
		"-c", `features.remote_compaction_v2=true`,
		"-c", `features.respect_system_proxy=true`,
	}
}

type codexAppServerMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func awaitCodexAppServerResponse(decoder *json.Decoder, id int) (json.RawMessage, error) {
	for {
		message, err := decodeCodexAppServerMessage(decoder)
		if err != nil {
			return nil, err
		}
		if string(message.ID) != strconv.Itoa(id) {
			continue
		}
		if len(message.Error) != 0 && string(message.Error) != "null" {
			return nil, errors.New("Codex app-server request failed")
		}
		return message.Result, nil
	}
}

func awaitCodexAppServerTurn(decoder *json.Decoder, id int, threadID string) error {
	responseSeen := false
	completed := false
	for !responseSeen || !completed {
		message, err := decodeCodexAppServerMessage(decoder)
		if err != nil {
			return err
		}
		if string(message.ID) == strconv.Itoa(id) {
			if len(message.Error) != 0 && string(message.Error) != "null" {
				return errors.New("Codex app-server turn request failed")
			}
			responseSeen = true
		}
		if message.Method != "turn/completed" {
			continue
		}
		var notification struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				Status string          `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(message.Params, &notification); err != nil || notification.ThreadID != threadID {
			continue
		}
		if notification.Turn.Status != "completed" {
			return errors.New("Codex app-server turn did not complete")
		}
		completed = true
	}
	return nil
}

func awaitCodexAppServerTextTurn(decoder *json.Decoder, id int, threadID, expectedText string) error {
	responseSeen := false
	textSeen := false
	for {
		message, err := decodeCodexAppServerMessage(decoder)
		if err != nil {
			return err
		}
		if string(message.ID) == strconv.Itoa(id) {
			if len(message.Error) != 0 && string(message.Error) != "null" {
				return errors.New("Codex app-server text turn request failed")
			}
			responseSeen = true
		}
		switch message.Method {
		case "item/completed":
			var notification struct {
				ThreadID string `json:"threadId"`
				Item     struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(message.Params, &notification) == nil && notification.ThreadID == threadID && notification.Item.Type == "agentMessage" && notification.Item.Text == expectedText {
				textSeen = true
			}
		case "turn/completed":
			var notification struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(message.Params, &notification) != nil || notification.ThreadID != threadID {
				continue
			}
			if notification.Turn.Status != "completed" {
				return errors.New("Codex app-server text turn did not complete")
			}
			if !responseSeen || !textSeen {
				return fmt.Errorf("Codex app-server text turn evidence incomplete: response=%t text=%t", responseSeen, textSeen)
			}
			return nil
		}
	}
}

func awaitCodexAppServerToolTurn(decoder *json.Decoder, id int, threadID, finalText string) error {
	responseSeen := false
	toolSeen := false
	continuationSeen := false
	agentTextBytes := 0
	for {
		message, err := decodeCodexAppServerMessage(decoder)
		if err != nil {
			return err
		}
		if string(message.ID) == strconv.Itoa(id) {
			if len(message.Error) != 0 && string(message.Error) != "null" {
				return errors.New("Codex app-server tool turn request failed")
			}
			responseSeen = true
		}
		switch message.Method {
		case "item/completed":
			var notification struct {
				ThreadID string `json:"threadId"`
				Item     struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Status   string `json:"status"`
					ExitCode *int   `json:"exitCode"`
				} `json:"item"`
			}
			if json.Unmarshal(message.Params, &notification) != nil || notification.ThreadID != threadID {
				continue
			}
			switch notification.Item.Type {
			case "agentMessage":
				agentTextBytes += len(notification.Item.Text)
				if notification.Item.Text == finalText {
					if !toolSeen {
						return errors.New("Codex app-server continuation arrived before tool execution")
					}
					continuationSeen = true
				}
			case "commandExecution":
				if notification.Item.Status != "completed" || notification.Item.ExitCode == nil || *notification.Item.ExitCode != 0 {
					return errors.New("Codex app-server tool execution failed")
				}
				toolSeen = true
			}
		case "turn/completed":
			var notification struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(message.Params, &notification) != nil || notification.ThreadID != threadID {
				continue
			}
			if notification.Turn.Status != "completed" {
				return errors.New("Codex app-server tool turn did not complete")
			}
			if !responseSeen || !toolSeen || !continuationSeen {
				return fmt.Errorf(
					"Codex app-server tool turn evidence incomplete: response=%t tool=%t continuation=%t agent_text_bytes=%d",
					responseSeen,
					toolSeen,
					continuationSeen,
					agentTextBytes,
				)
			}
			return nil
		}
	}
}

func decodeCodexAppServerMessage(decoder *json.Decoder) (codexAppServerMessage, error) {
	var message codexAppServerMessage
	if err := decoder.Decode(&message); err != nil {
		return message, errors.New("read Codex app-server response")
	}
	if message.Method == "error" {
		return message, fmt.Errorf("Codex app-server reported an error: %s", codexAppServerSafeErrorClassification(message.Params))
	}
	return message, nil
}

func codexAppServerSafeErrorClassification(params json.RawMessage) string {
	var notification struct {
		Error struct {
			CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			Message        string          `json:"message"`
		} `json:"error"`
		WillRetry bool `json:"willRetry"`
	}
	if json.Unmarshal(params, &notification) != nil {
		return "unknown will_retry=unknown"
	}
	classification := "unknown"
	var name string
	if json.Unmarshal(notification.Error.CodexErrorInfo, &name) == nil {
		switch name {
		case "contextWindowExceeded", "sessionBudgetExceeded", "usageLimitExceeded", "serverOverloaded", "cyberPolicy", "misalignmentPolicyViolation", "internalServerError", "unauthorized", "badRequest", "threadRollbackFailed", "sandboxError", "other":
			classification = name
		}
	} else {
		var connection map[string]struct {
			HTTPStatusCode *uint16 `json:"httpStatusCode"`
		}
		if json.Unmarshal(notification.Error.CodexErrorInfo, &connection) == nil && len(connection) == 1 {
			for kind, detail := range connection {
				switch kind {
				case "httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts":
					status := "unknown"
					if detail.HTTPStatusCode != nil {
						status = strconv.Itoa(int(*detail.HTTPStatusCode))
					}
					classification = kind + "(status=" + status + ")"
				}
			}
		}
	}
	return classification + " will_retry=" + strconv.FormatBool(notification.WillRetry) + " message=" + codexAppServerSafeMessageClassification(notification.Error.Message)
}

func codexAppServerSafeMessageClassification(message string) string {
	lower := strings.ToLower(message)
	classes := make([]string, 0, 8)
	if strings.Contains(lower, "websocket") && (strings.Contains(lower, "closed") || strings.Contains(lower, "disconnect")) {
		classes = append(classes, "websocket_closed")
	}
	if strings.Contains(lower, "before response.completed") || strings.Contains(lower, "before completion") {
		classes = append(classes, "response_incomplete")
	}
	for _, status := range []string{"401", "403", "502", "503"} {
		if strings.Contains(lower, "status "+status) || strings.Contains(lower, "status="+status) {
			classes = append(classes, "status_"+status)
		}
	}
	if strings.Contains(lower, "authentication required") || strings.Contains(lower, "login required") || strings.Contains(lower, "not logged in") {
		classes = append(classes, "auth_required")
	}
	if strings.Contains(lower, "refresh token") {
		classes = append(classes, "refresh_token")
	}
	if strings.Contains(lower, "failed to refresh") || strings.Contains(lower, "refresh failed") {
		classes = append(classes, "refresh_failed")
	}
	if strings.Contains(lower, "expired") && (strings.Contains(lower, "auth") || strings.Contains(lower, "token")) {
		classes = append(classes, "auth_expired")
	}
	if strings.Contains(lower, "unauthorized") {
		classes = append(classes, "unauthorized")
	}
	if strings.Contains(lower, "forbidden") {
		classes = append(classes, "forbidden")
	}
	if len(classes) == 0 {
		keywords := make([]string, 0, 8)
		for _, keyword := range []string{
			"access", "account", "api", "associated", "audience", "auth", "bearer", "chatgpt", "claim", "config", "credential",
			"decode", "environment", "error", "exchange", "expired", "failed", "file", "invalid", "issuer", "key", "load", "login",
			"mismatch", "missing", "oauth", "obtain", "openai", "parse", "permission", "provided", "refresh", "required", "sandbox",
			"scope", "signed", "stored", "token", "unable", "unknown", "unsupported", "valid",
		} {
			if strings.Contains(lower, keyword) {
				keywords = append(keywords, keyword)
			}
		}
		if len(keywords) == 0 {
			return "unclassified"
		}
		return "keywords_" + strings.Join(keywords, "_")
	}
	return strings.Join(classes, ",")
}

func TestDecodeCodexAppServerMessageReportsSafeErrorClassification(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(`{"method":"error","params":{"error":{"message":"must-not-appear websocket closed by server before response.completed unexpected status 502 Bad Gateway","additionalDetails":"must-not-appear","codexErrorInfo":{"responseStreamDisconnected":{"httpStatusCode":502}}},"threadId":"private","turnId":"private","willRetry":false}}`))
	_, err := decodeCodexAppServerMessage(decoder)
	if err == nil || !strings.Contains(err.Error(), "responseStreamDisconnected(status=502)") || !strings.Contains(err.Error(), "will_retry=false") ||
		!strings.Contains(err.Error(), "message=websocket_closed,response_incomplete,status_502") {
		t.Fatalf("safe app-server error classification missing: %v", err)
	}
	if strings.Contains(err.Error(), "must-not-appear") || strings.Contains(err.Error(), "private") {
		t.Fatalf("app-server error leaked private details: %v", err)
	}
}

func TestCodexAppServerSafeMessageClassificationReportsAuthWithoutRawMessage(t *testing.T) {
	private := "must-not-appear"
	got := codexAppServerSafeMessageClassification(private + " authentication required: failed to refresh refresh token")
	if got != "auth_required,refresh_token,refresh_failed" || strings.Contains(got, private) {
		t.Fatalf("safe auth classification = %q", got)
	}
}

func TestAwaitCodexAppServerToolTurnRequiresOrderedEvidence(t *testing.T) {
	stream := strings.Join([]string{
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"id":"command-1","type":"commandExecution","command":"read message","commandActions":[],"cwd":"/tmp","status":"completed","exitCode":0}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"id":"message-2","type":"agentMessage","text":"LIVE-APP-SERVER-TOOL-PONG"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[],"status":"completed"}}}`,
	}, "\n")
	decoder := json.NewDecoder(strings.NewReader(stream))
	if err := awaitCodexAppServerToolTurn(decoder, 3, "thread-1", "LIVE-APP-SERVER-TOOL-PONG"); err != nil {
		t.Fatalf("ordered app-server tool turn: %v", err)
	}
}

func TestAwaitCodexAppServerToolTurnRejectsContinuationBeforeTool(t *testing.T) {
	stream := strings.Join([]string{
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"id":"message-2","type":"agentMessage","text":"LIVE-APP-SERVER-TOOL-PONG"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[],"status":"completed"}}}`,
	}, "\n")
	decoder := json.NewDecoder(strings.NewReader(stream))
	if err := awaitCodexAppServerToolTurn(decoder, 3, "thread-1", "LIVE-APP-SERVER-TOOL-PONG"); err == nil {
		t.Fatal("app-server continuation before tool succeeded")
	}
}

func TestAwaitCodexAppServerTextTurnRequiresExactText(t *testing.T) {
	stream := strings.Join([]string{
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"id":"message-1","type":"agentMessage","text":"LIVE-APP-SERVER-STARTING"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[],"status":"completed"}}}`,
	}, "\n")
	decoder := json.NewDecoder(strings.NewReader(stream))
	if err := awaitCodexAppServerTextTurn(decoder, 3, "thread-1", "LIVE-APP-SERVER-STARTING"); err != nil {
		t.Fatalf("app-server text turn: %v", err)
	}
}
