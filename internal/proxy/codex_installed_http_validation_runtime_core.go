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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexInstalledHTTPValidationTempPrefix = "cq-codex-v2-"

const (
	codexInstalledHTTPValidationAccountA codex.AccountKey = "validation-account-a"
	codexInstalledHTTPValidationAccountB codex.AccountKey = "validation-account-b"
	codexInstalledHTTPValidationDefault  codex.AccountKey = "validation-account-default"
)

type codexInstalledHTTPValidationRuntimeCore struct {
	tempRoot string
	handler  *CodexNativeHTTPHandler
	upstream *codexInstalledHTTPValidationUpstream

	credentialOwner *codex.CredentialControl
	continuity      *CodexContinuityCoordinator
	leaseRuntime    *CodexLeaseRuntime
	admissions      *codexInstalledNativeHTTPAdmissionCounter
	capacity        *CodexCapacityLedger
	inventory       *codexInstalledHTTPValidationInventory

	closeMu  sync.Mutex
	closed   bool
	closeErr error
}

type codexInstalledHTTPValidationFileSystem struct {
	fsutil.OSFileSystem
	home string
}

func (fsys codexInstalledHTTPValidationFileSystem) UserHomeDir() (string, error) {
	if fsys.home == "" {
		return "", errors.New("Codex validation home unavailable")
	}
	return fsys.home, nil
}

func newCodexInstalledHTTPValidationRuntimeCore(ctx context.Context) (core *codexInstalledHTTPValidationRuntimeCore, returnErr error) {
	if ctx == nil {
		return nil, errors.New("Codex installed HTTP validation context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	core = &codexInstalledHTTPValidationRuntimeCore{}
	defer func() {
		if recover() != nil {
			_ = core.close()
			core = nil
			returnErr = errors.New("Codex installed HTTP validation runtime panicked")
			return
		}
		if returnErr != nil {
			_ = core.close()
			core = nil
		}
	}()

	shortTempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return nil, errors.New("resolve Codex installed HTTP validation isolation parent")
	}
	root, err := os.MkdirTemp(shortTempRoot, codexInstalledHTTPValidationTempPrefix)
	if err != nil {
		return nil, errors.New("create Codex installed HTTP validation isolation")
	}
	core.tempRoot = root
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, errors.New("secure Codex installed HTTP validation isolation")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("validate Codex installed HTTP validation isolation")
	}
	fsys := codexInstalledHTTPValidationFileSystem{home: root}

	ownerDir := filepath.Join(root, "owner")
	if err := fsutil.EnsureSecureDirectory(fsys, ownerDir); err != nil {
		return nil, errors.New("prepare isolated Codex credential authority")
	}
	store, err := codex.NewManagedStore(fsys)
	if err != nil {
		return nil, errors.New("prepare isolated Codex managed store")
	}
	credentialCoordinator, err := codex.NewCredentialCoordinator(store)
	if err != nil {
		return nil, errors.New("prepare isolated Codex credential coordinator")
	}
	credentialCoordinator.ExternalSources = nil
	core.credentialOwner, err = codex.OpenCredentialControlPrepared(
		ctx,
		filepath.Join(ownerDir, "credential.sock"),
		credentialCoordinator,
		nil,
	)
	if err != nil || core.credentialOwner == nil || !core.credentialOwner.Owner() {
		return nil, errors.New("acquire isolated Codex credential owner")
	}

	stateDir := filepath.Join(root, "continuity")
	options := CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     filepath.Join(stateDir, "lease.key"),
		JournalPath: filepath.Join(stateDir, "lease.json"),
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       time.Now,
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}},
	}
	if err := InitialiseCodexContinuityAuthority(options, core.credentialOwner); err != nil {
		return nil, errors.New("initialise isolated Codex v2 continuity authority")
	}
	core.continuity, err = OpenCodexContinuityCoordinator(options, core.credentialOwner)
	if err != nil {
		return nil, errors.New("open isolated Codex v2 continuity authority")
	}

	core.inventory = newCodexInstalledHTTPValidationInventory(time.Now())
	core.admissions = &codexInstalledNativeHTTPAdmissionCounter{}
	core.leaseRuntime, err = newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(
		core.continuity,
		core.inventory.revalidate,
		core.admissions,
	)
	if err != nil {
		return nil, errors.New("open isolated Codex v2 request runtime")
	}
	core.capacity = NewCodexCapacityLedger(time.Now, 24*time.Hour)
	core.upstream, err = startCodexInstalledHTTPValidationUpstream()
	if err != nil {
		return nil, err
	}
	executor := &CodexAttemptExecutor{
		Inventory: core.inventory,
		Secrets:   core.inventory,
		Transport: &CodexTokenTransport{Inner: newCodexInstalledHTTPValidationRoundTripper(core.upstream.address)},
	}
	planner := &CodexHTTPRequestPlanFactory{
		Inventory:         core.inventory,
		Capacity:          core.capacity,
		Routes:            core.continuity,
		Runtime:           core.leaseRuntime,
		DefaultAccountKey: codexInstalledHTTPValidationDefault,
		Authority: CodexLeaseAuthorityPolicy{
			ModeEpoch:     1,
			Authoritative: true,
		},
		Headroom:     codexInstalledHTTPValidationHeadroom{},
		HeadroomMode: HeadroomModeToken,
		Now:          time.Now,
	}
	session := &CodexHTTPRequestSession{
		Executor:  executor,
		Refresher: core.inventory,
		Capacity:  core.capacity,
	}
	core.handler, err = NewCodexNativeHTTPHandler(planner, session, "http://"+core.upstream.address)
	if err != nil {
		return nil, errors.New("construct isolated production Codex native HTTP handler")
	}
	if err := core.setCapacity(100, 90, 10); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return core, nil
}

func (core *codexInstalledHTTPValidationRuntimeCore) nativeHTTPAdmissionSnapshot() codexInstalledNativeHTTPAdmissionSnapshot {
	if core == nil || core.admissions == nil || core.leaseRuntime == nil {
		return codexInstalledNativeHTTPAdmissionSnapshot{PromotionBlocked: true}
	}
	return codexInstalledNativeHTTPAdmissionSnapshot{
		FirstAuthoritative: core.admissions.snapshot(),
		PromotionBlocked:   core.leaseRuntime.nativeHTTPAdmissionPromotionBlocked(),
	}
}

func (core *codexInstalledHTTPValidationRuntimeCore) nativeHTTPHandler() *CodexNativeHTTPHandler {
	if core == nil {
		return nil
	}
	return core.handler
}

func (core *codexInstalledHTTPValidationRuntimeCore) installedListenerExercise(address, localToken string) (codexInstalledHTTPExercise, error) {
	if core == nil || core.handler == nil || core.upstream == nil || core.capacity == nil || core.inventory == nil {
		return nil, errors.New("Codex installed HTTP validation exercise unavailable")
	}
	target, err := validateCodexInstalledHTTPValidationLoopbackAddress(address)
	if err != nil || !validCodexInstalledHTTPValidationToken(localToken) {
		return nil, err
	}
	return &codexInstalledHTTPValidationExercise{
		core:       core,
		address:    target,
		localToken: localToken,
		client: &http.Client{
			Transport: newCodexInstalledHTTPValidationRoundTripper(target),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("Codex installed HTTP validation redirect rejected")
			},
		},
	}, nil
}

func (core *codexInstalledHTTPValidationRuntimeCore) setCapacity(a, b, defaultAccount int) error {
	if core == nil || core.capacity == nil {
		return errors.New("Codex installed HTTP validation capacity unavailable")
	}
	for account, remaining := range map[codex.AccountKey]int{
		codexInstalledHTTPValidationAccountA: a,
		codexInstalledHTTPValidationAccountB: b,
		codexInstalledHTTPValidationDefault:  defaultAccount,
	} {
		stream := core.capacity.NewObservationStream()
		fact := stream.Stamp(CapacityFact{
			AccountKey:   account,
			Bucket:       CapacityBucketBase,
			RemainingPct: remaining,
			Source:       CapacitySourceLiveRateLimits,
			ObservedAt:   time.Now(),
			ResetAt:      time.Now().Add(time.Hour),
			Confidence:   CapacityConfidenceAuthoritative,
		})
		if !core.capacity.Observe(fact) {
			return errors.New("set Codex installed HTTP validation capacity")
		}
	}
	return nil
}

func (core *codexInstalledHTTPValidationRuntimeCore) close() error {
	return core.closeWithTimeout(codexInstalledHTTPValidationQuiesceTimeout)
}

func (core *codexInstalledHTTPValidationRuntimeCore) closeWithTimeout(timeout time.Duration) error {
	if core == nil {
		return nil
	}
	if timeout <= 0 {
		return errors.New("Codex installed HTTP validation close timeout unavailable")
	}
	core.closeMu.Lock()
	defer core.closeMu.Unlock()
	if core.closed {
		return core.closeErr
	}
	if core.handler != nil {
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), timeout)
		drainErr := core.handler.CloseAndDrain(drainCtx)
		cancelDrain()
		if drainErr != nil {
			return drainErr
		}
	}
	var closeErr error
	if core.upstream != nil {
		closeErr = errors.Join(closeErr, core.upstream.close())
	}
	if core.continuity != nil {
		closeErr = errors.Join(closeErr, core.continuity.Close())
	}
	if core.credentialOwner != nil {
		closeErr = errors.Join(closeErr, core.credentialOwner.Close())
	}
	if core.tempRoot != "" {
		closeErr = errors.Join(closeErr, removeCodexInstalledHTTPValidationTempRoot(core.tempRoot))
	}
	core.handler = nil
	core.capacity = nil
	core.inventory = nil
	core.closed = true
	core.closeErr = closeErr
	return core.closeErr
}

func removeCodexInstalledHTTPValidationTempRoot(root string) error {
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) || filepath.Dir(clean) == clean || filepath.Dir(clean) == string(filepath.Separator) ||
		!strings.HasPrefix(filepath.Base(clean), codexInstalledHTTPValidationTempPrefix) {
		return errors.New("refuse unsafe Codex installed HTTP validation cleanup")
	}
	if err := os.RemoveAll(clean); err != nil {
		return errors.New("remove Codex installed HTTP validation isolation")
	}
	return nil
}

type codexInstalledHTTPValidationInventory struct {
	inventory codex.Inventory
	material  map[codex.AccountKey]codex.CredentialMaterial
}

func newCodexInstalledHTTPValidationInventory(now time.Time) *codexInstalledHTTPValidationInventory {
	result := &codexInstalledHTTPValidationInventory{material: make(map[codex.AccountKey]codex.CredentialMaterial)}
	for _, fixture := range []struct {
		key       codex.AccountKey
		candidate codex.CandidateID
		accountID string
		userID    string
		token     string
	}{
		{codexInstalledHTTPValidationAccountA, "validation-candidate-a", "validation-upstream-a", "validation-user-a", "validation-token-a"},
		{codexInstalledHTTPValidationAccountB, "validation-candidate-b", "validation-upstream-b", "validation-user-b", "validation-token-b"},
		{codexInstalledHTTPValidationDefault, "validation-candidate-default", "validation-upstream-default", "validation-user-default", "validation-token-default"},
	} {
		identity := codex.AccountIdentity{AccountID: fixture.accountID, UserID: fixture.userID, PlanType: "pro"}
		candidate := codex.CredentialCandidate{
			Ref: codex.CandidateRef{
				AccountKey:  fixture.key,
				CandidateID: fixture.candidate,
			},
			Revision:        codex.Revision("validation-revision-1"),
			Source:          codex.SourceExternal,
			AccessExpiresAt: now.Add(24 * time.Hour),
			Routable:        true,
		}
		result.inventory.Accounts = append(result.inventory.Accounts, codex.LogicalAccount{
			Key:        fixture.key,
			Identity:   identity,
			Candidates: []codex.CredentialCandidate{candidate},
			Routable:   true,
		})
		result.material[fixture.key] = codex.CredentialMaterial{
			AccessToken: fixture.token,
			IDToken:     codexInstalledHTTPValidationIDToken(fixture.accountID, fixture.userID),
			AccountID:   fixture.accountID,
		}
	}
	return result
}

func codexInstalledHTTPValidationIDToken(accountID, userID string) string {
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    userID,
			"chatgpt_plan_type":  "pro",
		},
	})
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func (inventory *codexInstalledHTTPValidationInventory) List(ctx context.Context) (codex.Inventory, error) {
	if ctx == nil {
		return codex.Inventory{}, errors.New("Codex validation inventory context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return codex.Inventory{}, err
	}
	if inventory == nil {
		return codex.Inventory{}, codex.ErrCredentialAuthorityUnavailable
	}
	result := codex.Inventory{Accounts: make([]codex.LogicalAccount, len(inventory.inventory.Accounts))}
	for index, account := range inventory.inventory.Accounts {
		result.Accounts[index] = account
		result.Accounts[index].Candidates = append([]codex.CredentialCandidate(nil), account.Candidates...)
	}
	return result, nil
}

func (inventory *codexInstalledHTTPValidationInventory) ResolveExact(ctx context.Context, planned codex.PlannedCandidate) (codex.CredentialMaterial, error) {
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
			material, ok := inventory.material[account.Key]
			if !ok {
				break
			}
			return material, nil
		}
	}
	return codex.CredentialMaterial{}, codex.ErrStaleRevision
}

func (inventory *codexInstalledHTTPValidationInventory) RefreshReference(ctx context.Context, _ codex.CandidateRef, _ codex.Revision) (codex.CandidateRef, codex.Revision, error) {
	if ctx == nil {
		return codex.CandidateRef{}, "", errors.New("Codex validation refresh context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return codex.CandidateRef{}, "", err
	}
	return codex.CandidateRef{}, "", codex.ErrRefreshUnavailable
}

func (inventory *codexInstalledHTTPValidationInventory) revalidate(ctx context.Context, key codex.AccountKey) error {
	view, err := inventory.List(ctx)
	if err != nil {
		return err
	}
	for _, account := range view.Accounts {
		if account.Key == key && account.Routable && !account.Unstable {
			return nil
		}
	}
	return codex.ErrStaleRevision
}

type codexInstalledHTTPValidationHeadroom struct{}

func (codexInstalledHTTPValidationHeadroom) CompressResponses(ctx context.Context, body []byte, _ HeadroomMode) ([]byte, int, error) {
	if ctx == nil {
		return nil, 0, errors.New("Codex validation Headroom context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, 0, errors.New("decode Codex validation Headroom request")
	}
	request["instructions"] = json.RawMessage(`"Reply with exactly PONG."`)
	transformed, err := json.Marshal(request)
	if err != nil {
		return nil, 0, errors.New("encode Codex validation Headroom request")
	}
	return transformed, 1, nil
}

type codexInstalledHTTPValidationScenario uint8

const (
	codexInstalledHTTPValidationScenarioNormal codexInstalledHTTPValidationScenario = iota
	codexInstalledHTTPValidationScenarioFrozenReplay
	codexInstalledHTTPValidationScenarioHardReplay
	codexInstalledHTTPValidationScenarioTerminalDefault
	codexInstalledHTTPValidationScenarioAdmittedHardLimit
)

type codexInstalledHTTPValidationUpstream struct {
	mu sync.Mutex

	address        string
	server         *http.Server
	listener       net.Listener
	done           chan struct{}
	serveErr       error
	closing        bool
	scenarioByTurn map[string]codexInstalledHTTPValidationScenario
	requestsByTurn map[string]uint64
	turns          map[string]struct{}
	routes         []codexInstalledHTTPValidationRoute
	nextHardReplay bool
	requests       uint64
	responses      uint64
	compact        uint64
	models         uint64
	closeOnce      sync.Once
	closeErr       error
}

type codexInstalledHTTPValidationRoute struct {
	Metadata  CodexTurnMetadata
	AccountID string
	Status    int
}

func startCodexInstalledHTTPValidationUpstream() (*codexInstalledHTTPValidationUpstream, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("start Codex validation synthetic upstream")
	}
	upstream := &codexInstalledHTTPValidationUpstream{
		address:        listener.Addr().String(),
		listener:       listener,
		done:           make(chan struct{}),
		scenarioByTurn: make(map[string]codexInstalledHTTPValidationScenario),
		requestsByTurn: make(map[string]uint64),
		turns:          make(map[string]struct{}),
	}
	upstream.server = &http.Server{
		Handler:           http.HandlerFunc(upstream.serveHTTPRecovering),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
	go upstream.serve()
	return upstream, nil
}

func (upstream *codexInstalledHTTPValidationUpstream) serve() {
	defer close(upstream.done)
	defer func() {
		if recover() != nil {
			upstream.recordServeError(errors.New("Codex validation synthetic upstream panicked"))
		}
	}()
	err := upstream.server.Serve(upstream.listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		upstream.recordServeError(errors.New("Codex validation synthetic upstream stopped"))
	}
}

func (upstream *codexInstalledHTTPValidationUpstream) serveHTTPRecovering(writer http.ResponseWriter, request *http.Request) {
	defer func() {
		if recover() != nil {
			upstream.recordServeError(errors.New("Codex validation synthetic upstream request panicked"))
			writeError(writer, http.StatusInternalServerError, "api_error", "validation upstream unavailable")
		}
	}()
	upstream.serveHTTP(writer, request)
}

func (upstream *codexInstalledHTTPValidationUpstream) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.Method != http.MethodPost || (request.URL.Path != "/responses" && request.URL.Path != "/responses/compact") || request.URL.RawQuery != "" {
		http.NotFound(writer, request)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody+1))
	closeErr := request.Body.Close()
	if err != nil || closeErr != nil || len(body) > maxRequestBody {
		writeError(writer, http.StatusBadRequest, "invalid_request_error", "invalid validation request")
		return
	}
	defer clearBytes(body)
	decoded, err := DecodeCodexRequest(body, request.Header.Get("Content-Encoding"), codexTransportRewriteLimits())
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request_error", "invalid validation request")
		return
	}
	decodedBody := decoded.Decoded()
	defer clearBytes(decodedBody)
	protocol, err := ParseCodexProtocolRequest(decodedBody, request.Header.Get(codexTurnMetadataKey), nil)
	if err != nil || !protocol.Metadata.Strong || extractModel(decodedBody) != "gpt-5.6-sol" {
		writeError(writer, http.StatusBadRequest, "invalid_request_error", "validation request lacks strong turn authority")
		return
	}
	turnKey := codexInstalledHTTPValidationTurnKey(protocol.Metadata.Metadata)
	accountID := request.Header.Get("ChatGPT-Account-ID")
	if turnKey == "" || accountID == "" || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer validation-token-") {
		writeError(writer, http.StatusUnauthorized, "authentication_error", "validation credential unavailable")
		return
	}

	upstream.mu.Lock()
	_, turnSeen := upstream.turns[turnKey]
	if !turnSeen && upstream.nextHardReplay {
		upstream.scenarioByTurn[turnKey] = codexInstalledHTTPValidationScenarioHardReplay
		upstream.nextHardReplay = false
	}
	upstream.requests++
	upstream.models++
	upstream.turns[turnKey] = struct{}{}
	upstream.requestsByTurn[turnKey]++
	requestOrdinal := upstream.requestsByTurn[turnKey]
	scenario := upstream.scenarioByTurn[turnKey]
	if request.URL.Path == "/responses/compact" {
		upstream.compact++
	} else {
		upstream.responses++
	}
	upstream.mu.Unlock()

	reject := false
	switch scenario {
	case codexInstalledHTTPValidationScenarioFrozenReplay, codexInstalledHTTPValidationScenarioHardReplay:
		reject = accountID == "validation-upstream-a"
	case codexInstalledHTTPValidationScenarioTerminalDefault:
		reject = true
	case codexInstalledHTTPValidationScenarioAdmittedHardLimit:
		reject = requestOrdinal >= 2
	}
	if reject {
		upstream.recordRoute(protocol.Metadata.Metadata, accountID, http.StatusTooManyRequests)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`)
		return
	}
	upstream.recordRoute(protocol.Metadata.Metadata, accountID, http.StatusOK)
	if request.URL.Path == "/responses/compact" {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"validation-response","output":[{"type":"compaction","encrypted_content":"validation-state"}]}`)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"validation-response\"}}\n\n")
	_, _ = io.WriteString(writer, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"validation-message\",\"content\":[{\"type\":\"output_text\",\"text\":\"PONG\"}]}}\n\n")
	_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"validation-response\",\"end_turn\":true,\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
}

func (upstream *codexInstalledHTTPValidationUpstream) recordRoute(metadata CodexTurnMetadata, accountID string, status int) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	upstream.routes = append(upstream.routes, codexInstalledHTTPValidationRoute{
		Metadata:  metadata,
		AccountID: accountID,
		Status:    status,
	})
}

func (upstream *codexInstalledHTTPValidationUpstream) armNextNewTurnHardReplay() error {
	if upstream == nil {
		return errors.New("arm Codex validation hard-limit replay")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.nextHardReplay {
		return errors.New("Codex validation hard-limit replay already armed")
	}
	upstream.nextHardReplay = true
	return nil
}

func (upstream *codexInstalledHTTPValidationUpstream) routeHistory() []codexInstalledHTTPValidationRoute {
	if upstream == nil {
		return nil
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]codexInstalledHTTPValidationRoute(nil), upstream.routes...)
}

func (upstream *codexInstalledHTTPValidationUpstream) setScenario(metadata CodexTurnMetadata, scenario codexInstalledHTTPValidationScenario) error {
	key := codexInstalledHTTPValidationTurnKey(metadata)
	if upstream == nil || key == "" {
		return errors.New("set Codex validation synthetic scenario")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if _, exists := upstream.scenarioByTurn[key]; exists {
		return errors.New("Codex validation synthetic scenario already exists")
	}
	upstream.scenarioByTurn[key] = scenario
	return nil
}

func (upstream *codexInstalledHTTPValidationUpstream) healthy() error {
	if upstream == nil {
		return errors.New("Codex validation synthetic upstream unavailable")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return upstream.serveErr
}

func (upstream *codexInstalledHTTPValidationUpstream) recordServeError(err error) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if !upstream.closing && upstream.serveErr == nil {
		upstream.serveErr = err
	}
}

func (upstream *codexInstalledHTTPValidationUpstream) close() error {
	if upstream == nil {
		return nil
	}
	upstream.closeOnce.Do(func() {
		upstream.mu.Lock()
		upstream.closing = true
		upstream.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		shutdownErr := upstream.server.Shutdown(ctx)
		cancel()
		listenerErr := upstream.listener.Close()
		if errors.Is(listenerErr, net.ErrClosed) {
			listenerErr = nil
		}
		select {
		case <-upstream.done:
		case <-time.After(2 * time.Second):
			shutdownErr = errors.Join(shutdownErr, errors.New("Codex validation synthetic upstream did not stop"))
		}
		upstream.mu.Lock()
		serveErr := upstream.serveErr
		upstream.mu.Unlock()
		upstream.closeErr = errors.Join(shutdownErr, listenerErr, serveErr)
	})
	return upstream.closeErr
}

func codexInstalledHTTPValidationTurnKey(metadata CodexTurnMetadata) string {
	if metadata.SessionID == "" || metadata.ThreadID == "" || metadata.TurnID == "" {
		return ""
	}
	return metadata.SessionID + "\x00" + metadata.ThreadID + "\x00" + metadata.TurnID
}

type codexInstalledHTTPValidationRoundTripper struct {
	target    string
	transport *http.Transport
}

func newCodexInstalledHTTPValidationRoundTripper(target string) *codexInstalledHTTPValidationRoundTripper {
	transport := &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		DisableKeepAlives:     false,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	result := &codexInstalledHTTPValidationRoundTripper{target: target, transport: transport}
	transport.DialContext = result.dialContext
	return result
}

func (transport *codexInstalledHTTPValidationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport == nil || transport.transport == nil || request == nil || request.URL == nil ||
		request.URL.Scheme != "http" || request.URL.Host != transport.target || request.URL.User != nil {
		return nil, errors.New("Codex validation network target rejected")
	}
	return transport.transport.RoundTrip(request)
}

func (transport *codexInstalledHTTPValidationRoundTripper) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if ctx == nil {
		return nil, errors.New("Codex validation dial context unavailable")
	}
	if address != transport.target || (network != "tcp" && network != "tcp4" && network != "tcp6") {
		return nil, errors.New("Codex validation network target rejected")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	return dialer.DialContext(ctx, "tcp4", transport.target)
}

func validateCodexInstalledHTTPValidationLoopbackAddress(address string) (string, error) {
	trimmed := strings.TrimSpace(address)
	host, portText, err := net.SplitHostPort(trimmed)
	if err != nil || strings.ContainsAny(trimmed, "/?#@") {
		return "", errors.New("Codex validation listener address is invalid")
	}
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(portText)
	if portErr != nil || ip == nil || !ip.IsLoopback() || port < 1 || port > 65535 {
		return "", errors.New("Codex validation listener must be exact loopback TCP")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

type codexInstalledHTTPValidationExercise struct {
	mu         sync.Mutex
	run        bool
	core       *codexInstalledHTTPValidationRuntimeCore
	address    string
	localToken string
	client     *http.Client
}

type codexInstalledHTTPValidationTrafficCase struct {
	metadata CodexTurnMetadata
	scenario codexInstalledHTTPValidationScenario
	capacity [3]int
	compact  bool
	requests int
}

func (exercise *codexInstalledHTTPValidationExercise) Run(ctx context.Context) (returnErr error) {
	if ctx == nil {
		return errors.New("Codex installed HTTP validation exercise context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if exercise == nil || exercise.core == nil || exercise.client == nil || exercise.core.upstream == nil {
		return errors.New("Codex installed HTTP validation exercise unavailable")
	}
	exercise.mu.Lock()
	if exercise.run {
		exercise.mu.Unlock()
		return errors.New("Codex installed HTTP validation exercise already ran")
	}
	exercise.run = true
	exercise.mu.Unlock()
	defer func() {
		if recover() != nil {
			returnErr = errors.New("Codex installed HTTP validation exercise panicked")
		}
	}()

	for _, trafficCase := range codexInstalledHTTPValidationTrafficCases() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := exercise.core.upstream.healthy(); err != nil {
			return errors.New("Codex validation synthetic upstream unavailable")
		}
		if err := exercise.core.setCapacity(trafficCase.capacity[0], trafficCase.capacity[1], trafficCase.capacity[2]); err != nil {
			return err
		}
		if err := exercise.core.upstream.setScenario(trafficCase.metadata, trafficCase.scenario); err != nil {
			return err
		}
		for requestIndex := 0; requestIndex < trafficCase.requests; requestIndex++ {
			wantStatus := http.StatusOK
			if trafficCase.scenario == codexInstalledHTTPValidationScenarioTerminalDefault ||
				(trafficCase.scenario == codexInstalledHTTPValidationScenarioAdmittedHardLimit && requestIndex > 0) {
				wantStatus = http.StatusTooManyRequests
			}
			if err := exercise.send(ctx, trafficCase.metadata, trafficCase.compact, wantStatus); err != nil {
				return err
			}
		}
	}
	if err := exercise.core.upstream.healthy(); err != nil {
		return errors.New("Codex validation synthetic upstream unavailable")
	}
	return nil
}

func codexInstalledHTTPValidationTrafficCases() []codexInstalledHTTPValidationTrafficCase {
	turn := func(lane string, turn int, kind CodexRequestKind, phase CodexCompactionPhase) CodexTurnMetadata {
		return CodexTurnMetadata{
			SessionID:       "validation-session-" + lane,
			ThreadID:        "validation-thread-" + lane,
			TurnID:          fmt.Sprintf("validation-turn-%02d", turn),
			RequestKind:     kind,
			CompactionPhase: phase,
		}
	}
	cases := []codexInstalledHTTPValidationTrafficCase{
		{turn("frozen", 1, CodexRequestTurn, ""), codexInstalledHTTPValidationScenarioFrozenReplay, [3]int{100, 90, 10}, false, 2},
		{turn("hard-replay", 1, CodexRequestTurn, ""), codexInstalledHTTPValidationScenarioHardReplay, [3]int{100, 90, 10}, false, 2},
		{turn("warm", 1, CodexRequestTurn, ""), codexInstalledHTTPValidationScenarioNormal, [3]int{50, 100, 10}, false, 2},
		{turn("warm", 2, CodexRequestTurn, ""), codexInstalledHTTPValidationScenarioNormal, [3]int{100, 50, 10}, false, 2},
		{turn("fallback", 1, CodexRequestTurn, ""), codexInstalledHTTPValidationScenarioNormal, [3]int{50, 100, 10}, false, 2},
		{turn("fallback", 2, CodexRequestTurn, ""), codexInstalledHTTPValidationScenarioNormal, [3]int{100, 0, 10}, false, 2},
		{turn("admitted", 1, CodexRequestTurn, ""), codexInstalledHTTPValidationScenarioAdmittedHardLimit, [3]int{100, 50, 10}, false, 2},
		{turn("terminal", 1, CodexRequestTurn, ""), codexInstalledHTTPValidationScenarioTerminalDefault, [3]int{0, 0, 0}, false, 1},
		{turn("compact", 1, CodexRequestCompaction, CodexCompactionStandalone), codexInstalledHTTPValidationScenarioNormal, [3]int{100, 90, 10}, true, 2},
	}
	for index := 1; index <= 11; index++ {
		requests := 2
		if index == 1 {
			requests = 3
		}
		cases = append(cases, codexInstalledHTTPValidationTrafficCase{
			metadata: turn(fmt.Sprintf("ordinary-%02d", index), 1, CodexRequestTurn, ""),
			scenario: codexInstalledHTTPValidationScenarioNormal,
			capacity: [3]int{100, 90, 10},
			requests: requests,
		})
	}
	return cases
}

func (exercise *codexInstalledHTTPValidationExercise) send(ctx context.Context, metadata CodexTurnMetadata, compact bool, wantStatus int) error {
	body, err := json.Marshal(map[string]any{
		"model": "gpt-5.6-sol",
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "Reply with exactly PONG."}},
		}},
		"stream": true,
		"client_metadata": map[string]any{
			codexTurnMetadataKey: metadata,
		},
	})
	if err != nil {
		return errors.New("encode Codex installed HTTP validation request")
	}
	encoded, err := EncodeCodexRequest(body, "zstd", codexTransportRewriteLimits())
	clearBytes(body)
	if err != nil {
		return errors.New("compress Codex installed HTTP validation request")
	}
	defer clearBytes(encoded)
	path := "/responses"
	if compact {
		path = "/responses/compact"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+exercise.address+path, bytes.NewReader(encoded))
	if err != nil {
		return errors.New("construct Codex installed HTTP validation request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+exercise.localToken)
	response, err := exercise.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("send Codex installed HTTP validation request")
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, codexProtocolMaxBytes+1))
	closeErr := response.Body.Close()
	defer clearBytes(responseBody)
	if readErr != nil || closeErr != nil || len(responseBody) == 0 || len(responseBody) > codexProtocolMaxBytes || response.StatusCode != wantStatus {
		return errors.New("Codex installed HTTP validation response mismatch")
	}
	return nil
}
