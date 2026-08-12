package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	codexAcceptanceTurns      = 20
	codexAcceptanceLocalToken = "cq-acceptance-local"
	codexAcceptanceExecutable = "/opt/homebrew/bin/codex"
	codexAcceptanceVersion    = "codex-cli 0.146.0"
)

type CodexHTTPAcceptanceResult struct {
	Turns                     int
	Requests                  int
	SelectorCalls             int
	ContinuityErrors          uint64
	UnknownEvents             uint64
	InstalledVersion          string
	InstalledRequests         uint64
	InstalledModelRequests    uint64
	InstalledAttempts         uint64
	InstalledSelectorCalls    int
	InstalledStrongKeys       uint64
	InstalledZstdRequests     uint64
	InstalledUnknownEvents    uint64
	InstalledContinuityErrors uint64
	InstalledQuiescentLeases  int
	HeadroomRequests          uint64
	HeadroomParseErrors       uint64
	UnexpectedRoutes          uint64
	EgressAttempts            uint64
	InstalledResolutions      uint64
	PongVerified              bool
}

type codexAcceptanceCommand struct {
	executable         string
	expectedExecutable codexInstalledExecutableProof
	args               []string
	env                []string
	dir                string
	endpoint           string
	outputPath         string
	egressProxyURL     string
	sandboxWriteRoot   string
	captureOutput      bool
	loopbackOnly       bool
}

type codexAcceptanceRunner interface {
	Run(context.Context, codexAcceptanceCommand) ([]byte, error)
}

type codexAcceptanceDependencies struct {
	executable string
	runner     codexAcceptanceRunner
}

type codexAcceptanceChooser struct {
	mu    sync.Mutex
	calls int
}

func (chooser *codexAcceptanceChooser) Choose(_ context.Context, requirements CodexRouteRequirements, excluded ...codex.SelectionExclusion) (RouteChoice, error) {
	chooser.mu.Lock()
	defer chooser.mu.Unlock()
	blocked := make(map[codex.AccountKey]bool, len(excluded))
	for _, exclusion := range excluded {
		blocked[exclusion.AccountKey] = true
	}
	keys := []codex.AccountKey{"acceptance-one", "acceptance-two"}
	start := chooser.calls % len(keys)
	chooser.calls++
	for offset := range len(keys) {
		key := keys[(start+offset)%len(keys)]
		if blocked[key] {
			continue
		}
		return RouteChoice{
			AccountKey:      key,
			RequestedModel:  requirements.RequestedModel,
			EffectiveModel:  requirements.RequestedModel,
			RequiredBuckets: routeBuckets(requirements.RequestedModel, requirements.RequiredModels, "pro"),
		}, nil
	}
	return RouteChoice{}, errors.New("acceptance accounts excluded")
}

func (chooser *codexAcceptanceChooser) Calls() int {
	chooser.mu.Lock()
	defer chooser.mu.Unlock()
	return chooser.calls
}

type codexAcceptanceCredentials struct {
	inventory codex.Inventory
	resolved  atomic.Uint64
}

func newCodexAcceptanceCredentials() *codexAcceptanceCredentials {
	accounts := make([]codex.LogicalAccount, 0, 2)
	for _, key := range []codex.AccountKey{"acceptance-one", "acceptance-two"} {
		candidateID := codex.CandidateID(key + "-candidate")
		userID := "acceptance-user-" + string(key)
		accounts = append(accounts, codex.LogicalAccount{
			Key:      key,
			Routable: true,
			Identity: codex.AccountIdentity{AccountID: string(key), UserID: userID, PlanType: "pro"},
			Candidates: []codex.CredentialCandidate{{
				Ref:             codex.CandidateRef{AccountKey: key, CandidateID: candidateID},
				Revision:        "acceptance-revision",
				Source:          codex.SourceManaged,
				AccessExpiresAt: time.Now().Add(time.Hour),
				Routable:        true,
			}},
		})
	}
	return &codexAcceptanceCredentials{inventory: codex.Inventory{Accounts: accounts}}
}

func (credentials *codexAcceptanceCredentials) List(context.Context) (codex.Inventory, error) {
	return credentials.inventory, nil
}

func (credentials *codexAcceptanceCredentials) Resolve(_ context.Context, ref codex.CandidateRef) (codex.CredentialMaterial, error) {
	if ref.AccountKey == "" || ref.CandidateID != codex.CandidateID(ref.AccountKey+"-candidate") {
		return codex.CredentialMaterial{}, errors.New("unknown acceptance credential")
	}
	return codex.CredentialMaterial{AccessToken: "cq-token-" + string(ref.AccountKey), AccountID: string(ref.AccountKey)}, nil
}

func (credentials *codexAcceptanceCredentials) ResolveExact(_ context.Context, planned codex.PlannedCandidate) (codex.CredentialMaterial, error) {
	key := planned.Ref.AccountKey
	userID := "acceptance-user-" + string(key)
	if (key != "acceptance-one" && key != "acceptance-two") ||
		planned.Ref.CandidateID != codex.CandidateID(key+"-candidate") ||
		planned.Revision != "acceptance-revision" || planned.Source != codex.SourceManaged ||
		planned.Identity.AccountID != string(key) || planned.Identity.UserID != userID {
		return codex.CredentialMaterial{}, errors.New("unknown acceptance credential generation")
	}
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": string(key),
			"chatgpt_user_id":    userID,
		},
	})
	material := codex.CredentialMaterial{
		AccessToken: "cq-token-" + string(key),
		AccountID:   string(key),
		IDToken:     "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature",
	}
	credentials.resolved.Add(1)
	return material, nil
}

func (credentials *codexAcceptanceCredentials) Resolutions() uint64 {
	if credentials == nil {
		return 0
	}
	return credentials.resolved.Load()
}

type codexAcceptanceUpstream struct {
	mu     sync.Mutex
	tokens []string
	count  atomic.Uint64
}

func (upstream *codexAcceptanceUpstream) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, maxRequestBody+1))
	_ = request.Body.Close()
	token := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	upstream.mu.Lock()
	upstream.tokens = append(upstream.tokens, token)
	upstream.mu.Unlock()
	responseID := upstream.count.Add(1)
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"acceptance-%d\"}}\n\n", responseID)
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"acceptance-%d\",\"end_turn\":true}}\n\n", responseID)
}

func (upstream *codexAcceptanceUpstream) Tokens() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]string(nil), upstream.tokens...)
}

func runCodexHTTPEnforcedAcceptance(ctx context.Context) (CodexHTTPAcceptanceResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	upstream := &codexAcceptanceUpstream{}
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return CodexHTTPAcceptanceResult{}, fmt.Errorf("listen acceptance upstream: %w", err)
	}
	upstreamServer := &http.Server{Handler: upstream, ReadHeaderTimeout: 5 * time.Second}
	upstreamErrors := make(chan error, 1)
	go func() {
		upstreamErrors <- upstreamServer.Serve(upstreamListener)
	}()
	defer shutdownCodexAcceptanceServer(upstreamServer)

	credentials := newCodexAcceptanceCredentials()
	chooser := &codexAcceptanceChooser{}
	router := &CodexRequestRouter{
		Scope: &CodexRequestScope{Chooser: chooser, Inventory: credentials},
		Executor: &CodexAttemptExecutor{
			Inventory: credentials,
			Secrets:   credentials,
			Transport: &CodexTokenTransport{Inner: &http.Transport{
				Proxy:             http.ProxyFromEnvironment,
				ForceAttemptHTTP2: false,
			}},
		},
	}
	store, err := OpenCodexLeaseStore(fsutil.NewMemFS(), "/acceptance/leases.json", "/acceptance/leases.key")
	if err != nil {
		return CodexHTTPAcceptanceResult{}, err
	}
	enforcer, err := NewCodexHTTPEnforcer(router, 1, store)
	if err != nil {
		return CodexHTTPAcceptanceResult{}, err
	}
	upstreamURL := "http://" + upstreamListener.Addr().String()
	server := &Server{
		Config: &Config{
			LocalToken:     codexAcceptanceLocalToken,
			ClaudeUpstream: upstreamURL,
			CodexUpstream:  upstreamURL,
		},
		CodexRequests:     router,
		CodexHTTPEnforcer: enforcer,
	}
	handler, err := server.handler()
	if err != nil {
		return CodexHTTPAcceptanceResult{}, err
	}
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return CodexHTTPAcceptanceResult{}, fmt.Errorf("listen acceptance proxy: %w", err)
	}
	proxyServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	proxyErrors := make(chan error, 1)
	go func() {
		proxyErrors <- proxyServer.Serve(proxyListener)
	}()
	defer shutdownCodexAcceptanceServer(proxyServer)

	client := &http.Client{Timeout: 5 * time.Second}
	proxyURL := "http://" + proxyListener.Addr().String() + "/responses"
	requests := 0
	for turn := range codexAcceptanceTurns {
		for range 2 {
			body := []byte(`{"type":"response.create","model":"gpt-5.4","input":"ping"}`)
			encoding := ""
			if turn%2 != 0 {
				body = encodeCodexAcceptanceZstd(body)
				encoding = "zstd"
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL, bytes.NewReader(body))
			if err != nil {
				return CodexHTTPAcceptanceResult{}, err
			}
			request.Header.Set("Authorization", "Bearer "+codexAcceptanceLocalToken)
			request.Header.Set("Content-Type", "application/json")
			if encoding != "" {
				request.Header.Set("Content-Encoding", encoding)
			}
			request.Header.Set(codexTurnMetadataKey, fmt.Sprintf(`{"session_id":"acceptance-session","thread_id":"acceptance-thread-%d","turn_id":"acceptance-turn-%d","request_kind":"turn"}`, turn%2, turn))
			response, err := client.Do(request)
			if err != nil {
				return CodexHTTPAcceptanceResult{}, fmt.Errorf("acceptance request %d: %w", requests, err)
			}
			_, copyErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if copyErr != nil || closeErr != nil || response.StatusCode != http.StatusOK {
				return CodexHTTPAcceptanceResult{}, fmt.Errorf("acceptance request %d failed: status=%d copy=%v close=%v", requests, response.StatusCode, copyErr, closeErr)
			}
			requests++
		}
	}

	tokens := upstream.Tokens()
	if len(tokens) != requests {
		return CodexHTTPAcceptanceResult{}, fmt.Errorf("acceptance upstream requests = %d, want %d", len(tokens), requests)
	}
	for turn := range codexAcceptanceTurns {
		want := "cq-token-acceptance-one"
		if turn%2 != 0 {
			want = "cq-token-acceptance-two"
		}
		if tokens[turn*2] != want || tokens[turn*2+1] != want {
			return CodexHTTPAcceptanceResult{}, fmt.Errorf("acceptance turn %d changed account", turn)
		}
	}
	if err := codexAcceptanceServeError(upstreamErrors); err != nil {
		return CodexHTTPAcceptanceResult{}, err
	}
	if err := codexAcceptanceServeError(proxyErrors); err != nil {
		return CodexHTTPAcceptanceResult{}, err
	}
	health := enforcer.Observer.Health()
	result := CodexHTTPAcceptanceResult{
		Turns:            codexAcceptanceTurns,
		Requests:         requests,
		SelectorCalls:    chooser.Calls(),
		ContinuityErrors: health.ContinuityErrors,
		UnknownEvents:    health.Unknown,
	}
	if result.SelectorCalls != codexAcceptanceTurns || result.ContinuityErrors != 0 || result.UnknownEvents != 0 {
		return result, fmt.Errorf("acceptance health failed: selectors=%d continuity=%d unknown=%d", result.SelectorCalls, result.ContinuityErrors, result.UnknownEvents)
	}
	return result, nil
}

func encodeCodexAcceptanceZstd(body []byte) []byte {
	encoder, _ := zstd.NewWriter(nil)
	defer encoder.Close()
	return encoder.EncodeAll(body, nil)
}

func shutdownCodexAcceptanceServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func codexAcceptanceServeError(serverErrors <-chan error) error {
	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	default:
	}
	return nil
}
