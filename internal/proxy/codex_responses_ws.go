package proxy

import (
	"errors"
	"fmt"
	"net/http"
)

const codexWSModelHeader = "x-codex-model"

var (
	ErrCodexWSHandshakeUnsupported = errors.New("Codex WebSocket handshake lacks prospective routing metadata")
	ErrCodexWSResyncRequired       = errors.New("Codex WebSocket resynchronisation required")
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
