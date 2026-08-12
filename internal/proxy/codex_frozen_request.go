package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
)

var ErrCodexFrozenRequestReleased = errors.New("Codex frozen request released")

// CodexFrozenRequestErrorCode classifies failures before any account dispatch.
type CodexFrozenRequestErrorCode string

const (
	CodexFrozenRequestCanceled            CodexFrozenRequestErrorCode = "canceled"
	CodexFrozenRequestUnsupportedEncoding CodexFrozenRequestErrorCode = "unsupported_encoding"
	CodexFrozenRequestEncodedLimit        CodexFrozenRequestErrorCode = "encoded_limit"
	CodexFrozenRequestDecodedLimit        CodexFrozenRequestErrorCode = "decoded_limit"
	CodexFrozenRequestDecodeFailed        CodexFrozenRequestErrorCode = "decode_failed"
	CodexFrozenRequestMetadataAuthority   CodexFrozenRequestErrorCode = "metadata_authority"
	CodexFrozenRequestModelAuthority      CodexFrozenRequestErrorCode = "model_authority"
	CodexFrozenRequestProtocolInvalid     CodexFrozenRequestErrorCode = "protocol_invalid"
	CodexFrozenRequestChoiceInvalid       CodexFrozenRequestErrorCode = "choice_invalid"
	CodexFrozenRequestTransformFailed     CodexFrozenRequestErrorCode = "transform_failed"
	CodexFrozenRequestEncodeFailed        CodexFrozenRequestErrorCode = "encode_failed"
	CodexFrozenRequestConsumed            CodexFrozenRequestErrorCode = "consumed"
)

// CodexFrozenRequestError deliberately carries no request, identity, header,
// or arbitrary cause material. Its private identity is restricted to safe
// lifecycle sentinels.
type CodexFrozenRequestError struct {
	Code     CodexFrozenRequestErrorCode
	identity error
}

func (e *CodexFrozenRequestError) Error() string {
	if e == nil {
		return "Codex frozen request failed"
	}
	return "Codex frozen request failed: " + string(e.Code)
}

func (e *CodexFrozenRequestError) Is(target error) bool {
	return e != nil && e.identity != nil && target == e.identity
}

func newCodexFrozenRequestError(code CodexFrozenRequestErrorCode, cause error) error {
	var identity error
	switch cause {
	case context.Canceled, context.DeadlineExceeded, ErrCodexFrozenRequestReleased:
		identity = cause
	}
	return &CodexFrozenRequestError{Code: code, identity: identity}
}

// CodexRequestHeadroom is the one-shot Headroom boundary used before replay
// bytes freeze. Implementations must observe ctx and must not retain body.
type CodexRequestHeadroom interface {
	CompressResponses(context.Context, []byte, HeadroomMode) ([]byte, int, error)
}

// CodexRequestHeadroomFunc adapts a function to CodexRequestHeadroom.
type CodexRequestHeadroomFunc func(context.Context, []byte, HeadroomMode) ([]byte, int, error)

func (function CodexRequestHeadroomFunc) CompressResponses(ctx context.Context, body []byte, mode HeadroomMode) ([]byte, int, error) {
	return function(ctx, body, mode)
}

type codexRequestHeadroomAdapter struct {
	bridge *HeadroomBridge
}

// NewCodexRequestHeadroomAdapter adapts the live bridge to one context-aware
// pre-freeze transform. A nil bridge disables Headroom preparation.
func NewCodexRequestHeadroomAdapter(bridge *HeadroomBridge) CodexRequestHeadroom {
	if bridge == nil {
		return nil
	}
	return codexRequestHeadroomAdapter{bridge: bridge}
}

func (adapter codexRequestHeadroomAdapter) CompressResponses(ctx context.Context, body []byte, mode HeadroomMode) ([]byte, int, error) {
	var transformed []byte
	var saved int
	var err error
	if mode == HeadroomModeCache {
		transformed, saved, err = adapter.bridge.CompressResponsesCacheContext(ctx, body)
	} else {
		transformed, saved, err = adapter.bridge.CompressResponsesContext(ctx, body, mode)
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return transformed, saved, err
	}
	fmt.Fprintln(os.Stderr, "cq: headroom: compression unavailable")
	return bytes.Clone(body), 0, nil
}

// CodexFrozenRequestInspection owns one decoded view until Freeze or
// Release consumes it. Source metadata and model authority are parsed once.
type CodexFrozenRequestInspection struct {
	state         *codexFrozenRequestInspectionState
	encodeRequest func([]byte, string, CodexZstdLimits) ([]byte, error)
}

type codexFrozenRequestInspectionState struct {
	mu              sync.Mutex
	encoded         []byte
	decoded         []byte
	headers         http.Header
	encoding        string
	protocol        CodexProtocolRequest
	modelAuthority  codexFrozenModelAuthority
	previous        codexFrozenPreviousAuthority
	metadataSources codexFrozenMetadataSources
	released        bool
}

// CodexFrozenRequest owns one immutable post-transform replay envelope.
type CodexFrozenRequest struct {
	state *codexFrozenRequestState
}

type codexFrozenRequestState struct {
	mu              sync.Mutex
	envelope        *CodexRequestEnvelope
	protocol        CodexProtocolRequest
	choice          RouteChoice
	headroomSavings int
	released        bool
}

// InspectCodexNativeRequest performs content decoding and strict source
// authority parsing without dispatching, transforming, or retaining secrets.
func InspectCodexNativeRequest(ctx context.Context, encoded []byte, headers http.Header) (*CodexFrozenRequestInspection, error) {
	codexInstalledHTTPTraceFromContext(ctx).recordInspect()
	if err := codexFrozenRequestContextError(ctx); err != nil {
		return nil, err
	}
	replayHeaders := codexReplayHeaders(headers)
	originalEncoding, err := parseCodexContentEncoding(headers)
	if err != nil {
		clear(replayHeaders)
		return nil, classifyCodexFrozenCodecError(err, false)
	}
	encoding, err := parseCodexContentEncoding(replayHeaders)
	if err != nil {
		clear(replayHeaders)
		return nil, classifyCodexFrozenCodecError(err, false)
	}
	if originalEncoding != "" && encoding == "" {
		clear(replayHeaders)
		return nil, newCodexFrozenRequestError(CodexFrozenRequestProtocolInvalid, errors.New("content encoding excluded by connection authority"))
	}
	limits := codexHTTPZstdLimits()
	decodedRequest, err := DecodeCodexRequest(encoded, encoding, limits)
	if err != nil {
		clear(replayHeaders)
		return nil, classifyCodexFrozenCodecError(err, false)
	}
	encoding = decodedRequest.Encoding()
	normaliseCodexFrozenFraming(replayHeaders, encoding)
	ownedEncoded := decodedRequest.original
	ownedDecoded := decodedRequest.decoded
	fail := func(code CodexFrozenRequestErrorCode, cause error) (*CodexFrozenRequestInspection, error) {
		clearBytes(ownedEncoded)
		clearBytes(ownedDecoded)
		clear(replayHeaders)
		return nil, newCodexFrozenRequestError(code, cause)
	}
	if err := codexFrozenRequestContextError(ctx); err != nil {
		clearBytes(ownedEncoded)
		clearBytes(ownedDecoded)
		clear(replayHeaders)
		return nil, err
	}
	directMetadata, directMetadataPresent, err := singleCodexFrozenHeader(replayHeaders, codexTurnMetadataKey)
	if err != nil {
		return fail(CodexFrozenRequestMetadataAuthority, err)
	}
	authority, code, err := extractCodexFrozenAuthority(ownedDecoded, directMetadata, directMetadataPresent, replayHeaders)
	if err != nil {
		return fail(code, err)
	}
	switch authority.protocol.Metadata.Metadata.RequestKind {
	case CodexRequestTurn, CodexRequestCompaction:
	default:
		return fail(CodexFrozenRequestMetadataAuthority, errors.New("unsupported native HTTP request kind"))
	}
	if err := codexFrozenRequestContextError(ctx); err != nil {
		clearBytes(ownedEncoded)
		clearBytes(ownedDecoded)
		clear(replayHeaders)
		return nil, err
	}
	return &CodexFrozenRequestInspection{
		state: &codexFrozenRequestInspectionState{
			encoded:         ownedEncoded,
			decoded:         ownedDecoded,
			headers:         replayHeaders,
			encoding:        encoding,
			protocol:        authority.protocol,
			modelAuthority:  authority.model,
			previous:        authority.previous,
			metadataSources: authority.metadataSources,
		},
	}, nil
}

// Protocol returns the strictly parsed source protocol view.
func (inspection *CodexFrozenRequestInspection) Protocol() (CodexProtocolRequest, error) {
	if inspection == nil || inspection.state == nil {
		return CodexProtocolRequest{}, ErrCodexFrozenRequestReleased
	}
	state := inspection.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.released {
		return CodexProtocolRequest{}, ErrCodexFrozenRequestReleased
	}
	return state.protocol, nil
}

// Freeze consumes inspection, applies the chosen model and optional Headroom
// operation once, encodes at most once, and transfers ownership to an immutable
// replay envelope. Every return consumes inspection, including errors.
func (inspection *CodexFrozenRequestInspection) Freeze(ctx context.Context, choice RouteChoice, headroom CodexRequestHeadroom, mode HeadroomMode) (*CodexFrozenRequest, error) {
	trace := codexInstalledHTTPTraceFromContext(ctx)
	trace.recordFreeze()
	encoded, decoded, headers, encoding, protocol, modelAuthority, previousAuthority, metadataSources, err := inspection.consume()
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			clearBytes(encoded)
			clearBytes(decoded)
			clear(headers)
		}
	}()
	if err := codexFrozenRequestContextError(ctx); err != nil {
		return nil, err
	}
	if err := validateCodexFrozenRequestChoice(choice, protocol.Model); err != nil {
		return nil, newCodexFrozenRequestError(CodexFrozenRequestChoiceInvalid, err)
	}
	choice = cloneRouteChoice(choice)

	preparedDecoded := decoded
	modelChanged := !strings.EqualFold(ParseModel(choice.RequestedModel), ParseModel(choice.EffectiveModel))
	if modelChanged {
		rewritten, rewriteErr := rewriteCodexFrozenModel(preparedDecoded, modelAuthority, choice.EffectiveModel)
		if rewriteErr != nil {
			return nil, rewriteErr
		}
		clearBytes(preparedDecoded)
		preparedDecoded = rewritten
		decoded = preparedDecoded
		trace.recordModelRewrite()
	}

	headroomChanged := false
	headroomSavings := 0
	if headroom != nil {
		if err := codexFrozenRequestContextError(ctx); err != nil {
			return nil, err
		}
		headroomInput := bytes.Clone(preparedDecoded)
		transformed, savings, transformErr := headroom.CompressResponses(ctx, headroomInput, mode)
		if transformErr != nil {
			clearBytes(transformed)
			clearBytes(headroomInput)
			if contextErr := codexFrozenRequestContextError(ctx); contextErr != nil {
				return nil, contextErr
			}
			return nil, newCodexFrozenRequestError(CodexFrozenRequestTransformFailed, transformErr)
		}
		transformedCopy := bytes.Clone(transformed)
		headroomChanged = !bytes.Equal(transformedCopy, preparedDecoded)
		clearBytes(transformed)
		clearBytes(headroomInput)
		if err := codexFrozenRequestContextError(ctx); err != nil {
			clearBytes(transformedCopy)
			return nil, err
		}
		if headroomChanged {
			clearBytes(preparedDecoded)
			preparedDecoded = transformedCopy
			decoded = preparedDecoded
			trace.recordHeadroomTransform()
		} else {
			clearBytes(transformedCopy)
		}
		headroomSavings = savings
	}
	if modelChanged || headroomChanged {
		directMetadata, directMetadataPresent, headerErr := singleCodexFrozenHeader(headers, codexTurnMetadataKey)
		if headerErr != nil {
			return nil, newCodexFrozenRequestError(CodexFrozenRequestMetadataAuthority, headerErr)
		}
		preparedAuthority, code, authorityErr := extractCodexFrozenAuthority(preparedDecoded, directMetadata, directMetadataPresent, headers)
		if authorityErr != nil {
			return nil, newCodexFrozenRequestError(code, authorityErr)
		}
		if code, authorityErr = validateCodexFrozenPreparedAuthority(preparedAuthority, protocol, modelAuthority, previousAuthority, metadataSources, choice.EffectiveModel); authorityErr != nil {
			return nil, newCodexFrozenRequestError(code, authorityErr)
		}
	}

	if err := codexFrozenRequestContextError(ctx); err != nil {
		return nil, err
	}
	preparedEncoded := encoded
	if modelChanged || headroomChanged {
		clearBytes(preparedEncoded)
		preparedEncoded = nil
		trace.recordEncode()
		preparedEncoded, err = inspection.encode(preparedDecoded, encoding, codexHTTPZstdLimits())
		if err != nil {
			return nil, classifyCodexFrozenCodecError(err, true)
		}
		encoded = preparedEncoded
		normaliseCodexFrozenFraming(headers, encoding)
	}
	if err := codexFrozenRequestContextError(ctx); err != nil {
		return nil, err
	}

	meter := codexInstalledHTTPReplayMeterFromContext(ctx)
	retainedBytes := uint64(len(preparedEncoded)) + uint64(len(preparedDecoded))
	if meter != nil && !meter.retain(retainedBytes) {
		return nil, newCodexFrozenRequestError(CodexFrozenRequestEncodeFailed, errors.New("replay envelope accounting unavailable"))
	}
	envelope := &CodexRequestEnvelope{
		encoded:        preparedEncoded,
		decoded:        preparedDecoded,
		headers:        headers,
		effectiveModel: strings.Clone(choice.EffectiveModel),
		meter:          meter,
		retainedBytes:  retainedBytes,
	}
	envelope.ownedBytes = codexProcessRuntimeObservability.ownReplayBytes(envelope.encoded, envelope.decoded)
	frozen := &CodexFrozenRequest{
		state: &codexFrozenRequestState{
			envelope:        envelope,
			protocol:        protocol,
			choice:          choice,
			headroomSavings: headroomSavings,
		},
	}
	encoded = nil
	decoded = nil
	headers = nil
	cleanup = false
	return frozen, nil
}

func (inspection *CodexFrozenRequestInspection) encode(body []byte, contentEncoding string, limits CodexZstdLimits) ([]byte, error) {
	if inspection != nil && inspection.encodeRequest != nil {
		return inspection.encodeRequest(body, contentEncoding, limits)
	}
	return EncodeCodexRequest(body, contentEncoding, limits)
}

func (inspection *CodexFrozenRequestInspection) consume() ([]byte, []byte, http.Header, string, CodexProtocolRequest, codexFrozenModelAuthority, codexFrozenPreviousAuthority, codexFrozenMetadataSources, error) {
	if inspection == nil || inspection.state == nil {
		return nil, nil, nil, "", CodexProtocolRequest{}, codexFrozenModelAuthority{}, codexFrozenPreviousAuthority{}, 0, newCodexFrozenRequestError(CodexFrozenRequestConsumed, ErrCodexFrozenRequestReleased)
	}
	state := inspection.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.released {
		return nil, nil, nil, "", CodexProtocolRequest{}, codexFrozenModelAuthority{}, codexFrozenPreviousAuthority{}, 0, newCodexFrozenRequestError(CodexFrozenRequestConsumed, ErrCodexFrozenRequestReleased)
	}
	encoded := state.encoded
	decoded := state.decoded
	headers := state.headers
	encoding := state.encoding
	protocol := state.protocol
	modelAuthority := state.modelAuthority
	previous := state.previous
	metadataSources := state.metadataSources
	state.encoded = nil
	state.decoded = nil
	state.headers = nil
	state.encoding = ""
	state.protocol = CodexProtocolRequest{}
	state.modelAuthority = codexFrozenModelAuthority{}
	state.previous = codexFrozenPreviousAuthority{}
	state.metadataSources = 0
	state.released = true
	return encoded, decoded, headers, encoding, protocol, modelAuthority, previous, metadataSources, nil
}

// Release consumes and best-effort clears inspection state.
func (inspection *CodexFrozenRequestInspection) Release() {
	encoded, decoded, headers, _, _, _, _, _, err := inspection.consume()
	if err != nil {
		return
	}
	clearBytes(encoded)
	clearBytes(decoded)
	clear(headers)
}

// Replay returns a snapshot over the exact frozen encoded bytes.
func (request *CodexFrozenRequest) Replay() (*CodexRequestReplay, error) {
	if request == nil || request.state == nil {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	state := request.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.released || state.envelope == nil {
		return nil, ErrCodexRequestEnvelopeReleased
	}
	return state.envelope.Replay()
}

// Protocol returns the original parsed authority view, not reparsed transform output.
func (request *CodexFrozenRequest) Protocol() (CodexProtocolRequest, error) {
	if request == nil || request.state == nil {
		return CodexProtocolRequest{}, ErrCodexFrozenRequestReleased
	}
	state := request.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.released {
		return CodexProtocolRequest{}, ErrCodexFrozenRequestReleased
	}
	return state.protocol, nil
}

// Choice returns a copy of the indivisible account/model/bucket choice.
func (request *CodexFrozenRequest) Choice() (RouteChoice, error) {
	if request == nil || request.state == nil {
		return RouteChoice{}, ErrCodexFrozenRequestReleased
	}
	state := request.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.released {
		return RouteChoice{}, ErrCodexFrozenRequestReleased
	}
	return cloneRouteChoice(state.choice), nil
}

// HeadroomSavings returns one pre-freeze transform's reported token savings.
func (request *CodexFrozenRequest) HeadroomSavings() int {
	if request == nil || request.state == nil {
		return 0
	}
	state := request.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.released {
		return 0
	}
	return state.headroomSavings
}

// Release clears request authority and releases all replay-envelope ownership.
func (request *CodexFrozenRequest) Release() {
	if request == nil || request.state == nil {
		return
	}
	state := request.state
	state.mu.Lock()
	if state.released {
		state.mu.Unlock()
		return
	}
	envelope := state.envelope
	state.envelope = nil
	state.protocol = CodexProtocolRequest{}
	clear(state.choice.RequiredBuckets)
	state.choice = RouteChoice{}
	state.headroomSavings = 0
	state.released = true
	state.mu.Unlock()
	envelope.Release()
}

func validateCodexFrozenRequestChoice(choice RouteChoice, requestedModel string) error {
	if choice.AccountKey == "" || choice.RequestedModel == "" || choice.EffectiveModel == "" || choice.RequestedModel != requestedModel || len(choice.RequiredBuckets) == 0 {
		return errors.New("route choice does not match inspected request")
	}
	effectiveBucket := CapacityBucketForModel(choice.EffectiveModel)
	hasEffectiveBucket := false
	seen := make(map[CapacityBucket]struct{}, len(choice.RequiredBuckets))
	for _, bucket := range choice.RequiredBuckets {
		if bucket == "" {
			return errors.New("route choice has an empty capacity bucket")
		}
		if _, duplicate := seen[bucket]; duplicate {
			return errors.New("route choice has duplicate capacity buckets")
		}
		seen[bucket] = struct{}{}
		if bucket == effectiveBucket {
			hasEffectiveBucket = true
		}
	}
	if !hasEffectiveBucket {
		return errors.New("route choice omits effective model capacity")
	}
	return nil
}

type codexFrozenModelLocation uint8

const (
	codexFrozenModelRoot codexFrozenModelLocation = iota + 1
	codexFrozenModelParams
)

type codexFrozenRawValue struct {
	start   int
	end     int
	present bool
}

func (value codexFrozenRawValue) bytes(source []byte) []byte {
	if !value.present || value.start < 0 || value.end < value.start || value.end > len(source) {
		return nil
	}
	return source[value.start:value.end]
}

type codexFrozenModelAuthority struct {
	location codexFrozenModelLocation
	value    codexFrozenRawValue
}

type codexFrozenOptionalStringShape uint8

const (
	codexFrozenOptionalAbsent codexFrozenOptionalStringShape = iota
	codexFrozenOptionalNull
	codexFrozenOptionalEmpty
	codexFrozenOptionalValue
)

type codexFrozenPreviousAuthority struct {
	root   codexFrozenOptionalStringShape
	params codexFrozenOptionalStringShape
}

type codexFrozenMetadataSources uint8

const (
	codexFrozenMetadataNestedSource codexFrozenMetadataSources = 1 << iota
	codexFrozenMetadataHeaderSource
	codexFrozenMetadataFlatSource
)

type codexFrozenMetadataFields struct {
	sessionID       codexFrozenRawValue
	threadID        codexFrozenRawValue
	turnID          codexFrozenRawValue
	windowID        codexFrozenRawValue
	requestKind     codexFrozenRawValue
	compaction      codexFrozenRawValue
	compactionPhase codexFrozenRawValue
	any             bool
}

type codexFrozenScanResult struct {
	typeValue         codexFrozenRawValue
	rootModel         codexFrozenRawValue
	paramsModel       codexFrozenRawValue
	rootPrevious      codexFrozenRawValue
	paramsPrevious    codexFrozenRawValue
	params            codexFrozenRawValue
	clientMetadata    codexFrozenRawValue
	nestedMetadata    codexFrozenRawValue
	nestedFields      codexFrozenMetadataFields
	flatFields        codexFrozenMetadataFields
	hasEncryptedState bool
}

type codexFrozenAuthority struct {
	protocol        CodexProtocolRequest
	model           codexFrozenModelAuthority
	previous        codexFrozenPreviousAuthority
	metadataSources codexFrozenMetadataSources
}

type codexFrozenAuthorityFailure struct {
	code CodexFrozenRequestErrorCode
	err  error
}

func (failure *codexFrozenAuthorityFailure) Error() string { return failure.err.Error() }
func (failure *codexFrozenAuthorityFailure) Unwrap() error { return failure.err }

func newCodexFrozenAuthorityFailure(code CodexFrozenRequestErrorCode, message string) error {
	return &codexFrozenAuthorityFailure{code: code, err: errors.New(message)}
}

func codexFrozenAuthorityFailureCode(err error) CodexFrozenRequestErrorCode {
	var failure *codexFrozenAuthorityFailure
	if errors.As(err, &failure) {
		return failure.code
	}
	return CodexFrozenRequestProtocolInvalid
}

func extractCodexFrozenAuthority(body []byte, directMetadata string, directMetadataPresent bool, headers http.Header) (codexFrozenAuthority, CodexFrozenRequestErrorCode, error) {
	scanner := codexFrozenJSONScanner{source: body}
	result, err := scanner.scanRequest()
	if err != nil {
		return codexFrozenAuthority{}, codexFrozenAuthorityFailureCode(err), err
	}

	if result.params.present && !codexFrozenRawNullOrObject(result.params.bytes(body)) {
		err = newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request params must be an object")
		return codexFrozenAuthority{}, codexFrozenAuthorityFailureCode(err), err
	}
	if result.clientMetadata.present && !codexFrozenRawNullOrObject(result.clientMetadata.bytes(body)) {
		err = newCodexFrozenAuthorityFailure(CodexFrozenRequestMetadataAuthority, "client metadata must be an object")
		return codexFrozenAuthority{}, codexFrozenAuthorityFailureCode(err), err
	}

	model := codexFrozenModelAuthority{}
	switch {
	case result.rootModel.present && result.paramsModel.present:
		err = newCodexFrozenAuthorityFailure(CodexFrozenRequestModelAuthority, "native request requires one model authority")
	case result.rootModel.present:
		model = codexFrozenModelAuthority{location: codexFrozenModelRoot, value: result.rootModel}
	case result.paramsModel.present:
		model = codexFrozenModelAuthority{location: codexFrozenModelParams, value: result.paramsModel}
	default:
		err = newCodexFrozenAuthorityFailure(CodexFrozenRequestModelAuthority, "native request requires one model authority")
	}
	if err != nil {
		return codexFrozenAuthority{}, codexFrozenAuthorityFailureCode(err), err
	}
	modelName, err := decodeCodexFrozenString(model.value.bytes(body), len(body), "native request model")
	if err != nil || modelName == "" {
		if err == nil {
			err = errors.New("native request model must be non-empty")
		}
		err = &codexFrozenAuthorityFailure{code: CodexFrozenRequestModelAuthority, err: err}
		return codexFrozenAuthority{}, CodexFrozenRequestModelAuthority, err
	}

	typeName, err := decodeCodexFrozenOptionalString(result.typeValue.bytes(body), codexTurnIDMaxBytes, "native request type")
	if err != nil {
		err = &codexFrozenAuthorityFailure{code: CodexFrozenRequestProtocolInvalid, err: err}
		return codexFrozenAuthority{}, CodexFrozenRequestProtocolInvalid, err
	}
	previous, previousAuthority, err := reconcileCodexFrozenPrevious(body, result.rootPrevious, result.paramsPrevious)
	if err != nil {
		err = &codexFrozenAuthorityFailure{code: CodexFrozenRequestProtocolInvalid, err: err}
		return codexFrozenAuthority{}, CodexFrozenRequestProtocolInvalid, err
	}

	metadataResults := make([]CodexTurnMetadataResult, 0, 3)
	metadataSources := codexFrozenMetadataSources(0)
	if result.nestedMetadata.present {
		metadata, parseErr := parseCodexFrozenMetadataValue(body, result.nestedMetadata, &result.nestedFields, len(body))
		if parseErr != nil {
			err = &codexFrozenAuthorityFailure{code: CodexFrozenRequestMetadataAuthority, err: parseErr}
			return codexFrozenAuthority{}, CodexFrozenRequestMetadataAuthority, err
		}
		metadataResults = append(metadataResults, CodexTurnMetadataResult{Metadata: metadata, Source: CodexTurnMetadataNested, Found: true, Strong: true})
		metadataSources |= codexFrozenMetadataNestedSource
	}
	if directMetadataPresent {
		directBytes := []byte(directMetadata)
		metadata, parseErr := parseCodexFrozenMetadataDocument(directBytes, codexTurnMetadataMaxBytes)
		clearBytes(directBytes)
		if parseErr != nil {
			err = &codexFrozenAuthorityFailure{code: CodexFrozenRequestMetadataAuthority, err: parseErr}
			return codexFrozenAuthority{}, CodexFrozenRequestMetadataAuthority, err
		}
		metadataResults = append(metadataResults, CodexTurnMetadataResult{Metadata: metadata, Source: CodexTurnMetadataHeader, Found: true, Strong: true})
		metadataSources |= codexFrozenMetadataHeaderSource
	}
	if result.flatFields.any {
		metadata, parseErr := decodeCodexFrozenMetadataFields(body, result.flatFields)
		if parseErr != nil {
			err = &codexFrozenAuthorityFailure{code: CodexFrozenRequestMetadataAuthority, err: parseErr}
			return codexFrozenAuthority{}, CodexFrozenRequestMetadataAuthority, err
		}
		if len(metadataResults) == 0 {
			metadataResults = append(metadataResults, CodexTurnMetadataResult{Metadata: metadata, Source: CodexTurnMetadataFlat, Found: true, Strong: true})
		} else if validateErr := validateCodexFrozenPartialMetadata(result.flatFields, metadata, metadataResults[0].Metadata); validateErr != nil {
			err = &codexFrozenAuthorityFailure{code: CodexFrozenRequestMetadataAuthority, err: validateErr}
			return codexFrozenAuthority{}, CodexFrozenRequestMetadataAuthority, err
		}
		metadataSources |= codexFrozenMetadataFlatSource
	}
	if len(metadataResults) == 0 {
		err = newCodexFrozenAuthorityFailure(CodexFrozenRequestMetadataAuthority, "strong turn metadata required")
		return codexFrozenAuthority{}, CodexFrozenRequestMetadataAuthority, err
	}
	for index := range metadataResults {
		if validateErr := validateCodexTurnMetadata(metadataResults[index].Metadata); validateErr != nil {
			err = &codexFrozenAuthorityFailure{code: CodexFrozenRequestMetadataAuthority, err: validateErr}
			return codexFrozenAuthority{}, CodexFrozenRequestMetadataAuthority, err
		}
		if index > 0 && metadataResults[index].Metadata != metadataResults[0].Metadata {
			err = newCodexFrozenAuthorityFailure(CodexFrozenRequestMetadataAuthority, "conflicting strong turn metadata")
			return codexFrozenAuthority{}, CodexFrozenRequestMetadataAuthority, err
		}
	}

	turnState, hasTurnState, err := parseCodexFrozenTurnState(headers)
	if err != nil {
		err = &codexFrozenAuthorityFailure{code: CodexFrozenRequestMetadataAuthority, err: err}
		return codexFrozenAuthority{}, CodexFrozenRequestMetadataAuthority, err
	}
	return codexFrozenAuthority{
		protocol: CodexProtocolRequest{
			Type:               typeName,
			Model:              modelName,
			PreviousResponseID: previous,
			Metadata:           metadataResults[0],
			TurnState:          turnState,
			HasTurnState:       hasTurnState,
			HasEncryptedState:  result.hasEncryptedState,
		},
		model:           model,
		previous:        previousAuthority,
		metadataSources: metadataSources,
	}, "", nil
}

func validateCodexFrozenPartialMetadata(fields codexFrozenMetadataFields, got, expected CodexTurnMetadata) error {
	for _, field := range []struct {
		present  bool
		got      string
		expected string
	}{
		{fields.sessionID.present, got.SessionID, expected.SessionID},
		{fields.threadID.present, got.ThreadID, expected.ThreadID},
		{fields.turnID.present, got.TurnID, expected.TurnID},
		{fields.windowID.present, got.WindowID, expected.WindowID},
		{fields.requestKind.present, string(got.RequestKind), string(expected.RequestKind)},
		{fields.compaction.present || fields.compactionPhase.present, string(got.CompactionPhase), string(expected.CompactionPhase)},
	} {
		if field.present && field.got != field.expected {
			return errors.New("conflicting strong turn metadata")
		}
	}
	return nil
}

func validateCodexFrozenPreparedAuthority(prepared codexFrozenAuthority, source CodexProtocolRequest, sourceModel codexFrozenModelAuthority, sourcePrevious codexFrozenPreviousAuthority, sourceMetadata codexFrozenMetadataSources, effectiveModel string) (CodexFrozenRequestErrorCode, error) {
	if prepared.protocol.Model != effectiveModel || prepared.model.location != sourceModel.location {
		return CodexFrozenRequestModelAuthority, errors.New("transformed request changed effective model authority")
	}
	if prepared.protocol.Metadata != source.Metadata || prepared.metadataSources != sourceMetadata || prepared.protocol.TurnState != source.TurnState || prepared.protocol.HasTurnState != source.HasTurnState {
		return CodexFrozenRequestMetadataAuthority, errors.New("transformed request changed turn authority")
	}
	if prepared.protocol.Type != source.Type || prepared.protocol.PreviousResponseID != source.PreviousResponseID || prepared.previous != sourcePrevious || prepared.protocol.HasEncryptedState != source.HasEncryptedState {
		return CodexFrozenRequestProtocolInvalid, errors.New("transformed request changed protocol authority")
	}
	return "", nil
}

func rewriteCodexFrozenModel(body []byte, authority codexFrozenModelAuthority, effectiveModel string) ([]byte, error) {
	rawModel, err := json.Marshal(effectiveModel)
	if err != nil {
		return nil, newCodexFrozenRequestError(CodexFrozenRequestModelAuthority, err)
	}
	oldLength := authority.value.end - authority.value.start
	if !authority.value.present || authority.value.start < 0 || authority.value.end > len(body) || oldLength < 0 {
		return nil, newCodexFrozenRequestError(CodexFrozenRequestModelAuthority, errors.New("model rewrite authority unavailable"))
	}
	baseLength := len(body) - oldLength
	if len(rawModel) > int(^uint(0)>>1)-baseLength {
		return nil, newCodexFrozenRequestError(CodexFrozenRequestEncodeFailed, errors.New("model rewrite size overflow"))
	}
	newLength := baseLength + len(rawModel)
	rewritten := make([]byte, 0, newLength)
	rewritten = append(rewritten, body[:authority.value.start]...)
	rewritten = append(rewritten, rawModel...)
	rewritten = append(rewritten, body[authority.value.end:]...)
	return rewritten, nil
}

func reconcileCodexFrozenPrevious(body []byte, root, params codexFrozenRawValue) (string, codexFrozenPreviousAuthority, error) {
	rootValue, rootShape, err := decodeCodexFrozenPreviousValue(body, root)
	if err != nil {
		return "", codexFrozenPreviousAuthority{}, err
	}
	paramsValue, paramsShape, err := decodeCodexFrozenPreviousValue(body, params)
	if err != nil {
		return "", codexFrozenPreviousAuthority{}, err
	}
	authority := codexFrozenPreviousAuthority{root: rootShape, params: paramsShape}
	if root.present && params.present && rootValue != paramsValue {
		return "", codexFrozenPreviousAuthority{}, errors.New("conflicting previous response authority")
	}
	if root.present {
		return rootValue, authority, nil
	}
	return paramsValue, authority, nil
}

func decodeCodexFrozenPreviousValue(body []byte, value codexFrozenRawValue) (string, codexFrozenOptionalStringShape, error) {
	if !value.present {
		return "", codexFrozenOptionalAbsent, nil
	}
	raw := value.bytes(body)
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", codexFrozenOptionalNull, nil
	}
	decoded, err := decodeCodexFrozenOptionalString(raw, codexTurnIDMaxBytes, "previous response ID")
	if err != nil {
		return "", codexFrozenOptionalAbsent, err
	}
	if decoded == "" {
		return "", codexFrozenOptionalEmpty, nil
	}
	return decoded, codexFrozenOptionalValue, nil
}

func decodeCodexFrozenOptionalString(raw []byte, limit int, subject string) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	return decodeCodexFrozenString(raw, limit, subject)
}

func decodeCodexFrozenString(raw []byte, limit int, subject string) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("%s is invalid", subject)
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", subject)
	}
	if len(value) > limit {
		return "", fmt.Errorf("%s exceeds limit", subject)
	}
	return value, nil
}

func codexFrozenRawNullOrObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return bytes.Equal(trimmed, []byte("null")) || len(trimmed) > 0 && trimmed[0] == '{'
}

func singleCodexFrozenHeader(headers http.Header, wanted string) (string, bool, error) {
	var values []string
	for name, entries := range headers {
		if strings.EqualFold(name, wanted) {
			values = append(values, entries...)
		}
	}
	if len(values) > 1 {
		return "", false, errors.New("multiple turn metadata headers")
	}
	if len(values) == 1 {
		if len(values[0]) > codexTurnMetadataMaxBytes {
			return "", false, errors.New("Codex turn metadata header exceeds limit")
		}
		return values[0], true, nil
	}
	return "", false, nil
}

func parseCodexFrozenTurnState(headers http.Header) (string, bool, error) {
	canonical := make(http.Header)
	for name, entries := range headers {
		if !strings.EqualFold(name, "x-codex-turn-state") {
			continue
		}
		if !codexJSONASCIIName(name) {
			return "", false, errors.New("non-ASCII Codex turn state authority")
		}
		for _, entry := range entries {
			if len(entry) > codexTurnMetadataMaxBytes {
				return "", false, errors.New("Codex turn state header exceeds limit")
			}
			canonical["X-Codex-Turn-State"] = append(canonical["X-Codex-Turn-State"], entry)
		}
	}
	return ParseCodexTurnStateHeader(canonical)
}

func parseCodexFrozenMetadataValue(source []byte, raw codexFrozenRawValue, captured *codexFrozenMetadataFields, limit int) (CodexTurnMetadata, error) {
	value := bytes.TrimSpace(raw.bytes(source))
	if len(value) == 0 || len(value) > limit {
		return CodexTurnMetadata{}, errors.New("Codex turn metadata exceeds limit")
	}
	if value[0] == '{' {
		return decodeCodexFrozenMetadataFields(source, *captured)
	}
	if value[0] != '"' {
		return CodexTurnMetadata{}, errors.New("Codex turn metadata must be an object")
	}
	var encoded string
	if err := json.Unmarshal(value, &encoded); err != nil {
		return CodexTurnMetadata{}, errors.New("Codex turn metadata string is invalid")
	}
	if len(encoded) > limit {
		return CodexTurnMetadata{}, errors.New("Codex turn metadata exceeds limit")
	}
	decoded := []byte(encoded)
	defer clearBytes(decoded)
	return parseCodexFrozenMetadataObject(decoded)
}

func parseCodexFrozenMetadataDocument(source []byte, limit int) (CodexTurnMetadata, error) {
	if len(source) == 0 || len(source) > limit {
		return CodexTurnMetadata{}, errors.New("Codex turn metadata exceeds limit")
	}
	trimmed := bytes.TrimSpace(source)
	if len(trimmed) == 0 {
		return CodexTurnMetadata{}, errors.New("Codex turn metadata is empty")
	}
	if trimmed[0] == '"' {
		var encoded string
		if err := json.Unmarshal(trimmed, &encoded); err != nil {
			return CodexTurnMetadata{}, errors.New("Codex turn metadata string is invalid")
		}
		if len(encoded) > limit {
			return CodexTurnMetadata{}, errors.New("Codex turn metadata exceeds limit")
		}
		decoded := []byte(encoded)
		defer clearBytes(decoded)
		return parseCodexFrozenMetadataObject(decoded)
	}
	return parseCodexFrozenMetadataObject(source)
}

func parseCodexFrozenMetadataObject(source []byte) (CodexTurnMetadata, error) {
	scanner := codexFrozenJSONScanner{source: source}
	fields, err := scanner.scanMetadata()
	if err != nil {
		return CodexTurnMetadata{}, err
	}
	return decodeCodexFrozenMetadataFields(source, fields)
}

func decodeCodexFrozenMetadataFields(source []byte, fields codexFrozenMetadataFields) (CodexTurnMetadata, error) {
	sessionID, err := decodeCodexFrozenOptionalString(fields.sessionID.bytes(source), codexTurnIDMaxBytes, "Codex session_id")
	if err != nil {
		return CodexTurnMetadata{}, err
	}
	threadID, err := decodeCodexFrozenOptionalString(fields.threadID.bytes(source), codexTurnIDMaxBytes, "Codex thread_id")
	if err != nil {
		return CodexTurnMetadata{}, err
	}
	turnID, err := decodeCodexFrozenOptionalString(fields.turnID.bytes(source), codexTurnIDMaxBytes, "Codex turn_id")
	if err != nil {
		return CodexTurnMetadata{}, err
	}
	windowID, err := decodeCodexFrozenOptionalString(fields.windowID.bytes(source), codexTurnIDMaxBytes, "Codex window_id")
	if err != nil {
		return CodexTurnMetadata{}, err
	}
	requestKind, err := decodeCodexFrozenOptionalString(fields.requestKind.bytes(source), codexTurnIDMaxBytes, "Codex request_kind")
	if err != nil {
		return CodexTurnMetadata{}, err
	}
	var compaction CodexCompactionPhase
	if fields.compaction.present {
		compaction, err = decodeCodexCompactionPhase(fields.compaction.bytes(source))
		if err != nil {
			return CodexTurnMetadata{}, err
		}
	}
	return CodexTurnMetadata{
		SessionID:       sessionID,
		ThreadID:        threadID,
		TurnID:          turnID,
		WindowID:        windowID,
		RequestKind:     CodexRequestKind(requestKind),
		CompactionPhase: compaction,
	}, nil
}

type codexFrozenJSONContext uint8

const (
	codexFrozenJSONGeneric codexFrozenJSONContext = iota
	codexFrozenJSONRoot
	codexFrozenJSONParams
	codexFrozenJSONClientMetadata
	codexFrozenJSONMetadata
	codexFrozenJSONCompaction
)

type codexFrozenJSONScanner struct {
	source []byte
	pos    int
	depth  int
	result codexFrozenScanResult
}

func (scanner *codexFrozenJSONScanner) scanRequest() (codexFrozenScanResult, error) {
	scanner.skipSpace()
	if scanner.pos >= len(scanner.source) || scanner.source[scanner.pos] != '{' {
		return codexFrozenScanResult{}, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request must be a JSON object")
	}
	if _, _, err := scanner.scanValue(codexFrozenJSONRoot, nil); err != nil {
		return codexFrozenScanResult{}, err
	}
	scanner.skipSpace()
	if scanner.pos != len(scanner.source) {
		return codexFrozenScanResult{}, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request has trailing data")
	}
	return scanner.result, nil
}

func (scanner *codexFrozenJSONScanner) scanMetadata() (codexFrozenMetadataFields, error) {
	scanner.skipSpace()
	if scanner.pos >= len(scanner.source) || scanner.source[scanner.pos] != '{' {
		return codexFrozenMetadataFields{}, newCodexFrozenAuthorityFailure(CodexFrozenRequestMetadataAuthority, "Codex turn metadata must be an object")
	}
	var fields codexFrozenMetadataFields
	if _, _, err := scanner.scanValue(codexFrozenJSONMetadata, &fields); err != nil {
		return codexFrozenMetadataFields{}, err
	}
	scanner.skipSpace()
	if scanner.pos != len(scanner.source) {
		return codexFrozenMetadataFields{}, newCodexFrozenAuthorityFailure(CodexFrozenRequestMetadataAuthority, "Codex turn metadata has trailing data")
	}
	return fields, nil
}

func (scanner *codexFrozenJSONScanner) scanValue(context codexFrozenJSONContext, metadata *codexFrozenMetadataFields) (int, int, error) {
	scanner.skipSpace()
	start := scanner.pos
	if start >= len(scanner.source) {
		return 0, 0, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "unexpected end of native request")
	}
	switch scanner.source[scanner.pos] {
	case '{':
		if scanner.depth >= 512 {
			return 0, 0, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request nesting exceeds limit")
		}
		scanner.depth++
		err := scanner.scanObject(context, metadata)
		scanner.depth--
		if err != nil {
			return 0, 0, err
		}
	case '[':
		if scanner.depth >= 512 {
			return 0, 0, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request nesting exceeds limit")
		}
		scanner.depth++
		err := scanner.scanArray()
		scanner.depth--
		if err != nil {
			return 0, 0, err
		}
	case '"':
		if _, _, err := scanner.scanString(); err != nil {
			return 0, 0, err
		}
	case 't':
		if err := scanner.scanLiteral("true"); err != nil {
			return 0, 0, err
		}
	case 'f':
		if err := scanner.scanLiteral("false"); err != nil {
			return 0, 0, err
		}
	case 'n':
		if err := scanner.scanLiteral("null"); err != nil {
			return 0, 0, err
		}
	default:
		if err := scanner.scanNumber(); err != nil {
			return 0, 0, err
		}
	}
	return start, scanner.pos, nil
}

func (scanner *codexFrozenJSONScanner) scanObject(context codexFrozenJSONContext, metadata *codexFrozenMetadataFields) error {
	scanner.pos++
	scanner.skipSpace()
	if scanner.pos < len(scanner.source) && scanner.source[scanner.pos] == '}' {
		scanner.pos++
		return nil
	}
	for {
		keyStart, keyEnd, err := scanner.scanString()
		if err != nil {
			return err
		}
		name := ""
		if keyEnd-keyStart <= 258 {
			if err := json.Unmarshal(scanner.source[keyStart:keyEnd], &name); err != nil {
				return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request field name is invalid")
			}
		}
		scanner.skipSpace()
		if scanner.pos >= len(scanner.source) || scanner.source[scanner.pos] != ':' {
			return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request field is missing a colon")
		}
		scanner.pos++

		field, code, err := codexFrozenAuthorityField(context, name)
		if err != nil {
			return err
		}
		childContext := codexFrozenJSONGeneric
		childMetadata := (*codexFrozenMetadataFields)(nil)
		switch {
		case context == codexFrozenJSONRoot && field == "params":
			childContext = codexFrozenJSONParams
		case context == codexFrozenJSONRoot && field == "client_metadata":
			childContext = codexFrozenJSONClientMetadata
		case context == codexFrozenJSONClientMetadata && field == codexTurnMetadataKey:
			childContext = codexFrozenJSONMetadata
			childMetadata = &scanner.result.nestedFields
		case context == codexFrozenJSONClientMetadata && field == "compaction":
			childContext = codexFrozenJSONCompaction
			childMetadata = &scanner.result.flatFields
		case context == codexFrozenJSONMetadata && field == "compaction":
			childContext = codexFrozenJSONCompaction
			childMetadata = metadata
		}
		valueStart, valueEnd, err := scanner.scanValue(childContext, childMetadata)
		if err != nil {
			return err
		}
		if name == "encrypted_content" {
			scanner.result.hasEncryptedState = true
		}
		if field != "" {
			if err := scanner.captureField(context, field, code, valueStart, valueEnd, metadata); err != nil {
				return err
			}
		}
		scanner.skipSpace()
		if scanner.pos >= len(scanner.source) {
			return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request object is not closed")
		}
		switch scanner.source[scanner.pos] {
		case ',':
			scanner.pos++
			scanner.skipSpace()
		case '}':
			scanner.pos++
			return nil
		default:
			return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request object separator is invalid")
		}
	}
}

func (scanner *codexFrozenJSONScanner) scanArray() error {
	scanner.pos++
	scanner.skipSpace()
	if scanner.pos < len(scanner.source) && scanner.source[scanner.pos] == ']' {
		scanner.pos++
		return nil
	}
	for {
		if _, _, err := scanner.scanValue(codexFrozenJSONGeneric, nil); err != nil {
			return err
		}
		scanner.skipSpace()
		if scanner.pos >= len(scanner.source) {
			return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request array is not closed")
		}
		switch scanner.source[scanner.pos] {
		case ',':
			scanner.pos++
			scanner.skipSpace()
		case ']':
			scanner.pos++
			return nil
		default:
			return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request array separator is invalid")
		}
	}
}

func (scanner *codexFrozenJSONScanner) scanString() (int, int, error) {
	start := scanner.pos
	if start >= len(scanner.source) || scanner.source[start] != '"' {
		return 0, 0, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request field name must be a string")
	}
	scanner.pos++
	for scanner.pos < len(scanner.source) {
		value := scanner.source[scanner.pos]
		scanner.pos++
		switch {
		case value == '"':
			return start, scanner.pos, nil
		case value < 0x20:
			return 0, 0, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request string contains a control byte")
		case value != '\\':
			continue
		}
		if scanner.pos >= len(scanner.source) {
			break
		}
		escape := scanner.source[scanner.pos]
		scanner.pos++
		if strings.ContainsRune(`"\/bfnrt`, rune(escape)) {
			continue
		}
		if escape != 'u' || scanner.pos+4 > len(scanner.source) {
			return 0, 0, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request string escape is invalid")
		}
		for _, digit := range scanner.source[scanner.pos : scanner.pos+4] {
			if !codexFrozenHexDigit(digit) {
				return 0, 0, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request Unicode escape is invalid")
			}
		}
		scanner.pos += 4
	}
	return 0, 0, newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request string is not closed")
}

func (scanner *codexFrozenJSONScanner) scanLiteral(literal string) error {
	if !bytes.HasPrefix(scanner.source[scanner.pos:], []byte(literal)) {
		return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request literal is invalid")
	}
	scanner.pos += len(literal)
	return nil
}

func (scanner *codexFrozenJSONScanner) scanNumber() error {
	start := scanner.pos
	if scanner.pos < len(scanner.source) && scanner.source[scanner.pos] == '-' {
		scanner.pos++
	}
	if scanner.pos >= len(scanner.source) {
		return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request number is invalid")
	}
	if scanner.source[scanner.pos] == '0' {
		scanner.pos++
	} else if scanner.source[scanner.pos] >= '1' && scanner.source[scanner.pos] <= '9' {
		for scanner.pos < len(scanner.source) && scanner.source[scanner.pos] >= '0' && scanner.source[scanner.pos] <= '9' {
			scanner.pos++
		}
	} else {
		return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request number is invalid")
	}
	if scanner.pos < len(scanner.source) && scanner.source[scanner.pos] == '.' {
		scanner.pos++
		fractionStart := scanner.pos
		for scanner.pos < len(scanner.source) && scanner.source[scanner.pos] >= '0' && scanner.source[scanner.pos] <= '9' {
			scanner.pos++
		}
		if scanner.pos == fractionStart {
			return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request number fraction is invalid")
		}
	}
	if scanner.pos < len(scanner.source) && (scanner.source[scanner.pos] == 'e' || scanner.source[scanner.pos] == 'E') {
		scanner.pos++
		if scanner.pos < len(scanner.source) && (scanner.source[scanner.pos] == '+' || scanner.source[scanner.pos] == '-') {
			scanner.pos++
		}
		exponentStart := scanner.pos
		for scanner.pos < len(scanner.source) && scanner.source[scanner.pos] >= '0' && scanner.source[scanner.pos] <= '9' {
			scanner.pos++
		}
		if scanner.pos == exponentStart {
			return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request number exponent is invalid")
		}
	}
	if scanner.pos == start {
		return newCodexFrozenAuthorityFailure(CodexFrozenRequestProtocolInvalid, "native request value is invalid")
	}
	return nil
}

func (scanner *codexFrozenJSONScanner) captureField(context codexFrozenJSONContext, field string, code CodexFrozenRequestErrorCode, start, end int, metadata *codexFrozenMetadataFields) error {
	value := codexFrozenRawValue{start: start, end: end, present: true}
	var target *codexFrozenRawValue
	switch context {
	case codexFrozenJSONRoot:
		switch field {
		case "type":
			target = &scanner.result.typeValue
		case "model":
			target = &scanner.result.rootModel
		case "previous_response_id":
			target = &scanner.result.rootPrevious
		case "params":
			target = &scanner.result.params
		case "client_metadata":
			target = &scanner.result.clientMetadata
		}
	case codexFrozenJSONParams:
		if field == "model" {
			target = &scanner.result.paramsModel
		} else if field == "previous_response_id" {
			target = &scanner.result.paramsPrevious
		}
	case codexFrozenJSONClientMetadata:
		if field == codexTurnMetadataKey {
			target = &scanner.result.nestedMetadata
		} else {
			target = codexFrozenMetadataTarget(&scanner.result.flatFields, field)
			metadata = &scanner.result.flatFields
		}
	case codexFrozenJSONMetadata:
		target = codexFrozenMetadataTarget(metadata, field)
	case codexFrozenJSONCompaction:
		if field == "phase" {
			target = &metadata.compactionPhase
		}
	}
	if target == nil {
		return nil
	}
	if target.present {
		return newCodexFrozenAuthorityFailure(code, "duplicate native request authority field")
	}
	*target = value
	if metadata != nil && context != codexFrozenJSONCompaction && field != codexTurnMetadataKey {
		metadata.any = true
	}
	return nil
}

func codexFrozenMetadataTarget(metadata *codexFrozenMetadataFields, field string) *codexFrozenRawValue {
	if metadata == nil {
		return nil
	}
	switch field {
	case "session_id":
		return &metadata.sessionID
	case "thread_id":
		return &metadata.threadID
	case "turn_id":
		return &metadata.turnID
	case "window_id":
		return &metadata.windowID
	case "request_kind":
		return &metadata.requestKind
	case "compaction":
		return &metadata.compaction
	default:
		return nil
	}
}

func codexFrozenAuthorityField(context codexFrozenJSONContext, name string) (string, CodexFrozenRequestErrorCode, error) {
	fields := []string(nil)
	code := CodexFrozenRequestProtocolInvalid
	switch context {
	case codexFrozenJSONRoot:
		fields = []string{"type", "model", "previous_response_id", "params", "client_metadata"}
	case codexFrozenJSONParams:
		fields = []string{"model", "previous_response_id"}
	case codexFrozenJSONClientMetadata:
		fields = []string{codexTurnMetadataKey, "session_id", "thread_id", "turn_id", "window_id", "request_kind", "compaction"}
		code = CodexFrozenRequestMetadataAuthority
	case codexFrozenJSONMetadata:
		fields = []string{"session_id", "thread_id", "turn_id", "window_id", "request_kind", "compaction"}
		code = CodexFrozenRequestMetadataAuthority
	case codexFrozenJSONCompaction:
		fields = []string{"phase"}
		code = CodexFrozenRequestMetadataAuthority
	}
	for _, field := range fields {
		if !codexJSONNameEqual(name, field) {
			continue
		}
		if field == "model" {
			code = CodexFrozenRequestModelAuthority
		} else if field == "client_metadata" || field == codexTurnMetadataKey {
			code = CodexFrozenRequestMetadataAuthority
		}
		if !codexJSONASCIIName(name) {
			return "", code, newCodexFrozenAuthorityFailure(code, "non-ASCII native request authority field")
		}
		return field, code, nil
	}
	return "", code, nil
}

func (scanner *codexFrozenJSONScanner) skipSpace() {
	for scanner.pos < len(scanner.source) {
		switch scanner.source[scanner.pos] {
		case ' ', '\t', '\r', '\n':
			scanner.pos++
		default:
			return
		}
	}
}

func codexFrozenHexDigit(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func normaliseCodexFrozenFraming(headers http.Header, encoding string) {
	if headers == nil {
		return
	}
	headers.Del("Content-Length")
	switch encoding {
	case "":
		headers.Del("Content-Encoding")
	case "identity":
		headers.Set("Content-Encoding", "identity")
	case "zstd":
		headers.Set("Content-Encoding", "zstd")
	}
}

func classifyCodexFrozenCodecError(err error, encoding bool) error {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "unsupported") || strings.Contains(message, "multiple content encodings") || strings.Contains(message, "content encoding is empty"):
		return newCodexFrozenRequestError(CodexFrozenRequestUnsupportedEncoding, err)
	case strings.Contains(message, "encoded") && strings.Contains(message, "limit"):
		return newCodexFrozenRequestError(CodexFrozenRequestEncodedLimit, err)
	case strings.Contains(message, "decoded") && strings.Contains(message, "limit"), strings.Contains(message, "expansion ratio"):
		return newCodexFrozenRequestError(CodexFrozenRequestDecodedLimit, err)
	case encoding:
		return newCodexFrozenRequestError(CodexFrozenRequestEncodeFailed, err)
	default:
		return newCodexFrozenRequestError(CodexFrozenRequestDecodeFailed, err)
	}
}

func codexFrozenRequestContextError(ctx context.Context) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	return newCodexFrozenRequestError(CodexFrozenRequestCanceled, ctx.Err())
}
