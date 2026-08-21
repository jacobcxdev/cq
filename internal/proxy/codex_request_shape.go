package proxy

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
}

func classifyCodexRequestShape(request CodexProtocolRequest, parseErr error) codexRequestShape {
	if parseErr != nil {
		return codexRequestShape{
			RequestLineage:           codexRequestLineageUnknown,
			RequestedReasoningEffort: codexRequestedReasoningEffortUnknown,
		}
	}
	shape := codexRequestShape{RequestedReasoningEffort: codexRequestedReasoningEffort(request)}
	if request.PreviousResponseID == "" {
		shape.RequestLineage = codexRequestLineagePreviousResponseIDAbsent
	} else {
		shape.RequestLineage = codexRequestLineagePreviousResponseIDPresent
	}
	return shape
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
