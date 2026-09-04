package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/gorilla/websocket"
)

type codexWSPendingFrame struct {
	messageType int
	encoded     []byte
	request     CodexProtocolRequest
	key         LeaseKey
	portable    bool
	prewarm     bool
	diagnostics *routeDiagnostics
	released    bool
}

type codexWSInvalidFrameOrigin string

const (
	codexWSInvalidFrameEnvelope         codexWSInvalidFrameOrigin = "envelope"
	codexWSInvalidFrameProtocol         codexWSInvalidFrameOrigin = "protocol"
	codexWSInvalidFramePrewarmAuthority codexWSInvalidFrameOrigin = "prewarm_authority"
	codexWSInvalidFrameBrokerAuthority  codexWSInvalidFrameOrigin = "broker_authority"
	codexWSInvalidFrameUpstreamPrewarm  codexWSInvalidFrameOrigin = "upstream_prewarm"
	codexWSInvalidFrameUpstreamResponse codexWSInvalidFrameOrigin = "upstream_response"
)

type codexWSFrameType string

const (
	codexWSFrameUnknown codexWSFrameType = "unknown"
	codexWSFrameText    codexWSFrameType = "text"
	codexWSFrameBinary  codexWSFrameType = "binary"
	codexWSFrameOther   codexWSFrameType = "other"
)

type codexWSFrameSize string

const (
	codexWSFrameSizeEmpty    codexWSFrameSize = "empty"
	codexWSFrameSizeSmall    codexWSFrameSize = "small"
	codexWSFrameSizeMedium   codexWSFrameSize = "medium"
	codexWSFrameSizeLarge    codexWSFrameSize = "large"
	codexWSFrameSizeOversize codexWSFrameSize = "oversize"
)

type codexWSInvalidFrameDetail string

const (
	codexWSInvalidFrameDetailUnknown      codexWSInvalidFrameDetail = "unknown"
	codexWSInvalidFrameMetadataAuthority  codexWSInvalidFrameDetail = "metadata_authority"
	codexWSInvalidFrameModelAuthority     codexWSInvalidFrameDetail = "model_authority"
	codexWSInvalidFrameProtocolAuthority  codexWSInvalidFrameDetail = "protocol_authority"
	codexWSInvalidFrameRequestShape       codexWSInvalidFrameDetail = "request_shape"
	codexWSInvalidFrameResponseType       codexWSInvalidFrameDetail = "response_type"
	codexWSInvalidFrameModelMissing       codexWSInvalidFrameDetail = "model_missing"
	codexWSInvalidFrameMetadataMissing    codexWSInvalidFrameDetail = "metadata_missing"
	codexWSInvalidFrameRequestKind        codexWSInvalidFrameDetail = "request_kind"
	codexWSInvalidFrameRequestKindMemory  codexWSInvalidFrameDetail = "request_kind_memory"
	codexWSInvalidFrameRequestKindUnknown codexWSInvalidFrameDetail = "request_kind_unknown"
	codexWSInvalidFrameLeaseKey           codexWSInvalidFrameDetail = "lease_key"
	codexWSInvalidFrameNonText            codexWSInvalidFrameDetail = "non_text"
	codexWSInvalidFrameMalformedEvent     codexWSInvalidFrameDetail = "malformed_event"
	codexWSInvalidFrameUnknownEvent       codexWSInvalidFrameDetail = "unknown_event"
	codexWSInvalidFrameCompletionOrder    codexWSInvalidFrameDetail = "completion_before_admission"
	codexWSInvalidFrameErrorOrder         codexWSInvalidFrameDetail = "error_before_admission"
	codexWSInvalidFrameDeltaOrder         codexWSInvalidFrameDetail = "delta_before_admission"
)

type codexWSEventType string

const codexWSEventTypeUnknown codexWSEventType = "unknown"

type codexWSInvalidFrameError struct {
	Origin codexWSInvalidFrameOrigin
	Type   codexWSFrameType
	Size   codexWSFrameSize
	Detail codexWSInvalidFrameDetail
	Event  codexWSEventType
	Status string
	Code   codexWSEventType
	Kind   codexWSEventType
	cause  error
}

func (err *codexWSInvalidFrameError) Error() string {
	return "Codex WebSocket frame rejected: " + string(err.Origin)
}

func (err *codexWSInvalidFrameError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func newCodexWSInvalidFrameError(origin codexWSInvalidFrameOrigin, messageType int, payload []byte, cause error) error {
	return newCodexWSInvalidFrameErrorWithDetail(origin, messageType, payload, "", cause)
}

func newCodexWSInvalidFrameErrorWithDetail(origin codexWSInvalidFrameOrigin, messageType int, payload []byte, detail codexWSInvalidFrameDetail, cause error) error {
	var authorityErr *codexWSAuthorityError
	if detail == "" && errors.As(cause, &authorityErr) {
		detail = authorityErr.Detail
	}
	status, errorType, code := classifyCodexWSErrorMetadata(messageType, payload)
	return &codexWSInvalidFrameError{
		Origin: origin,
		Type:   classifyCodexWSFrameType(messageType),
		Size:   classifyCodexWSFrameSize(len(payload)),
		Detail: detail,
		Event:  classifyCodexWSEventType(messageType, payload),
		Status: status,
		Code:   code,
		Kind:   errorType,
		cause:  errors.Join(ErrCodexWSInvalidFrame, cause),
	}
}

func classifyCodexWSErrorMetadata(messageType int, payload []byte) (string, codexWSEventType, codexWSEventType) {
	if messageType != websocket.TextMessage {
		return "", "", ""
	}
	wrapped, err := ParseCodexWrappedError(payload)
	if err != nil || !wrapped.Found {
		return "", "", ""
	}
	status := ""
	if wrapped.Status >= 100 && wrapped.Status <= 599 {
		status = strconv.Itoa(wrapped.Status)
	}
	errorType := codexWSEventType("")
	if knownCodexWSErrorType(wrapped.ErrorType) {
		errorType = codexWSEventType(wrapped.ErrorType)
	}
	return status, errorType, ""
}

func classifyCodexWSEventType(messageType int, payload []byte) codexWSEventType {
	if messageType != websocket.TextMessage {
		return codexWSEventTypeUnknown
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &envelope) != nil || !knownCodexWSEventType(envelope.Type) {
		return codexWSEventTypeUnknown
	}
	return codexWSEventType(envelope.Type)
}

func knownCodexWSEventType(value string) bool {
	switch value {
	case "error",
		"codex.rate_limits",
		"codex.response.metadata",
		"keepalive",
		"response.completed",
		"response.content_part.added",
		"response.content_part.done",
		"response.created",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.failed",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.in_progress",
		"response.incomplete",
		"response.metadata",
		"response.output_item.added",
		"response.output_item.done",
		"response.output_text.delta",
		"response.output_text.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"responsesapi.websocket_timing":
		return true
	default:
		return false
	}
}

func knownCodexWSErrorType(value string) bool {
	switch value {
	case "api_error",
		"authentication_error",
		"insufficient_quota",
		"invalid_request_error",
		"rate_limit_exceeded",
		"server_error",
		"usage_limit_reached":
		return true
	default:
		return false
	}
}

func classifyCodexWSFrameType(messageType int) codexWSFrameType {
	switch messageType {
	case websocket.TextMessage:
		return codexWSFrameText
	case websocket.BinaryMessage:
		return codexWSFrameBinary
	case 0:
		return codexWSFrameUnknown
	default:
		return codexWSFrameOther
	}
}

func classifyCodexWSFrameSize(size int) codexWSFrameSize {
	switch {
	case size <= 0:
		return codexWSFrameSizeEmpty
	case size <= 64<<10:
		return codexWSFrameSizeSmall
	case size <= 1<<20:
		return codexWSFrameSizeMedium
	case size <= codexWebSocketUpstreamRequestMaxBytes:
		return codexWSFrameSizeLarge
	default:
		return codexWSFrameSizeOversize
	}
}

func newCodexWSPendingFrame(messageType int, payload []byte) (*codexWSPendingFrame, error) {
	return parseCodexWSPendingFrame(messageType, payload, true)
}

// newCodexWSPendingFrameOwned consumes payload. WebSocket ReadMessage already
// returns caller-owned bytes, so broker ingress must not clone them again.
func newCodexWSPendingFrameOwned(messageType int, payload []byte) (*codexWSPendingFrame, error) {
	pending, err := parseCodexWSPendingFrame(messageType, payload, false)
	if err != nil {
		clearBytes(payload)
	}
	return pending, err
}

func parseCodexWSPendingFrame(messageType int, payload []byte, clone bool) (*codexWSPendingFrame, error) {
	if messageType != websocket.TextMessage || len(payload) == 0 || codexLimitExceeded(len(payload), codexWebSocketUpstreamRequestMaxBytes) {
		return nil, newCodexWSInvalidFrameError(codexWSInvalidFrameEnvelope, messageType, payload, nil)
	}
	request, err := parseCodexProtocolRequest(payload, "", nil, codexWebSocketUpstreamRequestMaxBytes)
	if err != nil {
		return nil, newCodexWSInvalidFrameError(codexWSInvalidFrameProtocol, messageType, payload, err)
	}
	prewarm := request.Metadata.Found && request.Metadata.Metadata.RequestKind == CodexRequestPrewarm
	if prewarm {
		request, err = validateCodexWSPrewarmRequest(payload, request)
	} else {
		request, err = validateCodexWSBrokerRequest(payload, request)
	}
	if err != nil {
		origin := codexWSInvalidFrameBrokerAuthority
		if prewarm {
			origin = codexWSInvalidFramePrewarmAuthority
		}
		return nil, newCodexWSInvalidFrameError(origin, messageType, payload, err)
	}
	var key LeaseKey
	if !prewarm {
		key = NewCodexLeaseKey(request.Metadata.Metadata)
	}
	diagnostics := &routeDiagnostics{}
	diagnostics.codex = codexObservationFieldsForProtocol(request, nil)
	encoded := payload
	if clone {
		encoded = bytes.Clone(payload)
	}
	return &codexWSPendingFrame{
		messageType: messageType,
		encoded:     encoded,
		request:     request,
		key:         key,
		portable:    request.PreviousResponseID == "" && !request.HasPreviousResponseID && !request.HasTurnState,
		prewarm:     prewarm,
		diagnostics: diagnostics,
	}, nil
}

// codexWSFrameWithoutPrewarmAnchor removes the connection-scoped anchor after
// CQ retires a completed prewarm upstream and opens its replacement.
func codexWSFrameWithoutPrewarmAnchor(original *codexWSPendingFrame, anchor string) (*codexWSPendingFrame, error) {
	if original == nil || original.prewarm || anchor == "" || original.request.PreviousResponseID != anchor {
		return nil, ErrCodexWSInvalidFrame
	}
	encoded, removed, err := codexWSObjectWithoutPrewarmAnchor(original.encoded, anchor, true)
	if err != nil {
		return nil, err
	}
	if removed == 0 {
		clearBytes(encoded)
		return nil, ErrCodexWSInvalidFrame
	}
	rewritten, err := newCodexWSPendingFrameOwned(original.messageType, encoded)
	if err != nil {
		return nil, err
	}
	if rewritten.request.PreviousResponseID != "" || rewritten.request.HasPreviousResponseID || rewritten.key != original.key || rewritten.request.Model != original.request.Model {
		rewritten.Release()
		return nil, ErrCodexWSInvalidFrame
	}
	return rewritten, nil
}

func codexWSObjectWithoutPrewarmAnchor(source []byte, anchor string, rewriteParams bool) (result []byte, removed int, returnErr error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, 0, errors.Join(ErrCodexWSInvalidFrame, err)
	}
	var encoded bytes.Buffer
	encoded.Grow(len(source))
	encoded.WriteByte('{')
	transferred := false
	defer func() {
		if !transferred {
			clearBytes(encoded.Bytes())
		}
	}()
	written := 0
	previousFound := false
	for decoder.More() {
		keyToken, tokenErr := decoder.Token()
		key, ok := keyToken.(string)
		if tokenErr != nil || !ok {
			return nil, 0, errors.Join(ErrCodexWSInvalidFrame, tokenErr)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			clearBytes(value)
			return nil, 0, errors.Join(ErrCodexWSInvalidFrame, err)
		}
		if key == "previous_response_id" {
			if previousFound {
				clearBytes(value)
				return nil, 0, ErrCodexWSInvalidFrame
			}
			previousFound = true
			var previous string
			valueErr := json.Unmarshal(value, &previous)
			clearBytes(value)
			if valueErr != nil || previous != anchor {
				return nil, 0, errors.Join(ErrCodexWSInvalidFrame, valueErr)
			}
			removed++
			continue
		}
		if rewriteParams && key == "params" && len(bytes.TrimSpace(value)) > 0 && bytes.TrimSpace(value)[0] == '{' {
			rewritten, paramsRemoved, rewriteErr := codexWSObjectWithoutPrewarmAnchor(value, anchor, false)
			clearBytes(value)
			if rewriteErr != nil {
				return nil, 0, rewriteErr
			}
			value = rewritten
			removed += paramsRemoved
		}
		encodedKey, marshalErr := json.Marshal(key)
		if marshalErr != nil {
			clearBytes(value)
			return nil, 0, errors.Join(ErrCodexWSInvalidFrame, marshalErr)
		}
		if written > 0 {
			encoded.WriteByte(',')
		}
		encoded.Write(encodedKey)
		encoded.WriteByte(':')
		encoded.Write(value)
		clearBytes(encodedKey)
		clearBytes(value)
		written++
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, 0, errors.Join(ErrCodexWSInvalidFrame, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, 0, errors.Join(ErrCodexWSInvalidFrame, err)
	}
	encoded.WriteByte('}')
	result = encoded.Bytes()
	transferred = true
	return result, removed, nil
}

func validateCodexWSPrewarmRequest(payload []byte, request CodexProtocolRequest) (CodexProtocolRequest, error) {
	authority, code, err := extractCodexFrozenAuthority(payload, "", false, nil)
	if err == nil {
		request = authority.protocol
	} else if request.Metadata.Source != CodexTurnMetadataHandshake || code != CodexFrozenRequestMetadataAuthority {
		return CodexProtocolRequest{}, fmt.Errorf("%w: %v", ErrCodexWSInvalidFrame, err)
	}
	if request.Type != "response.create" || request.Model == "" || !request.Metadata.Found || request.Metadata.Metadata.RequestKind != CodexRequestPrewarm {
		return CodexProtocolRequest{}, fmt.Errorf("%w: invalid prewarm authority", ErrCodexWSInvalidFrame)
	}
	generate, found, err := codexWSPrewarmGenerate(payload)
	if err != nil || !found || generate {
		return CodexProtocolRequest{}, fmt.Errorf("%w: invalid prewarm generate authority", ErrCodexWSInvalidFrame)
	}
	return request, nil
}

func codexWSPrewarmGenerate(payload []byte) (bool, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false, false, err
	}
	var generate bool
	found := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return false, false, err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false, false, err
		}
		if key != "generate" {
			continue
		}
		if found {
			return false, false, errors.New("duplicate prewarm generate field")
		}
		found = true
		if err := json.Unmarshal(value, &generate); err != nil || string(value) == "null" {
			return false, false, errors.New("invalid prewarm generate field")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return false, false, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false, false, errors.New("invalid trailing prewarm JSON")
	}
	return generate, found, nil
}

func (frame *codexWSPendingFrame) Release() {
	if frame == nil || frame.released {
		return
	}
	clear(frame.encoded)
	frame.encoded = nil
	frame.request = CodexProtocolRequest{}
	frame.key = LeaseKey{}
	frame.portable = false
	frame.prewarm = false
	frame.diagnostics = nil
	frame.released = true
}
