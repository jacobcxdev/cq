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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexLiveNormalAccount codex.AccountKey = "live-normal-account"

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
		(accessClaims.ExpiresAt != 0 && time.Unix(accessClaims.ExpiresAt, 0).Before(time.Now().Add(5*time.Minute))) {
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
		AccessToken:  document.Tokens.AccessToken,
		RefreshToken: document.Tokens.RefreshToken,
		IDToken:      document.Tokens.IDToken,
		AccountID:    claims.AccountID,
	}
	result.localToken = localToken
	return result, nil
}

func newCodexLiveNormalAcceptanceServer(t *testing.T, credential codexLiveAcceptanceCredential) (*httptest.Server, *RuntimeSupervisor, *codexLiveNormalTraffic) {
	return newCodexLiveNormalAcceptanceServerWithCallers(t, credential, []NormalCallerCredentialV1{{
		Domain: NormalCallerCodex, Bearer: credential.localToken, SubjectID: "live-normal-codex", ValidUntil: time.Now().Add(10 * time.Minute),
	}})
}

func newCodexLiveNormalAcceptanceServerWithCallers(t *testing.T, credential codexLiveAcceptanceCredential, callers []NormalCallerCredentialV1) (*httptest.Server, *RuntimeSupervisor, *codexLiveNormalTraffic) {
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
	stream := core.capacity.NewObservationStream()
	if !core.capacity.Observe(stream.Stamp(CapacityFact{
		AccountKey: codexLiveNormalAccount, Bucket: CapacityBucketBase, RemainingPct: 100,
		Source: CapacitySourceLiveRateLimits, ObservedAt: now, ResetAt: now.Add(time.Hour), Confidence: CapacityConfidenceAuthoritative,
	})) {
		t.Fatal("seed live Codex normal capacity")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	planner := &CodexHTTPRequestPlanFactory{
		Inventory: core.inventory, Capacity: core.capacity, Routes: core.continuity, Runtime: core.leaseRuntime,
		DefaultAccountKey: codexLiveNormalAccount,
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		Now:               time.Now,
	}
	httpHandler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{
		Executor: &CodexAttemptExecutor{
			Inventory: core.inventory,
			Secrets:   core.inventory,
			Transport: &CodexTokenTransport{Inner: transport},
		},
		Refresher: core.inventory,
		Capacity:  core.capacity,
	}, DefaultCodexUpstream)
	if err != nil {
		t.Fatalf("construct live Codex normal HTTP handler: %v", err)
	}
	core.handler = httpHandler

	webSocketExecutor := NewCodexWebSocketAttemptExecutor(core.inventory, core.inventory)
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
	listener, supervisor := newCodexRuntimeSupervisorAcceptanceServerWithCredentials(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		traffic.serveHTTP(handler, writer, request)
	}), callers)
	return listener, supervisor, traffic
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

func runCodexAppServerCompactionAcceptance(
	ctx context.Context,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
) (returnErr error) {
	command := codexAcceptanceCommand{
		executable:         client.path,
		expectedExecutable: client,
		args:               codexLiveAppServerArguments(baseURL),
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
			"model": "gpt-5.6-sol", "modelProvider": "cq_acceptance", "sandbox": "read-only",
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
			"input":    []map[string]string{{"type": "text", "text": "Reply with exactly LIVE-COMPACT-SEED-PONG and no other text."}},
		},
	}); err != nil {
		return errors.New("start Codex app-server seed turn")
	}
	if err := awaitCodexAppServerTurn(decoder, 3, thread.Thread.ID); err != nil {
		return err
	}
	if err := encoder.Encode(map[string]any{
		"id": 4, "method": "thread/compact/start", "params": map[string]string{"threadId": thread.Thread.ID},
	}); err != nil {
		return errors.New("start Codex app-server compaction")
	}
	return awaitCodexAppServerTurn(decoder, 4, thread.Thread.ID)
}

func codexLiveAppServerArguments(baseURL string) []string {
	provider := "model_providers.cq_acceptance."
	return []string{
		"app-server", "--stdio", "--strict-config",
		"-c", `model_provider="cq_acceptance"`,
		"-c", provider + `name="OpenAI"`,
		"-c", provider + "base_url=" + strconv.Quote(baseURL),
		"-c", provider + `wire_api="responses"`,
		"-c", provider + `requires_openai_auth=true`,
		"-c", provider + `supports_websockets=false`,
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

func decodeCodexAppServerMessage(decoder *json.Decoder) (codexAppServerMessage, error) {
	var message codexAppServerMessage
	if err := decoder.Decode(&message); err != nil {
		return message, errors.New("read Codex app-server response")
	}
	if message.Method == "error" {
		return message, errors.New("Codex app-server reported an error")
	}
	return message, nil
}
