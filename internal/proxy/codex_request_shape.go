package proxy

import (
	"context"
	"net/http"
)

const (
	codexRequestLineagePreviousResponseIDAbsent  = "previous_response_id_absent"
	codexRequestLineagePreviousResponseIDPresent = "previous_response_id_present"
	codexRequestLineageUnknown                   = "unknown"

	codexRequestedReasoningEffortUnspecified = "unspecified"
	codexRequestedReasoningEffortUnknown     = "unknown"
)

type codexRequestShape struct {
	RequestLineage           string
	RequestedReasoningEffort string
	RequestedModelClass      string
	RequestKind              string
	CompactionPhase          string
}

func parseCodexObservationRequest(body []byte, headers http.Header) (CodexProtocolRequest, error) {
	directMetadata, directMetadataPresent, err := singleCodexFrozenHeader(headers, codexTurnMetadataKey)
	if err != nil {
		return CodexProtocolRequest{}, err
	}
	authority, _, err := extractCodexFrozenAuthorityWithRequirements(body, directMetadata, directMetadataPresent, nil, false, false, true)
	if err != nil {
		return CodexProtocolRequest{}, err
	}
	return authority.protocol, nil
}

func codexObservationFieldsForRequestShape(shape codexRequestShape) codexObservationFields {
	return codexObservationFields{
		RequestKind:              shape.RequestKind,
		RequestLineage:           shape.RequestLineage,
		RequestedReasoningEffort: shape.RequestedReasoningEffort,
		RequestedModelClass:      shape.RequestedModelClass,
		CompactionPhase:          shape.CompactionPhase,
	}
}

func codexObservationFieldsForProtocol(request CodexProtocolRequest, parseErr error) codexObservationFields {
	fields := codexObservationFieldsForRequestShape(classifyCodexRequestShape(request, parseErr))
	if parseErr != nil || !request.Metadata.Found || validateCodexTurnMetadata(request.Metadata.Metadata) != nil {
		return fields
	}
	if session := request.Metadata.Metadata.SessionID; session != "" {
		fields.SessionKey = hashPrefix("codex-session", session)
		fields.SessionSource = "metadata:session_id"
	}
	if thread := request.Metadata.Metadata.ThreadID; thread != "" {
		fields.ThreadKey = hashPrefix("codex-thread", thread)
	}
	return fields
}

func replaceCodexRequestObservation(ctx context.Context, request CodexProtocolRequest, parseErr error) {
	if ctx == nil {
		return
	}
	diagnostics, _ := ctx.Value(routeDiagnosticsContextKey{}).(*routeDiagnostics)
	if diagnostics == nil {
		return
	}
	fields := codexObservationFieldsForProtocol(request, parseErr)
	updateCodexTraceIdentity(ctx, fields.SessionKey, fields.SessionSource, fields.ThreadKey, fields.RequestKind)
	diagnostics.mu.Lock()
	diagnostics.codex.SessionKey = fields.SessionKey
	diagnostics.codex.SessionSource = fields.SessionSource
	diagnostics.codex.ThreadKey = fields.ThreadKey
	diagnostics.codex.RequestKind = fields.RequestKind
	diagnostics.codex.RequestLineage = fields.RequestLineage
	diagnostics.codex.RequestedReasoningEffort = fields.RequestedReasoningEffort
	diagnostics.codex.RequestedModelClass = fields.RequestedModelClass
	diagnostics.codex.CompactionPhase = fields.CompactionPhase
	diagnostics.mu.Unlock()
}

func replaceCodexRequestShapeObservation(ctx context.Context, shape codexRequestShape) {
	if ctx == nil {
		return
	}
	diagnostics, _ := ctx.Value(routeDiagnosticsContextKey{}).(*routeDiagnostics)
	if diagnostics == nil {
		return
	}
	fields := codexObservationFieldsForRequestShape(shape)
	diagnostics.mu.Lock()
	diagnostics.codex.RequestKind = fields.RequestKind
	diagnostics.codex.RequestLineage = fields.RequestLineage
	diagnostics.codex.RequestedReasoningEffort = fields.RequestedReasoningEffort
	diagnostics.codex.RequestedModelClass = fields.RequestedModelClass
	diagnostics.codex.CompactionPhase = fields.CompactionPhase
	diagnostics.mu.Unlock()
}

func classifyCodexRequestShape(request CodexProtocolRequest, parseErr error) codexRequestShape {
	if parseErr != nil {
		return codexRequestShape{
			RequestLineage:           codexRequestLineageUnknown,
			RequestedReasoningEffort: codexRequestedReasoningEffortUnknown,
			RequestedModelClass:      codexRequestedModelClassUnknown,
			CompactionPhase:          "unknown",
		}
	}
	shape := codexRequestShape{
		RequestedReasoningEffort: codexRequestedReasoningEffort(request),
		RequestedModelClass:      classifyCodexRequestedModelClass(request.Model),
		CompactionPhase:          "unknown",
	}
	if !request.HasPreviousResponseID {
		shape.RequestLineage = codexRequestLineagePreviousResponseIDAbsent
	} else {
		shape.RequestLineage = codexRequestLineagePreviousResponseIDPresent
	}
	metadata := request.Metadata
	if !metadata.Found || validateCodexTurnMetadata(metadata.Metadata) != nil {
		return shape
	}
	shape.RequestKind = string(metadata.Metadata.RequestKind)
	if !metadata.Strong {
		return shape
	}
	if metadata.Metadata.RequestKind != CodexRequestCompaction {
		shape.CompactionPhase = "not_applicable"
		return shape
	}
	switch metadata.Metadata.CompactionPhase {
	case CodexCompactionStandalone, CodexCompactionPreTurn, CodexCompactionMidTurn:
		shape.CompactionPhase = string(metadata.Metadata.CompactionPhase)
	}
	return shape
}

func classifyCodexRequestedModelClass(model string) string {
	switch model {
	case "":
		return codexRequestedModelClassUnknown
	case "gpt-5.6-sol":
		return codexRequestedModelClassSol
	case "gpt-5.6-terra":
		return codexRequestedModelClassTerra
	case "gpt-5.6-luna":
		return codexRequestedModelClassLuna
	default:
		return codexRequestedModelClassOther
	}
}

func codexRequestedReasoningEffort(request CodexProtocolRequest) string {
	if !request.HasRequestedReasoningEffort {
		return codexRequestedReasoningEffortUnspecified
	}
	if !request.RequestedReasoningEffortValid {
		return codexRequestedReasoningEffortUnknown
	}
	switch request.RequestedReasoningEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return request.RequestedReasoningEffort
	default:
		return codexRequestedReasoningEffortUnknown
	}
}
