package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
)

const (
	rescueRequestTargetLimit = 8 << 10
	rescueHeaderSectionLimit = 64 << 10
	rescueHeaderCountLimit   = 64
	rescueHeaderNameLimit    = 128
	rescueHeaderValueLimit   = 16 << 10
	rescueBearerLimit        = 16 << 10
	rescueRequestBodyLimit   = 64 << 20
)

var (
	ErrRescueCapacity = errors.New("rescue capacity unavailable")
	errRescueRedirect = errors.New("rescue redirect refused")
)

type RescueIngressKind string

const (
	RescueIngressUnverified     RescueIngressKind = "unverified_rescue_bearer"
	RescueIngressOwnerPermitted RescueIngressKind = "owner_permitted_rescue"
)

type rescueTokenBucket struct {
	tokens float64
	last   time.Time
}

type RescueBudget struct {
	mu sync.Mutex

	now         func() time.Time
	fairnessKey [sha256.Size]byte
	global      rescueTokenBucket
	owner       rescueTokenBucket
	perBearer   map[string]rescueTokenBucket

	httpTotal      int
	httpUnverified int
	upstreamTotal  int
	upstreamUnver  int
	wsTotal        int
	wsUnverified   int
	perBearerHTTP  map[string]int
	perBearerWS    map[string]int
}

func NewRescueBudget(now func() time.Time, fairnessKey [sha256.Size]byte) *RescueBudget {
	if now == nil {
		now = time.Now
	}
	return &RescueBudget{
		now: now, fairnessKey: fairnessKey,
		perBearer: make(map[string]rescueTokenBucket), perBearerHTTP: make(map[string]int), perBearerWS: make(map[string]int),
	}
}

func (budget *RescueBudget) bearerKey(bearer []byte) string {
	mac := hmac.New(sha256.New, budget.fairnessKey[:])
	_, _ = mac.Write([]byte("cq/rescue-fairness/v1\x00"))
	_, _ = mac.Write(bearer)
	return hex.EncodeToString(mac.Sum(nil))
}

func takeRescueToken(bucket rescueTokenBucket, now time.Time, rate, burst float64) (rescueTokenBucket, bool) {
	if bucket.last.IsZero() {
		bucket.tokens = burst
		bucket.last = now
	} else if now.After(bucket.last) {
		bucket.tokens = math.Min(burst, bucket.tokens+now.Sub(bucket.last).Seconds()*rate)
		bucket.last = now
	}
	if bucket.tokens < 1 {
		return bucket, false
	}
	bucket.tokens--
	return bucket, true
}

func (budget *RescueBudget) acquireRateLocked(kind RescueIngressKind, bearerKey string) bool {
	now := budget.now()
	if kind == RescueIngressOwnerPermitted {
		var ok bool
		budget.owner, ok = takeRescueToken(budget.owner, now, 4, 8)
		return ok
	}
	var ok bool
	budget.global, ok = takeRescueToken(budget.global, now, 16, 32)
	if !ok {
		return false
	}
	entry := budget.perBearer[bearerKey]
	entry, ok = takeRescueToken(entry, now, 2, 4)
	budget.perBearer[bearerKey] = entry
	return ok
}

func (budget *RescueBudget) AcquireHTTP(ctx context.Context, kind RescueIngressKind, bearer []byte) (func(), error) {
	if budget == nil || (kind != RescueIngressUnverified && kind != RescueIngressOwnerPermitted) {
		return nil, ErrRescueCapacity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := ""
	if kind == RescueIngressUnverified {
		key = budget.bearerKey(bearer)
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if !budget.acquireRateLocked(kind, key) || budget.httpTotal >= 8 ||
		(kind == RescueIngressUnverified && (budget.httpUnverified >= 6 || budget.perBearerHTTP[key] >= 2)) {
		return nil, ErrRescueCapacity
	}
	budget.httpTotal++
	if kind == RescueIngressUnverified {
		budget.httpUnverified++
		budget.perBearerHTTP[key]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			budget.mu.Lock()
			defer budget.mu.Unlock()
			budget.httpTotal--
			if kind == RescueIngressUnverified {
				budget.httpUnverified--
				budget.perBearerHTTP[key]--
				if budget.perBearerHTTP[key] == 0 {
					delete(budget.perBearerHTTP, key)
				}
			}
		})
	}, nil
}

func (budget *RescueBudget) acquireUpstream(ctx context.Context, kind RescueIngressKind) (func(), error) {
	if budget == nil {
		return nil, ErrRescueCapacity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.upstreamTotal >= 8 || (kind == RescueIngressUnverified && budget.upstreamUnver >= 6) {
		return nil, ErrRescueCapacity
	}
	budget.upstreamTotal++
	if kind == RescueIngressUnverified {
		budget.upstreamUnver++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			budget.mu.Lock()
			defer budget.mu.Unlock()
			budget.upstreamTotal--
			if kind == RescueIngressUnverified {
				budget.upstreamUnver--
			}
		})
	}, nil
}

func (budget *RescueBudget) AcquireWebSocket(ctx context.Context, kind RescueIngressKind, bearer []byte) (func(), error) {
	if budget == nil || (kind != RescueIngressUnverified && kind != RescueIngressOwnerPermitted) {
		return nil, ErrRescueCapacity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := ""
	if kind == RescueIngressUnverified {
		key = budget.bearerKey(bearer)
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if !budget.acquireRateLocked(kind, key) || budget.wsTotal >= 4 ||
		(kind == RescueIngressUnverified && (budget.wsUnverified >= 3 || budget.perBearerWS[key] >= 1)) {
		return nil, ErrRescueCapacity
	}
	budget.wsTotal++
	if kind == RescueIngressUnverified {
		budget.wsUnverified++
		budget.perBearerWS[key]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			budget.mu.Lock()
			defer budget.mu.Unlock()
			budget.wsTotal--
			if kind == RescueIngressUnverified {
				budget.wsUnverified--
				budget.perBearerWS[key]--
				if budget.perBearerWS[key] == 0 {
					delete(budget.perBearerWS, key)
				}
			}
		})
	}, nil
}

type RescueWebSocketDialer interface {
	DialContext(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)
}

type RescueRelay struct {
	Transport              httputil.Doer
	DialWS                 RescueWebSocketDialer
	Origin                 *url.URL
	LoopbackHost           string
	ForwardingAcknowledged bool
	DenyBearer             func([]byte) bool
	Budget                 *RescueBudget
	Admission              func(*http.Request) RescueIngressKind
}

type rescueRouteKind string

const (
	rescueRouteModels    rescueRouteKind = "models"
	rescueRouteResponse  rescueRouteKind = "response"
	rescueRouteCompact   rescueRouteKind = "compact"
	rescueRouteWebSocket rescueRouteKind = "websocket"
)

type rescueRoute struct {
	kind   rescueRouteKind
	suffix string
	flush  bool
	allow  string
}

func (relay *RescueRelay) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if relay == nil || request == nil || !validRescueOrigin(relay.Origin) || !relay.ForwardingAcknowledged || relay.Budget == nil {
		writeRescueError(writer, http.StatusServiceUnavailable, "rescue_unavailable")
		return
	}
	if err := validateRescueRequestEnvelope(request, relay.LoopbackHost); err != nil {
		writeRescueError(writer, http.StatusBadRequest, "rescue_request_invalid")
		return
	}
	route, status, code := classifyRescueRoute(request)
	if status != 0 {
		if route.allow != "" {
			writer.Header().Set("Allow", route.allow)
		}
		writeRescueError(writer, status, code)
		return
	}
	bearer := rescueBearer(request)
	if relay.DenyBearer != nil && relay.DenyBearer(bearer) {
		writeRescueError(writer, http.StatusUnauthorized, "rescue_local_token_refused")
		return
	}
	if err := validateRescueBody(request, route.kind); err != nil {
		writeRescueError(writer, http.StatusRequestEntityTooLarge, "rescue_body_unsupported")
		return
	}
	kind := RescueIngressUnverified
	if relay.Admission != nil {
		kind = relay.Admission(request)
	}
	if route.kind == rescueRouteWebSocket {
		relay.serveWebSocket(writer, request, route, bearer, kind)
		return
	}
	if relay.Transport == nil {
		writeRescueError(writer, http.StatusServiceUnavailable, "rescue_unavailable")
		return
	}
	requestCtx := request.Context()
	var cancel context.CancelFunc
	if kind == RescueIngressUnverified {
		requestCtx, cancel = context.WithTimeout(requestCtx, 15*time.Second)
		defer cancel()
	}
	releaseHTTP, err := relay.Budget.AcquireHTTP(requestCtx, kind, bearer)
	if err != nil {
		writer.Header().Set("Retry-After", "1")
		writeRescueError(writer, http.StatusServiceUnavailable, "rescue_capacity_exhausted")
		return
	}
	defer releaseHTTP()
	releaseUpstream, err := relay.Budget.acquireUpstream(requestCtx, kind)
	if err != nil {
		writer.Header().Set("Retry-After", "1")
		writeRescueError(writer, http.StatusServiceUnavailable, "rescue_capacity_exhausted")
		return
	}
	defer releaseUpstream()

	upstreamURL := *relay.Origin
	upstreamURL.Path = strings.TrimSuffix(relay.Origin.Path, "/") + route.suffix
	upstreamURL.RawPath = ""
	upstreamURL.RawQuery = request.URL.RawQuery
	upstream, err := http.NewRequestWithContext(requestCtx, request.Method, upstreamURL.String(), request.Body)
	if err != nil {
		writeRescueError(writer, http.StatusBadRequest, "rescue_request_invalid")
		return
	}
	upstream.Header = sanitiseRescueRequestHeaders(request.Header)
	upstream.Host = relay.Origin.Host
	upstream.ContentLength = request.ContentLength
	upstream.GetBody = nil
	upstream.Close = true
	response, err := relay.Transport.Do(upstream)
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		writeRescueError(writer, http.StatusBadGateway, "rescue_upstream_unavailable")
		return
	}
	defer response.Body.Close()
	if err := relayRescueHTTPResponse(writer, response, route); err != nil {
		return
	}
}

func validRescueOrigin(origin *url.URL) bool {
	return origin != nil && origin.Scheme == "https" && origin.Host == "chatgpt.com" && origin.Path == "/backend-api/codex" && origin.RawQuery == "" && origin.Fragment == "" && origin.User == nil
}

func validateRescueRequestEnvelope(request *http.Request, host string) error {
	if host == "" || request.Host != host || request.Method == http.MethodConnect || request.URL == nil || request.URL.IsAbs() || request.URL.Host != "" {
		return errors.New("invalid rescue target")
	}
	target := request.URL.RequestURI()
	if target == "" || target[0] != '/' || len(target) > rescueRequestTargetLimit || request.URL.EscapedPath() != request.URL.Path {
		return errors.New("invalid rescue target")
	}
	if request.Header.Get("Origin") != "" || request.Header.Get("Sec-Fetch-Site") != "" || request.Header.Get("Sec-Fetch-Mode") != "" || request.Header.Get("Sec-Fetch-Dest") != "" || request.Header.Get("Expect") != "" {
		return errors.New("browser or expect header refused")
	}
	count, total := 0, 0
	seen := make(map[string]struct{})
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		if _, exists := seen[lower]; exists {
			return errors.New("ambiguous header")
		}
		seen[lower] = struct{}{}
		if len(name) == 0 || len(name) > rescueHeaderNameLimit {
			return errors.New("invalid header name")
		}
		for _, value := range values {
			count++
			if len(value) > rescueHeaderValueLimit || strings.ContainsAny(value, "\r\n") {
				return errors.New("invalid header value")
			}
			total += len(name) + len(value) + 4
		}
	}
	if count > rescueHeaderCountLimit || total > rescueHeaderSectionLimit {
		return errors.New("header section too large")
	}
	return nil
}

func classifyRescueRoute(request *http.Request) (rescueRoute, int, string) {
	path := request.URL.Path
	if path == "/models" {
		if request.Method != http.MethodGet {
			return rescueRoute{allow: http.MethodGet}, http.StatusMethodNotAllowed, "rescue_method_unsupported"
		}
		return rescueRoute{kind: rescueRouteModels, suffix: "/models"}, 0, ""
	}
	if path == "/responses" || path == "/v1/responses" {
		if path == "/responses" && request.Method == http.MethodGet && isRescueWebSocketUpgrade(request) {
			return rescueRoute{kind: rescueRouteWebSocket, suffix: "/responses"}, 0, ""
		}
		if request.Method != http.MethodPost {
			allow := http.MethodPost
			if path == "/responses" {
				allow = http.MethodPost + ", " + http.MethodGet
			}
			return rescueRoute{allow: allow}, http.StatusMethodNotAllowed, "rescue_method_unsupported"
		}
		return rescueRoute{kind: rescueRouteResponse, suffix: "/responses", flush: true}, 0, ""
	}
	if path == "/responses/compact" || path == "/v1/responses/compact" {
		if request.Method != http.MethodPost {
			return rescueRoute{allow: http.MethodPost}, http.StatusMethodNotAllowed, "rescue_method_unsupported"
		}
		return rescueRoute{kind: rescueRouteCompact, suffix: "/responses/compact"}, 0, ""
	}
	return rescueRoute{}, http.StatusNotFound, "rescue_route_unsupported"
}

func rescueBearer(request *http.Request) []byte {
	if request == nil {
		return nil
	}
	values := request.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > rescueBearerLimit || !strings.HasPrefix(values[0], "Bearer ") {
		return nil
	}
	bearer := strings.TrimPrefix(values[0], "Bearer ")
	if bearer == "" {
		return nil
	}
	return []byte(bearer)
}

func validateRescueBody(request *http.Request, route rescueRouteKind) error {
	if len(request.TransferEncoding) != 0 || request.Trailer != nil {
		return errors.New("unsupported framing")
	}
	if route == rescueRouteModels || route == rescueRouteWebSocket {
		if request.ContentLength != 0 || request.Body != http.NoBody {
			return errors.New("body unsupported")
		}
		return nil
	}
	if request.ContentLength <= 0 || request.ContentLength > rescueRequestBodyLimit {
		return errors.New("invalid body length")
	}
	return nil
}

func sanitiseRescueRequestHeaders(input http.Header) http.Header {
	output := make(http.Header, len(input))
	for name, values := range input {
		lower := strings.ToLower(name)
		if lower == "content-length" || lower == "host" || lower == "connection" || lower == "proxy-connection" || lower == "transfer-encoding" || lower == "trailer" || lower == "upgrade" || lower == "keep-alive" || lower == "te" || lower == "expect" || lower == "accept-encoding" {
			continue
		}
		if strings.HasPrefix(lower, "sec-websocket-") {
			continue
		}
		output[name] = append([]string(nil), values...)
	}
	return output
}

func isRescueWebSocketUpgrade(request *http.Request) bool {
	return request != nil && strings.EqualFold(request.Header.Get("Upgrade"), "websocket") && strings.EqualFold(request.Header.Get("Connection"), "Upgrade")
}

type rescueGorillaWebSocketDialer struct {
	dialer websocket.Dialer
}

func (dialer rescueGorillaWebSocketDialer) DialContext(ctx context.Context, target string, header http.Header) (*websocket.Conn, *http.Response, error) {
	return dialer.dialer.DialContext(ctx, target, header)
}

func (relay *RescueRelay) serveWebSocket(writer http.ResponseWriter, request *http.Request, route rescueRoute, bearer []byte, kind RescueIngressKind) {
	ctx := request.Context()
	var cancel context.CancelFunc
	if kind == RescueIngressUnverified {
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	releaseWebSocket, err := relay.Budget.AcquireWebSocket(ctx, kind, bearer)
	if err != nil {
		writer.Header().Set("Retry-After", "1")
		writeRescueError(writer, http.StatusServiceUnavailable, "rescue_capacity_exhausted")
		return
	}
	defer releaseWebSocket()
	releaseUpstream, err := relay.Budget.acquireUpstream(ctx, kind)
	if err != nil {
		writer.Header().Set("Retry-After", "1")
		writeRescueError(writer, http.StatusServiceUnavailable, "rescue_capacity_exhausted")
		return
	}
	defer releaseUpstream()

	target := *relay.Origin
	target.Scheme = "wss"
	target.Path = strings.TrimSuffix(relay.Origin.Path, "/") + route.suffix
	target.RawPath = ""
	target.RawQuery = ""
	dialer := relay.DialWS
	if dialer == nil {
		dialer = rescueGorillaWebSocketDialer{dialer: websocket.Dialer{
			Proxy: nil, HandshakeTimeout: 15 * time.Second, EnableCompression: true,
			Subprotocols: websocket.Subprotocols(request),
		}}
	}
	upstream, response, err := dialer.DialContext(ctx, target.String(), sanitiseRescueRequestHeaders(request.Header))
	if err != nil {
		relayRescueWebSocketHandshakeError(writer, response)
		return
	}
	if upstream == nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
		if upstream != nil {
			_ = upstream.Close()
		}
		relayRescueWebSocketHandshakeError(writer, response)
		return
	}
	responseHeaders, err := validateRescueWebSocket101(response.Header)
	if err != nil {
		_ = upstream.Close()
		writeRescueError(writer, http.StatusBadGateway, "rescue_ws_handshake_invalid")
		return
	}
	upgrader := &websocket.Upgrader{
		HandshakeTimeout:  15 * time.Second,
		EnableCompression: true,
		CheckOrigin:       func(*http.Request) bool { return true },
	}
	if protocol := upstream.Subprotocol(); protocol != "" {
		upgrader.Subprotocols = []string{protocol}
	}
	downstream, err := upgrader.Upgrade(writer, request, responseHeaders)
	if err != nil {
		_ = upstream.Close()
		return
	}
	downstream.SetReadLimit(64 << 20)
	upstream.SetReadLimit(64 << 20)
	_ = relayWebSocketPair(ctx, downstream, upstream)
}

func relayRescueWebSocketHandshakeError(writer http.ResponseWriter, response *http.Response) {
	if response == nil || response.Body == nil {
		writeRescueError(writer, http.StatusBadGateway, "rescue_ws_handshake_failed")
		return
	}
	defer response.Body.Close()
	body, err := ioReadBounded(response.Body, httputil.MaxResponseBody)
	if err != nil {
		writeRescueError(writer, http.StatusBadGateway, "rescue_ws_handshake_body_too_large")
		return
	}
	headers, err := validateRescueResponseHeaders(response.Header, rescueRouteResponse)
	if err != nil || response.StatusCode == http.StatusSwitchingProtocols {
		writeRescueError(writer, http.StatusBadGateway, "rescue_ws_handshake_invalid")
		return
	}
	for name, values := range headers {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(body)
}

func validateRescueWebSocket101(input http.Header) (http.Header, error) {
	semantic := make(http.Header)
	for name, values := range input {
		lower := strings.ToLower(name)
		switch lower {
		case "upgrade", "connection", "sec-websocket-accept":
			continue
		case "sec-websocket-extensions", "sec-websocket-protocol":
			continue
		case "set-cookie", "location", "www-authenticate":
			return nil, errors.New("unsupported websocket response header")
		}
		if allowedRescueResponseHeader(lower, rescueRouteResponse) {
			semantic[name] = append([]string(nil), values...)
		}
	}
	return validateRescueResponseHeaders(semantic, rescueRouteResponse)
}

func relayRescueHTTPResponse(writer http.ResponseWriter, response *http.Response, route rescueRoute) error {
	headers, err := validateRescueResponseHeaders(response.Header, route.kind)
	if err != nil {
		code := "rescue_upstream_header_invalid"
		if errors.Is(err, errRescueRedirect) {
			code = "rescue_redirect_refused"
		}
		writeRescueError(writer, http.StatusBadGateway, code)
		return err
	}
	for name, values := range headers {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	if !route.flush {
		_, err = io.Copy(writer, response.Body)
		return err
	}
	flusher, canFlush := writer.(http.Flusher)
	buffer := make([]byte, 32<<10)
	for {
		read, readErr := response.Body.Read(buffer)
		if read > 0 {
			if _, writeErr := writer.Write(buffer[:read]); writeErr != nil {
				return writeErr
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func validateRescueResponseHeaders(input http.Header, route rescueRouteKind) (http.Header, error) {
	output := make(http.Header)
	total, count := 0, 0
	for name, values := range input {
		lower := strings.ToLower(name)
		if lower == "location" {
			return nil, errRescueRedirect
		}
		if !allowedRescueResponseHeader(lower, route) {
			continue
		}
		if len(values) != 1 || strings.ContainsAny(values[0], "\r\n") {
			return nil, errors.New("ambiguous response header")
		}
		count++
		total += len(name) + len(values[0]) + 4
		if count > rescueHeaderCountLimit || total > rescueHeaderSectionLimit {
			return nil, errors.New("response headers too large")
		}
		if lower == "content-type" {
			if _, _, err := mime.ParseMediaType(values[0]); err != nil {
				return nil, err
			}
		}
		if lower == "content-encoding" && values[0] != "identity" && values[0] != "gzip" && values[0] != "zstd" {
			return nil, errors.New("invalid content encoding")
		}
		if lower == "x-error-json" {
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			decoded, err := decodeStandardBase64(values[0])
			if err != nil || json.Unmarshal(decoded, &envelope) != nil || envelope.Error.Code == "" {
				return nil, errors.New("invalid error json")
			}
		}
		if strings.HasSuffix(lower, "-used-percent") {
			value, err := strconv.ParseFloat(values[0], 64)
			if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
				return nil, errors.New("invalid rate limit percent")
			}
		}
		isResetAt := strings.HasSuffix(lower, "-reset-at")
		if strings.HasSuffix(lower, "-window-minutes") || isResetAt {
			if values[0] != "" || !isResetAt {
				if _, err := strconv.ParseInt(values[0], 10, 64); err != nil {
					return nil, errors.New("invalid rate limit integer")
				}
			}
		}
		output[name] = append([]string(nil), values...)
	}
	return output, nil
}

func allowedRescueResponseHeader(name string, route rescueRouteKind) bool {
	switch name {
	case "content-type", "content-encoding", "x-request-id", "x-oai-request-id", "cf-ray", "x-openai-authorization-error", "x-error-json", "x-codex-active-limit", "x-codex-promo-message", "x-codex-credits-balance", "x-codex-rate-limit-reached-type", "x-codex-credits-has-credits", "x-codex-credits-unlimited":
		return true
	case "etag":
		return route == rescueRouteModels
	case "openai-model", "x-codex-turn-state", "x-models-etag", "x-codex-safety-buffering-faster-model", "x-reasoning-included", "x-codex-safety-buffering-enabled":
		return route == rescueRouteResponse || (route == rescueRouteCompact && name == "x-codex-turn-state")
	}
	if !strings.HasPrefix(name, "x-") {
		return false
	}
	for _, suffix := range []string{"-primary-used-percent", "-primary-window-minutes", "-primary-reset-at", "-secondary-used-percent", "-secondary-window-minutes", "-secondary-reset-at", "-limit-name"} {
		if strings.HasSuffix(name, suffix) {
			identifier := strings.TrimSuffix(strings.TrimPrefix(name, "x-"), suffix)
			return identifier != "" && strings.IndexFunc(identifier, func(character rune) bool {
				return !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '-'
			}) == -1
		}
	}
	return false
}

func decodeStandardBase64(value string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(value)
}

func writeRescueError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]string{"type": "rescue_error", "code": code}})
}
