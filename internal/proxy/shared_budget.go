package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

// SharedBudgetRoute binds a subscription catalogue to its Responses endpoint.
// The session and workspace headers are optional. CQ builds the upstream request and owns the
// stream and continuation so an adapter never receives downstream credentials.
type SharedBudgetRoute struct {
	Name            string
	URL             string
	Credential      func(context.Context) (SharedBudgetCredential, error)
	Credentials     func(context.Context) ([]SharedBudgetCredential, error)
	CredentialFor   func(context.Context, modelregistry.SharedBudgetCredentialScope) (SharedBudgetCredential, error)
	Preferred       func(context.Context) (modelregistry.SharedBudgetCredentialScope, bool)
	Transport       http.RoundTripper
	SessionHeader   string
	WorkspaceHeader string
	AllowFallback   bool
	// RequireCatalogueScope rejects entries without live workspace authority.
	RequireCatalogueScope bool
	// OmitStreamOptions applies to Responses translators that reject this field.
	OmitStreamOptions bool
	// OmitEmptyCallIDOutputs drops unpaired tool results rejected by strict translators.
	OmitEmptyCallIDOutputs bool
	// PlaintextCollaboration maps Codex task arguments and messages at the provider boundary.
	PlaintextCollaboration bool
}

// SharedBudgetCredential carries material for one subscription request. An
// authenticated Account identifies an OAuth member across token refreshes.
// A key has an empty Account and is identified by its bearer instead.
type SharedBudgetCredential struct {
	Bearer      string
	Account     string
	WorkspaceID string
}

type sharedBudgetSelection struct {
	id         modelregistry.Provider
	route      SharedBudgetRoute
	credential SharedBudgetCredential
	model      string
	fallback   bool
	nativeAPI  string
	bound      bool
}

type sharedBudgetRouteError struct {
	status  int
	message string
}

func (e sharedBudgetRouteError) Error() string { return e.message }

func (s *Server) routeCodexModel(model string) Provider {
	provider := RouteModelWithCatalog(model, s.Catalog)
	// The login-scoped Codex picker preserves native entitlements that may
	// be absent from the pooled registry. Keep native priority for Codex IDs
	// even when only the shared subscription published the model.
	if provider == ProviderSharedBudget && RouteModel(model) == ProviderCodex {
		return ProviderCodex
	}
	// Anthropic's native route speaks Messages. A subscription that offers
	// the same model over Responses can serve the Codex client directly.
	if provider == ProviderClaude && s.Catalog != nil && len(s.Catalog.SharedBudgetModels(ParseModel(model))) != 0 {
		return ProviderSharedBudget
	}
	return provider
}

func (s *Server) hasSharedBudgetCatalog() bool {
	return s.Catalog != nil && s.Catalog.HasSharedBudgetModels()
}

func (s *Server) selectSharedBudgetRoute(ctx context.Context, model string, fallback, advance bool) (sharedBudgetSelection, bool) {
	if s.Catalog == nil || len(s.SharedBudgetRoutes) == 0 {
		return sharedBudgetSelection{}, false
	}
	var candidates []sharedBudgetSelection
	var preferred modelregistry.Provider
	credentialsByProvider := make(map[modelregistry.Provider][]SharedBudgetCredential)
	for _, entry := range s.Catalog.SharedBudgetModels(ParseModel(model)) {
		route, ok := s.SharedBudgetRoutes[entry.Provider]
		if !ok || route.Credential == nil && route.Credentials == nil || entry.SharedBudget == nil || fallback && !route.AllowFallback {
			continue
		}
		if preferred != "" && preferred != entry.Provider {
			break
		}
		scope := entry.SharedBudget.CredentialScope
		unscoped := scope == (modelregistry.SharedBudgetCredentialScope{})
		if unscoped && route.RequireCatalogueScope {
			continue
		}
		credentials, loaded := credentialsByProvider[entry.Provider]
		if !loaded {
			var err error
			credentials, err = route.credentials(ctx)
			if err != nil {
				credentials = nil
			}
			credentialsByProvider[entry.Provider] = credentials
		}
		for _, credential := range credentials {
			if credential.Bearer != "" && (unscoped || scope == credential.binding()) {
				preferred = entry.Provider
				candidates = append(candidates, sharedBudgetSelection{id: entry.Provider, route: route, credential: credential, model: entry.ID, fallback: fallback})
				break
			}
		}
	}
	if len(candidates) == 0 {
		return sharedBudgetSelection{}, false
	}
	if key, _ := ctx.Value(sharedBudgetAffinityContextKey{}).(string); key != "" {
		if bound, found := s.sharedBudgetTurns.session(key, model); found {
			for _, candidate := range candidates {
				if candidate.id == bound.Provider && candidate.credential.binding() == bound.Credential {
					return candidate, true
				}
			}
			if ctx.Value(sharedBudgetRebindContextKey{}) == nil {
				return sharedBudgetSelection{}, false
			}
		}
	}
	next := s.sharedBudgetNext.Load()
	if advance {
		next = s.sharedBudgetNext.Add(1) - 1
	}
	return candidates[int(next%uint64(len(candidates)))], true
}

type sharedBudgetAffinityContextKey struct{}
type sharedBudgetRebindContextKey struct{}

func withSharedBudgetRebind(ctx context.Context, body []byte) context.Context {
	if portableSharedBudgetInput(body) {
		return context.WithValue(ctx, sharedBudgetRebindContextKey{}, true)
	}
	return ctx
}

func withSharedBudgetAffinity(ctx context.Context, header http.Header) context.Context {
	if key := sharedBudgetAffinity(header); key != "" {
		return context.WithValue(ctx, sharedBudgetAffinityContextKey{}, key)
	}
	return ctx
}

func (r SharedBudgetRoute) credentials(ctx context.Context) ([]SharedBudgetCredential, error) {
	if r.Credentials != nil {
		return r.Credentials(ctx)
	}
	credential, err := r.Credential(ctx)
	if err != nil {
		return nil, err
	}
	return []SharedBudgetCredential{credential}, nil
}

func (r SharedBudgetRoute) credentialFor(ctx context.Context, scope modelregistry.SharedBudgetCredentialScope) (SharedBudgetCredential, error) {
	if r.CredentialFor != nil {
		return r.CredentialFor(ctx, scope)
	}
	credentials, err := r.credentials(ctx)
	if err != nil {
		return SharedBudgetCredential{}, err
	}
	for _, credential := range credentials {
		if credential.Bearer != "" && credential.binding() == scope {
			return credential, nil
		}
	}
	return SharedBudgetCredential{}, errSharedBudgetContinuation
}

func (s *Server) sharedBudgetResponse(ctx context.Context, selected *sharedBudgetSelection, session string, header http.Header, body []byte, compactEndpoint bool) (*http.Response, *zaiCompactResponse, error) {
	route := selected.route
	originalBody := body
	scope := selected.credential.binding()
	models := s.Catalog.SharedBudgetModels(selected.model)
	entry := slices.IndexFunc(models, func(entry modelregistry.Entry) bool {
		return entry.Provider == selected.id && entry.SharedBudget != nil &&
			(!route.RequireCatalogueScope || entry.SharedBudget.CredentialScope == scope)
	})
	if route.RequireCatalogueScope && entry < 0 {
		return nil, nil, sharedBudgetRouteError{409, route.Name + " model is no longer available for this subscription"}
	}
	selected.nativeAPI = ""
	if entry >= 0 {
		selected.nativeAPI = models[entry].SharedBudget.NativeAPI
	}
	if len(body) > maxRequestBody {
		return nil, nil, sharedBudgetRouteError{413, route.Name + " request body exceeds 10 MiB"}
	}
	body, _, err := normaliseCQCompactionInput(body, s.cqCompactionToken())
	if err != nil {
		return nil, nil, sharedBudgetRouteError{400, "invalid CQ compaction input"}
	}
	if route.PlaintextCollaboration {
		body, err = normaliseSubscriptionAgentMessages(body, route.Name)
		if errors.Is(err, errEncryptedSubscriptionAgentMessage) {
			return nil, nil, err
		}
		if err != nil {
			return nil, nil, sharedBudgetRouteError{400, err.Error()}
		}
	}
	trigger, err := zaiCompactionRequested(body)
	if err != nil {
		return nil, nil, sharedBudgetRouteError{400, "invalid " + route.Name + " compaction trigger"}
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(body, &input) != nil || input == nil {
		return nil, nil, sharedBudgetRouteError{400, "invalid " + route.Name + " request"}
	}
	if entry >= 0 {
		nativeAPI := selected.nativeAPI
		if previous := bytes.TrimSpace(input["previous_response_id"]); len(previous) != 0 &&
			!bytes.Equal(previous, []byte("null")) && !bytes.Equal(previous, []byte(`""`)) &&
			nativeAPI != "" && nativeAPI != "responses" {
			return nil, nil, sharedBudgetRouteError{409, route.Name + " requires full input for translated Responses models"}
		}
	}
	_, hasAccessPrograms := input["access_programs"]
	if hasAccessPrograms || input["params"] != nil {
		// Scan the original bytes so rewriting history cannot hide duplicate declarations.
		if !standardResponsesAccessPrograms(originalBody) {
			return nil, nil, sharedBudgetRouteError{400, route.Name + " cannot enforce the requested access programme"}
		}
	}
	clientModel := input["model"]
	model := extractModel(body)
	upstreamModel := selected.model
	if !strings.EqualFold(ParseModel(model), upstreamModel) {
		return nil, nil, sharedBudgetRouteError{400, route.Name + " connection is bound to a different model"}
	}
	compacting := compactEndpoint || trigger
	if compacting {
		if !portableSharedBudgetInput(body) {
			return nil, nil, sharedBudgetRouteError{400, route.Name + " compaction requires full input"}
		}
		body, err = prepareCQCompactionRequest(body, trigger)
		if err != nil {
			return nil, nil, sharedBudgetRouteError{400, "invalid " + route.Name + " compaction input"}
		}
		input = nil
		_ = json.Unmarshal(body, &input)
		// The selected model supplies its own reasoning default.
		delete(input, "reasoning")
	}
	_, hasStreamOptions := input["stream_options"]
	omitStreamOptions := route.OmitStreamOptions && hasStreamOptions
	toolSchemasChanged := false
	if route.PlaintextCollaboration {
		if tools, changed := plaintextCollaborationSchemas(input["tools"], ""); changed {
			input["tools"] = tools
			toolSchemasChanged = true
		}
	}
	omitEmptyCallIDOutputs := false
	if route.OmitEmptyCallIDOutputs {
		filtered, omitted := withoutEmptyCallIDOutputs(input["input"])
		if omitted {
			input["input"] = filtered
			omitEmptyCallIDOutputs = true
		}
	}
	if _, hasClientMetadata := input["client_metadata"]; compacting || hasClientMetadata || model != upstreamModel || omitStreamOptions || omitEmptyCallIDOutputs || toolSchemasChanged || hasAccessPrograms {
		delete(input, "client_metadata")
		delete(input, "access_programs")
		if omitStreamOptions {
			delete(input, "stream_options")
		}
		input["model"], _ = json.Marshal(upstreamModel)
		body, err = json.Marshal(input)
		if err != nil || len(body) > maxRequestBody {
			return nil, nil, sharedBudgetRouteError{413, route.Name + " request body exceeds 10 MiB"}
		}
	}
	if s.CacheProfiler != nil {
		if identity, _ := ctx.Value(cacheProfileSessionContextKey{}).(cacheProfileRequestIdentity); identity.session == "" {
			ctx = context.WithValue(ctx, cacheProfileSessionContextKey{}, cacheProfileRequestIdentity{session: session})
		}
		ctx = context.WithValue(ctx, cacheProfileRouteContextKey{}, cacheProfileRouteIdentity{route: string(selected.id), account: string(scope[:])})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, route.URL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, sharedBudgetRouteError{500, "cannot create " + route.Name + " request"}
	}
	req.Header.Set("Authorization", "Bearer "+selected.credential.Bearer)
	if route.WorkspaceHeader != "" && selected.credential.WorkspaceID != "" {
		req.Header.Set(route.WorkspaceHeader, selected.credential.WorkspaceID)
	}
	req.Header.Set("Content-Type", "application/json")
	if string(input["stream"]) == "true" {
		req.Header.Set("Accept", "text/event-stream")
	}
	if route.SessionHeader != "" {
		req.Header.Set(route.SessionHeader, session)
	}
	client := &http.Client{Transport: s.CacheProfiler.WrapTransport(route.Transport), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, sharedBudgetRouteError{502, route.Name + " request failed"}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 && !compacting {
		s.recordSharedBudgetTurn(header, originalBody, selected.binding())
	}
	if !compacting || response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, nil, nil
	}
	defer response.Body.Close()
	completed, err := readZAICompactionSummary(response.Body)
	if err != nil {
		return nil, nil, sharedBudgetRouteError{502, "invalid " + route.Name + " compaction response"}
	}
	sealed, err := sealCQCompactionSummary(s.cqCompactionToken(), clientModel, completed)
	if err != nil {
		return nil, nil, sharedBudgetRouteError{502, "invalid " + route.Name + " compaction summary"}
	}
	if compactEndpoint {
		sealed.Object = "response.compaction"
	}
	return nil, &sealed, nil
}

// Standard mode carries no model access entitlement. Named or unknown
// programmes must never be removed to get a third-party request accepted.
func standardResponsesAccessPrograms(body []byte) bool {
	scanner := codexFrozenJSONScanner{source: body}
	fields, err := scanner.scanRequest()
	if err != nil || fields.paramsAccessPrograms.present {
		return false
	}
	if !fields.rootAccessPrograms.present {
		return true
	}
	var compact bytes.Buffer
	if json.Compact(&compact, fields.rootAccessPrograms.bytes(body)) != nil {
		return false
	}
	switch compact.String() {
	case "null", "{}", `{"cyber":"standard"}`:
		return true
	default:
		return false
	}
}

func withoutEmptyCallIDOutputs(raw json.RawMessage) (json.RawMessage, bool) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || items == nil {
		return raw, false
	}
	kept := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var call struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(item, &call) == nil && (call.Type == "function_call_output" || call.Type == "custom_tool_call_output") && call.CallID == "" {
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == len(items) {
		return raw, false
	}
	filtered, _ := json.Marshal(kept)
	return filtered, true
}

func sharedBudgetSession(header http.Header, preferred string) string {
	for _, name := range []string{preferred, "Session-Id", "Session_id", "X-Session-Id"} {
		if session := header.Get(name); session != "" {
			return session
		}
	}
	return "cq-" + rand.Text()
}

func (s *Server) serveSharedBudgetHTTP(w http.ResponseWriter, r *http.Request, body []byte) {
	selected, ok := s.selectSharedBudgetRoute(withSharedBudgetRebind(withSharedBudgetAffinity(r.Context(), r.Header), body), extractModel(body), false, true)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "api_error", "subscription credential unavailable")
		return
	}
	s.serveSelectedSharedBudgetHTTP(w, r, body, selected)
}

func (s *Server) serveSelectedSharedBudgetHTTP(w http.ResponseWriter, r *http.Request, body []byte, selected sharedBudgetSelection) {
	model := extractModel(body)
	s.observeHarness(r.Header, model, body)
	compactEndpoint := strings.HasSuffix(r.URL.Path, "/responses/compact")
	ctx := r.Context()
	if s.CacheProfiler != nil {
		ctx = withCacheProfileRequestSession(ctx, r.Header, body)
	}
	canRebind := !selected.bound && !compactEndpoint && portableSharedBudgetInput(body)
	if canRebind {
		if preferred, ok := s.preferredSharedBudgetAccount(ctx, selected, selected.credential.binding()); ok {
			selected = preferred
		}
	}
	session := sharedBudgetSession(r.Header, selected.route.SessionHeader)
	response, compact, err := s.sharedBudgetResponse(ctx, &selected, session, r.Header, body, compactEndpoint)
	tried := map[modelregistry.SharedBudgetCredentialScope]bool{selected.credential.binding(): true}
	for canRebind && err == nil && response != nil && firstPartyRejectedBeforeGeneration(response.StatusCode) {
		alternate, ok := s.matchSharedBudgetAccount(ctx, selected, func(scope modelregistry.SharedBudgetCredentialScope) bool { return !tried[scope] })
		if !ok {
			break
		}
		response.Body.Close()
		selected = alternate
		tried[selected.credential.binding()] = true
		response, compact, err = s.sharedBudgetResponse(ctx, &selected, session, r.Header, body, false)
	}
	if err != nil {
		if errors.Is(err, errEncryptedSubscriptionAgentMessage) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		var route sharedBudgetRouteError
		if errors.As(err, &route) {
			writeError(w, route.status, "api_error", route.message)
			return
		}
		writeError(w, 502, "api_error", selected.route.Name+" request failed")
		return
	}
	if compact != nil {
		var req struct{ Stream bool }
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		if req.Stream && !compactEndpoint {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range zaiCompactionEvents(*compact) {
				data, _ := json.Marshal(event)
				_, _ = fmt.Fprintf(zaiFlushWriter{w}, "event: %s\ndata: %s\n\n", event["type"], data)
			}
		} else {
			_ = json.NewEncoder(w).Encode(compact)
		}
		return
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Cache-Control", "Retry-After", "X-Request-Id"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		writeError(w, response.StatusCode, "api_error", selected.route.Name+" request rejected")
		return
	}
	s.relaySharedBudgetHTTP(w, response, selected, body)
}

func (s *Server) preferredSharedBudgetAccount(ctx context.Context, selected sharedBudgetSelection, current modelregistry.SharedBudgetCredentialScope) (sharedBudgetSelection, bool) {
	if selected.route.Preferred == nil {
		return sharedBudgetSelection{}, false
	}
	preferred, ok := selected.route.Preferred(ctx)
	if !ok || preferred == current {
		return sharedBudgetSelection{}, false
	}
	return s.matchSharedBudgetAccount(ctx, selected, func(scope modelregistry.SharedBudgetCredentialScope) bool { return scope == preferred })
}

func (s *Server) matchSharedBudgetAccount(ctx context.Context, selected sharedBudgetSelection, eligible func(modelregistry.SharedBudgetCredentialScope) bool) (sharedBudgetSelection, bool) {
	for _, entry := range s.Catalog.SharedBudgetModels(selected.model) {
		if entry.Provider != selected.id || entry.SharedBudget == nil || !eligible(entry.SharedBudget.CredentialScope) {
			continue
		}
		credential, err := selected.route.credentialFor(ctx, entry.SharedBudget.CredentialScope)
		if err == nil && credential.Bearer != "" && credential.binding() == entry.SharedBudget.CredentialScope {
			selected.credential = credential
			selected.model = entry.ID
			return selected, true
		}
	}
	return sharedBudgetSelection{}, false
}

func (s *Server) serveSharedBudgetWebSocket(ctx context.Context, downstream *websocket.Conn, header http.Header, kind int, first []byte, selected sharedBudgetSelection) error {
	bridge := s.sharedBudgetBridge(selected, header)
	bridge.AcceptsModel = func(model string) bool {
		return strings.EqualFold(ParseModel(model), selected.model)
	}
	return s.serveResponsesHTTPWebSocket(ctx, downstream, header, kind, first, bridge)
}

func (s *Server) sharedBudgetBridge(selected sharedBudgetSelection, header http.Header) responsesHTTPBridge {
	session := sharedBudgetSession(header, selected.route.SessionHeader)
	binding := selected.binding()
	selected.credential = SharedBudgetCredential{}
	return responsesHTTPBridge{
		Name: selected.route.Name, PrewarmPrefix: "cq_" + strings.ReplaceAll(string(selected.id), "-", "_") + "_prewarm_",
		Stream: func(ctx context.Context, downstream *websocket.Conn, header http.Header, body []byte) (string, []json.RawMessage, error) {
			return s.streamSharedBudget(ctx, downstream, header, &selected, &binding, session, body)
		},
		Compaction: zaiCompactionRequested,
	}
}

func (s *Server) streamSharedBudget(ctx context.Context, downstream *websocket.Conn, header http.Header, selected *sharedBudgetSelection, binding *sharedBudgetBoundState, session string, body []byte) (string, []json.RawMessage, error) {
	bound, found, complete := s.sharedBudgetRequestBinding(body)
	if !complete || (found && bound != *binding) {
		return "", nil, errSharedBudgetContinuation
	}
	canRebind := !selected.bound && !found && portableSharedBudgetInput(body)
	var preferred sharedBudgetSelection
	var hasPreference bool
	if canRebind {
		preferred, hasPreference = s.preferredSharedBudgetAccount(ctx, *selected, binding.Credential)
	}
	if hasPreference {
		*selected = preferred
		*binding = selected.binding()
	} else {
		credential, err := selected.route.credentialFor(ctx, binding.Credential)
		if err != nil || credential.Bearer == "" || credential.binding() != binding.Credential {
			return "", nil, errSharedBudgetContinuation
		}
		selected.credential = credential
	}
	s.observeHarness(header, extractModel(body), body)
	if s.CacheProfiler != nil {
		ctx = withCacheProfileRequestSession(ctx, header, body)
	}
	response, compact, err := s.sharedBudgetResponse(ctx, selected, session, header, body, false)
	if err != nil {
		return "", nil, err
	}
	tried := map[modelregistry.SharedBudgetCredentialScope]bool{selected.credential.binding(): true}
	for canRebind && response != nil && firstPartyRejectedBeforeGeneration(response.StatusCode) {
		alternate, ok := s.matchSharedBudgetAccount(ctx, *selected, func(scope modelregistry.SharedBudgetCredentialScope) bool { return !tried[scope] })
		if !ok {
			break
		}
		response.Body.Close()
		*selected = alternate
		*binding = alternate.binding()
		tried[selected.credential.binding()] = true
		response, compact, err = s.sharedBudgetResponse(ctx, selected, session, header, body, false)
		if err != nil {
			return "", nil, err
		}
	}
	if compact != nil {
		for _, event := range zaiCompactionEvents(*compact) {
			data, _ := json.Marshal(event)
			if err := downstream.WriteMessage(websocket.TextMessage, data); err != nil {
				return "", nil, err
			}
		}
		return compact.ID, compact.Output, nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := json.Marshal(map[string]any{"type": "error", "status": response.StatusCode, "error": map[string]string{"type": "api_error", "message": selected.route.Name + " request rejected"}})
		_ = downstream.WriteMessage(websocket.TextMessage, data)
		return "", nil, errors.New(selected.route.Name + " stream rejected")
	}
	emit := func(kind string, data []byte) error {
		if kind == "response.completed" {
			var event struct{ Response json.RawMessage }
			if json.Unmarshal(data, &event) == nil {
				s.recordSharedBudgetResponse(*binding, event.Response)
			}
		}
		return downstream.WriteMessage(websocket.TextMessage, data)
	}
	normalise := sharedBudgetStreamNormaliser(*selected, body, emit)
	if normalise == nil {
		normalise = func(data []byte) ([]byte, error) { return data, nil }
	}
	return relayResponsesSSE(response.Body, selected.route.Name, normalise, emit)
}

func portableSharedBudgetInput(body []byte) bool {
	var request struct {
		PreviousResponseID string          `json:"previous_response_id"`
		Conversation       json.RawMessage `json:"conversation"`
		Input              json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil || request.PreviousResponseID != "" ||
		(len(request.Conversation) != 0 && string(request.Conversation) != "null") {
		return false
	}
	input, err := responsesBridgeInput(request.Input, "subscription")
	if err != nil {
		return false
	}
	for _, raw := range input {
		var item struct {
			Type                  string            `json:"type"`
			EncryptedContent      string            `json:"encrypted_content"`
			EncryptedFunctionArgs []json.RawMessage `json:"encrypted_function_args"`
			EncryptedArguments    []json.RawMessage `json:"encrypted_arguments"`
			Output                json.RawMessage   `json:"output"`
		}
		if json.Unmarshal(raw, &item) != nil || item.EncryptedContent != "" ||
			len(item.EncryptedFunctionArgs) != 0 || len(item.EncryptedArguments) != 0 ||
			item.Type == "item_reference" || item.Type == "compaction" {
			return false
		}
		if item.Type != "function_call_output" && item.Type != "custom_tool_call_output" {
			continue
		}
		output := bytes.TrimSpace(item.Output)
		if len(output) == 0 || output[0] != '[' {
			continue
		}
		var parts []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		}
		if json.Unmarshal(output, &parts) != nil {
			return false
		}
		for _, part := range parts {
			if part.Type == "encrypted_content" || part.EncryptedContent != "" {
				return false
			}
		}
	}
	return true
}

func firstPartyRejectedBeforeGeneration(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusUnauthorized
}

type sharedBudgetFallbackDisabled struct{}

var errSharedBudgetFallback = errors.New("first-party subscription rejected before generation")
var errSharedBudgetContinuation = errors.New("subscription continuation unavailable; retry with full readable input")

func (s *Server) selectSharedBudgetFallback(ctx context.Context, body []byte) (sharedBudgetSelection, bool) {
	return s.sharedBudgetFallback(ctx, body, true)
}

func (s *Server) sharedBudgetFallback(ctx context.Context, body []byte, advance bool) (sharedBudgetSelection, bool) {
	if ctx.Err() != nil || ctx.Value(sharedBudgetFallbackDisabled{}) != nil || len(s.SharedBudgetRoutes) == 0 ||
		!portableSharedBudgetInput(body) {
		return sharedBudgetSelection{}, false
	}
	return s.selectSharedBudgetRoute(withSharedBudgetRebind(ctx, body), extractModel(body), true, advance)
}

// hasSharedBudgetFallback avoids reading a successful response's SSE prelude
// where no fallback is configured, and leaves OAuth refresh until rejection.
func (s *Server) hasSharedBudgetFallback(ctx context.Context, body []byte) bool {
	if ctx.Err() != nil || ctx.Value(sharedBudgetFallbackDisabled{}) != nil || s.Catalog == nil ||
		len(s.SharedBudgetRoutes) == 0 || !portableSharedBudgetInput(body) {
		return false
	}
	for _, entry := range s.Catalog.SharedBudgetModels(ParseModel(extractModel(body))) {
		if route := s.SharedBudgetRoutes[entry.Provider]; route.AllowFallback && route.Credential != nil {
			return true
		}
	}
	return false
}

func (s *Server) canFallbackToSharedBudget(ctx context.Context, body []byte) bool {
	_, ok := s.sharedBudgetFallback(ctx, body, false)
	return ok
}

func (s *Server) withSharedBudgetWebSocketFallback(primary func(context.Context, *websocket.Conn, http.Header, []byte) (string, []json.RawMessage, error)) func(context.Context, *websocket.Conn, http.Header, []byte) (string, []json.RawMessage, error) {
	var primarySelected bool
	var fallback responsesHTTPBridge
	var fallbackModel string
	return func(ctx context.Context, downstream *websocket.Conn, header http.Header, body []byte) (string, []json.RawMessage, error) {
		if fallback.Stream != nil {
			if !strings.EqualFold(ParseModel(extractModel(body)), ParseModel(fallbackModel)) {
				return "", nil, errors.New("subscription model changed; reconnect with full input")
			}
			return fallback.Stream(ctx, downstream, header, body)
		}
		if primarySelected {
			ctx = context.WithValue(ctx, sharedBudgetFallbackDisabled{}, true)
		}
		if s.knownSharedBudgetState(body) {
			return "", nil, errSharedBudgetContinuation
		}
		// Codex can prewarm before sending the turn metadata. The
		// synthetic prewarm does not commit this socket to the primary route.
		if !primarySelected {
			model := extractModel(body)
			if selected, bound := s.sharedBudgetTurnRoute(ctx, header, body, model); bound {
				if selected.id == "" {
					return "", nil, errSharedBudgetContinuation
				}
				fallbackModel = model
				fallback = s.sharedBudgetBridge(selected, header)
				return fallback.Stream(ctx, downstream, header, body)
			}
		}
		id, output, err := primary(ctx, downstream, header, body)
		if errors.Is(err, errSharedBudgetFallback) {
			body, _, err = normaliseCQCompactionInput(body, s.cqCompactionToken())
			if err != nil {
				return "", nil, err
			}
			fallbackModel = extractModel(body)
			selected, ok := s.selectSharedBudgetFallback(ctx, body)
			if !ok {
				return "", nil, errors.New("subscription credential unavailable")
			}
			fallback = s.sharedBudgetBridge(selected, header)
			return fallback.Stream(ctx, downstream, header, body)
		}
		primarySelected = err == nil
		return id, output, err
	}
}

func decodeSharedBudgetFallbackRequest(encoded []byte, header http.Header) []byte {
	encoding, err := parseCodexContentEncoding(header)
	if err != nil {
		return nil
	}
	c, err := DecodeCodexRequest(encoded, encoding, codexHTTPZstdLimits())
	if err != nil {
		return nil
	}
	return c.Decoded()
}

func (s *Server) tryCodexSharedBudgetFallback(w http.ResponseWriter, r *http.Request, encoded []byte) (bool, string) {
	if len(s.SharedBudgetRoutes) == 0 {
		return false, ""
	}
	body := decodeSharedBudgetFallbackRequest(encoded, r.Header)
	selected, ok := s.selectSharedBudgetFallback(r.Context(), body)
	if !ok {
		return false, ""
	}
	s.serveSelectedSharedBudgetHTTP(w, r, body, selected)
	return true, extractModel(body)
}

type codexWSSubscriptionFallbackKey struct{}

type codexWSSubscriptionFallback struct {
	selected   bool
	nativeTurn bool
	canUse     func(context.Context, []byte) bool
	hasState   func([]byte) bool
	serve      func(context.Context, int, []byte) error
}

func (f *codexWSSubscriptionFallback) eligible(ctx context.Context, pending *codexWSPendingFrame) bool {
	return !f.nativeTurn && pending.portable && !pending.prewarm && f.canUse(ctx, pending.encoded)
}

func (s *Server) codexSharedBudgetWebSocketFallback(client *websocket.Conn, header http.Header) *codexWSSubscriptionFallback {
	var state responsesHTTPBridgeState
	var model string
	var bridge responsesHTTPBridge
	return &codexWSSubscriptionFallback{
		canUse:   s.canFallbackToSharedBudget,
		hasState: s.knownSharedBudgetState,
		serve: func(ctx context.Context, kind int, frame []byte) error {
			current := extractModel(frame)
			if model != "" && !strings.EqualFold(ParseModel(current), ParseModel(model)) {
				return errors.New("subscription model changed; reconnect with full input")
			}
			if bridge.Stream == nil {
				selected, ok := s.selectSharedBudgetFallback(ctx, frame)
				if !ok {
					return errors.New("subscription credential unavailable")
				}
				bridge = s.sharedBudgetBridge(selected, header)
				model = current
			}
			return state.serveFrame(ctx, client, header, kind, frame, bridge)
		},
	}
}
