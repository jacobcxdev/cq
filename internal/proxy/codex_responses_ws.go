package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexWSModelHeader = "x-codex-model"

var (
	ErrCodexWSHandshakeUnsupported = errors.New("Codex WebSocket handshake lacks prospective routing metadata")
	ErrCodexWSResyncRequired       = errors.New("Codex WebSocket resynchronisation required")
	ErrCodexWSInvalidFrame         = errors.New("invalid Codex WebSocket response.create frame")
	ErrCodexWSStaleGeneration      = errors.New("stale Codex WebSocket generation")
)

type CodexWSHandshake struct {
	Metadata CodexTurnMetadata
	Model    string
}

func ParseCodexWSHandshake(header http.Header) (CodexWSHandshake, error) {
	metadata, err := ParseCodexTurnMetadata(nil, header.Get(codexTurnMetadataKey), nil)
	if err != nil {
		return CodexWSHandshake{}, err
	}
	model := header.Get(codexWSModelHeader)
	if !metadata.Found || !metadata.Strong || metadata.Metadata.TurnID == "" || model == "" {
		return CodexWSHandshake{}, ErrCodexWSHandshakeUnsupported
	}
	return CodexWSHandshake{Metadata: metadata.Metadata, Model: model}, nil
}

type CodexWSResolvedFrame struct {
	Request        CodexProtocolRequest
	HandshakeStale bool
}

func ResolveCodexWSFirstFrame(frame []byte, handshake CodexWSHandshake) (CodexWSResolvedFrame, error) {
	request, err := ParseCodexProtocolRequest(frame, "", &handshake.Metadata)
	if err != nil {
		return CodexWSResolvedFrame{}, err
	}
	if request.Model == "" || request.Model != handshake.Model {
		return CodexWSResolvedFrame{}, fmt.Errorf("%w: handshake and first-frame model mismatch", ErrCodexWSHandshakeUnsupported)
	}
	stale := NewCodexLeaseKey(request.Metadata.Metadata) != NewCodexLeaseKey(handshake.Metadata)
	return CodexWSResolvedFrame{Request: request, HandshakeStale: stale}, nil
}

type CodexWSFrameKind uint8

const (
	CodexWSFrameInitial CodexWSFrameKind = iota + 1
	CodexWSFrameSameTurn
	CodexWSFrameRequireResync
)

type CodexWSFrameDecision struct {
	Kind              CodexWSFrameKind
	Request           CodexProtocolRequest
	AccountKey        codex.AccountKey
	AttemptGeneration uint64
	RotationIntent    *CodexWSRotationIntent
}

type CodexWSFrameBrokerConfig struct {
	Handshake            CodexWSHandshake
	AccountKey           codex.AccountKey
	ModeEpoch            uint64
	DownstreamGeneration uint64
	UpstreamGeneration   uint64
	ClientBuild          string
	RetryBudget          int
}

// CodexWSFrameBroker is an isolated classifier for one downstream/upstream
// socket pair. Production routing deliberately does not construct it yet.
type CodexWSFrameBroker struct {
	mu                sync.Mutex
	config            CodexWSFrameBrokerConfig
	currentKey        LeaseKey
	pendingKey        LeaseKey
	initialised       bool
	active            bool
	frameMetadata     bool
	attemptGeneration uint64
}

func NewCodexWSFrameBroker(config CodexWSFrameBrokerConfig) (*CodexWSFrameBroker, error) {
	if config.AccountKey == "" {
		return nil, errors.New("Codex WebSocket broker requires account binding")
	}
	if config.Handshake.Model == "" || config.Handshake.Metadata.RequestKind != CodexRequestTurn {
		return nil, ErrCodexWSHandshakeUnsupported
	}
	if err := validateCodexTurnMetadata(config.Handshake.Metadata); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCodexWSHandshakeUnsupported, err)
	}
	if _, err := NewCodexWSRotationIntent(
		NewCodexLeaseKey(config.Handshake.Metadata),
		config.ModeEpoch,
		config.DownstreamGeneration,
		config.UpstreamGeneration,
		config.ClientBuild,
		config.RetryBudget,
	); err != nil {
		return nil, err
	}
	return &CodexWSFrameBroker{config: config}, nil
}

func (broker *CodexWSFrameBroker) ClassifyResponseCreate(downstreamGeneration uint64, messageType int, frame []byte) (CodexWSFrameDecision, error) {
	if broker == nil {
		return CodexWSFrameDecision{}, ErrCodexWSInvalidFrame
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if downstreamGeneration != broker.config.DownstreamGeneration {
		return CodexWSFrameDecision{}, ErrCodexWSStaleGeneration
	}
	if messageType != websocket.TextMessage {
		return CodexWSFrameDecision{}, ErrCodexWSInvalidFrame
	}

	if !broker.initialised {
		resolved, err := ResolveCodexWSFirstFrame(frame, broker.config.Handshake)
		if err != nil {
			if errors.Is(err, ErrCodexWSHandshakeUnsupported) {
				return CodexWSFrameDecision{}, err
			}
			return CodexWSFrameDecision{}, fmt.Errorf("%w: %v", ErrCodexWSInvalidFrame, err)
		}
		request, err := validateCodexWSBrokerRequest(frame, resolved.Request)
		if err != nil {
			return CodexWSFrameDecision{}, err
		}
		if resolved.HandshakeStale {
			return CodexWSFrameDecision{}, fmt.Errorf("%w: handshake and first-frame turn mismatch", ErrCodexWSHandshakeUnsupported)
		}
		broker.initialised = true
		broker.active = true
		broker.currentKey = NewCodexLeaseKey(request.Metadata.Metadata)
		broker.frameMetadata = request.Metadata.Source != CodexTurnMetadataHandshake
		broker.attemptGeneration++
		return broker.forwardDecision(CodexWSFrameInitial, request), nil
	}

	var handshake *CodexTurnMetadata
	if !broker.frameMetadata {
		handshake = &broker.config.Handshake.Metadata
	}
	request, err := ParseCodexProtocolRequest(frame, "", handshake)
	if err != nil {
		return CodexWSFrameDecision{}, fmt.Errorf("%w: %v", ErrCodexWSInvalidFrame, err)
	}
	request, err = validateCodexWSBrokerRequest(frame, request)
	if err != nil {
		return CodexWSFrameDecision{}, err
	}
	hasFrameMetadata := request.Metadata.Source != CodexTurnMetadataHandshake
	key := NewCodexLeaseKey(request.Metadata.Metadata)
	if key.Lane != broker.currentKey.Lane {
		return CodexWSFrameDecision{}, fmt.Errorf("%w: frame changed socket lane", ErrCodexWSInvalidFrame)
	}
	if broker.pendingKey.Turn != "" {
		if key == broker.currentKey {
			return CodexWSFrameDecision{}, ErrCodexStaleTurn
		}
		return CodexWSFrameDecision{}, ErrCodexWSResyncRequired
	}
	if key == broker.currentKey {
		if request.Model != broker.config.Handshake.Model {
			return CodexWSFrameDecision{}, fmt.Errorf("%w: socket model changed within turn", ErrCodexWSHandshakeUnsupported)
		}
		if broker.active {
			return CodexWSFrameDecision{}, ErrCodexConcurrentTurn
		}
		broker.active = true
		broker.frameMetadata = broker.frameMetadata || hasFrameMetadata
		broker.attemptGeneration++
		return broker.forwardDecision(CodexWSFrameSameTurn, request), nil
	}
	if broker.active {
		return CodexWSFrameDecision{}, ErrCodexConcurrentTurn
	}

	intent, err := NewCodexWSRotationIntent(
		key,
		broker.config.ModeEpoch,
		broker.config.DownstreamGeneration,
		broker.config.UpstreamGeneration,
		broker.config.ClientBuild,
		broker.config.RetryBudget,
	)
	if err != nil {
		return CodexWSFrameDecision{}, fmt.Errorf("%w: %v", ErrCodexWSInvalidFrame, err)
	}
	if err := intent.ArmFullNewTurn(broker.currentKey); err != nil {
		return CodexWSFrameDecision{}, err
	}
	broker.pendingKey = key
	broker.frameMetadata = broker.frameMetadata || hasFrameMetadata
	return CodexWSFrameDecision{
		Kind:           CodexWSFrameRequireResync,
		Request:        request,
		AccountKey:     broker.config.AccountKey,
		RotationIntent: &intent,
	}, nil
}

func (broker *CodexWSFrameBroker) DrainAttempt(downstreamGeneration, upstreamGeneration, attemptGeneration uint64) error {
	if broker == nil {
		return ErrCodexWSInvalidFrame
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if downstreamGeneration != broker.config.DownstreamGeneration ||
		upstreamGeneration != broker.config.UpstreamGeneration ||
		attemptGeneration != broker.attemptGeneration {
		return ErrCodexWSStaleGeneration
	}
	if !broker.active {
		return ErrCodexWSInvalidFrame
	}
	broker.active = false
	return nil
}

func (broker *CodexWSFrameBroker) forwardDecision(kind CodexWSFrameKind, request CodexProtocolRequest) CodexWSFrameDecision {
	return CodexWSFrameDecision{
		Kind:              kind,
		Request:           request,
		AccountKey:        broker.config.AccountKey,
		AttemptGeneration: broker.attemptGeneration,
	}
}

func validateCodexWSBrokerRequest(frame []byte, request CodexProtocolRequest) (CodexProtocolRequest, error) {
	if request.Type != "response.create" {
		return CodexProtocolRequest{}, newCodexWSAuthorityError(codexWSInvalidFrameResponseType, nil)
	}
	authority, code, err := extractCodexFrozenAuthority(frame, "", false, nil)
	if err == nil {
		request = authority.protocol
	} else if request.Metadata.Source != CodexTurnMetadataHandshake || code != CodexFrozenRequestMetadataAuthority {
		return CodexProtocolRequest{}, newCodexWSAuthorityError(codexWSInvalidFrameDetailForFrozenCode(code), err)
	}
	if request.Model == "" {
		return CodexProtocolRequest{}, newCodexWSAuthorityError(codexWSInvalidFrameModelMissing, nil)
	}
	if !request.Metadata.Found {
		return CodexProtocolRequest{}, newCodexWSAuthorityError(codexWSInvalidFrameMetadataMissing, nil)
	}
	requestKind := request.Metadata.Metadata.RequestKind
	if requestKind != CodexRequestTurn && requestKind != CodexRequestCompaction {
		detail := codexWSInvalidFrameRequestKindUnknown
		if requestKind == CodexRequestMemory {
			detail = codexWSInvalidFrameRequestKindMemory
		}
		return CodexProtocolRequest{}, newCodexWSAuthorityError(detail, nil)
	}
	if err := NewCodexLeaseKey(request.Metadata.Metadata).validate(); err != nil {
		return CodexProtocolRequest{}, newCodexWSAuthorityError(codexWSInvalidFrameLeaseKey, err)
	}
	return request, nil
}

type codexWSAuthorityError struct {
	Detail codexWSInvalidFrameDetail
	cause  error
}

func (err *codexWSAuthorityError) Error() string {
	return "Codex WebSocket request authority rejected: " + string(err.Detail)
}

func (err *codexWSAuthorityError) Unwrap() error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrCodexWSInvalidFrame, err.cause)
}

func newCodexWSAuthorityError(detail codexWSInvalidFrameDetail, cause error) error {
	return &codexWSAuthorityError{Detail: detail, cause: cause}
}

func codexWSInvalidFrameDetailForFrozenCode(code CodexFrozenRequestErrorCode) codexWSInvalidFrameDetail {
	switch code {
	case CodexFrozenRequestMetadataAuthority:
		return codexWSInvalidFrameMetadataAuthority
	case CodexFrozenRequestModelAuthority:
		return codexWSInvalidFrameModelAuthority
	case CodexFrozenRequestProtocolInvalid:
		return codexWSInvalidFrameProtocolAuthority
	default:
		return codexWSInvalidFrameDetailUnknown
	}
}

type CodexWSRotationIntent struct {
	Key                    LeaseKey
	ModeEpoch              uint64
	DownstreamGeneration   uint64
	UpstreamGeneration     uint64
	ClientBuild            string
	RetryBudget            int
	ReplacementAccount     string
	ReplacementTransport   string
	PreviousResponseFailed bool
	FullNewTurn            bool
	Consumed               bool
}

func NewCodexWSRotationIntent(key LeaseKey, modeEpoch, downstreamGeneration, upstreamGeneration uint64, clientBuild string, retryBudget int) (CodexWSRotationIntent, error) {
	if err := key.validate(); err != nil {
		return CodexWSRotationIntent{}, err
	}
	if modeEpoch == 0 || downstreamGeneration == 0 || upstreamGeneration == 0 || clientBuild == "" || retryBudget < 0 {
		return CodexWSRotationIntent{}, errors.New("incomplete Codex WebSocket rotation intent")
	}
	return CodexWSRotationIntent{Key: key, ModeEpoch: modeEpoch, DownstreamGeneration: downstreamGeneration, UpstreamGeneration: upstreamGeneration, ClientBuild: clientBuild, RetryBudget: retryBudget}, nil
}

func (intent *CodexWSRotationIntent) ArmFullNewTurn(predecessor LeaseKey) error {
	if intent == nil || predecessor.Lane != intent.Key.Lane || predecessor.Turn == intent.Key.Turn {
		return ErrCodexWSResyncRequired
	}
	intent.FullNewTurn = true
	return nil
}

func (intent *CodexWSRotationIntent) ObserveUpstreamError(frame []byte) (bool, error) {
	if intent == nil {
		return false, errors.New("Codex WebSocket rotation intent unavailable")
	}
	wrapped, err := ParseCodexWrappedError(frame)
	if err != nil {
		return false, err
	}
	matched := wrapped.Found && wrapped.Code == "previous_response_not_found"
	if matched {
		intent.PreviousResponseFailed = true
	}
	return matched, nil
}

func (intent *CodexWSRotationIntent) ConsumeReplacement(request CodexProtocolRequest, downstreamGeneration, upstreamGeneration uint64, clientBuild string, retriesUsed int, transport string) error {
	if intent == nil || intent.Consumed {
		return ErrCodexWSResyncRequired
	}
	if (!intent.PreviousResponseFailed && !intent.FullNewTurn) || clientBuild != intent.ClientBuild || retriesUsed > intent.RetryBudget || downstreamGeneration == intent.DownstreamGeneration || upstreamGeneration == intent.UpstreamGeneration {
		return ErrCodexWSResyncRequired
	}
	if NewCodexLeaseKey(request.Metadata.Metadata) != intent.Key || request.PreviousResponseID != "" || request.HasEncryptedState {
		return ErrCodexWSResyncRequired
	}
	if transport != "websocket" && transport != "http" {
		return ErrCodexWSResyncRequired
	}
	intent.ReplacementTransport = transport
	intent.Consumed = true
	return nil
}

func relayCodexWebSocketPreupgrade(writer http.ResponseWriter, response *http.Response, body []byte) {
	if response == nil {
		http.Error(writer, "Codex WebSocket upstream unavailable", http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Content-Type", "Retry-After", "X-Request-Id", "Cf-Ray", "Openai-Processing-Ms"} {
		for _, value := range response.Header.Values(name) {
			writer.Header().Add(name, value)
		}
	}
	status := response.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}
