package proxy

import (
	"errors"
	"testing"
)

func TestCodexRequestShapeLineage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request CodexProtocolRequest
		err     error
		want    string
	}{
		{name: "previous response ID absent", want: "previous_response_id_absent"},
		{name: "previous response ID non-empty", request: CodexProtocolRequest{PreviousResponseID: "response-private", HasPreviousResponseID: true}, want: "previous_response_id_present"},
		{name: "previous response ID empty", request: CodexProtocolRequest{HasPreviousResponseID: true}, want: "previous_response_id_present"},
		{name: "parse failure", err: errors.New("decode request"), want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyCodexRequestShape(tt.request, tt.err)
			if got.RequestLineage != tt.want {
				t.Fatalf("request lineage = %q, want %q", got.RequestLineage, tt.want)
			}
		})
	}
}

func TestCodexRequestShapeReasoningEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		effort string
		want   string
	}{
		{name: "none", effort: "none", want: "none"},
		{name: "minimal", effort: "minimal", want: "minimal"},
		{name: "low", effort: "low", want: "low"},
		{name: "medium", effort: "medium", want: "medium"},
		{name: "high", effort: "high", want: "high"},
		{name: "extended high", effort: "xhigh", want: "xhigh"},
		{name: "maximum", effort: "max", want: "max"},
		{name: "ultra", effort: "ultra", want: "ultra"},
		{name: "absent", want: "unspecified"},
		{name: "caller controlled", effort: "maximum", want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyCodexRequestShape(CodexProtocolRequest{RequestedReasoningEffort: tt.effort, HasRequestedReasoningEffort: tt.effort != "", RequestedReasoningEffortValid: true}, nil)
			if got.RequestedReasoningEffort != tt.want {
				t.Fatalf("reasoning effort = %q, want %q", got.RequestedReasoningEffort, tt.want)
			}
		})
	}
}

func TestCodexRequestShapeRejectsNonStringEffort(t *testing.T) {
	t.Parallel()
	request, err := ParseCodexProtocolRequest([]byte(`{"reasoning":{"effort":1}}`), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	shape := classifyCodexRequestShape(request, nil)
	if shape.RequestedReasoningEffort != "unknown" {
		t.Fatalf("reasoning effort = %q, want unknown", shape.RequestedReasoningEffort)
	}
}

func TestCodexRequestShapeClassifiesClosedModelAndMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		request         CodexProtocolRequest
		wantModelClass  string
		wantRequestKind string
		wantPhase       string
	}{
		{name: "supported compaction", request: CodexProtocolRequest{Model: "gpt-5.6-sol", Metadata: CodexTurnMetadataResult{Found: true, Strong: true, Metadata: CodexTurnMetadata{SessionID: "session", ThreadID: "thread", TurnID: "turn", RequestKind: CodexRequestCompaction, CompactionPhase: CodexCompactionMidTurn}}}, wantModelClass: "gpt_5_6_sol", wantRequestKind: "compaction", wantPhase: "mid_turn"},
		{name: "validated non compaction", request: CodexProtocolRequest{Model: "gpt-5.6-terra", Metadata: CodexTurnMetadataResult{Found: true, Strong: true, Metadata: CodexTurnMetadata{SessionID: "session", ThreadID: "thread", TurnID: "turn", RequestKind: CodexRequestTurn}}}, wantModelClass: "gpt_5_6_terra", wantRequestKind: "turn", wantPhase: "not_applicable"},
		{name: "weak metadata", request: CodexProtocolRequest{Model: "gpt-5.6-luna", Metadata: CodexTurnMetadataResult{Found: true, Metadata: CodexTurnMetadata{SessionID: "session", ThreadID: "thread", TurnID: "turn", RequestKind: CodexRequestCompaction, CompactionPhase: CodexCompactionPreTurn}}}, wantModelClass: "gpt_5_6_luna", wantRequestKind: "compaction", wantPhase: "unknown"},
		{name: "absent metadata and model", wantModelClass: "unknown", wantPhase: "unknown"},
		{name: "invalid metadata kind", request: CodexProtocolRequest{Metadata: CodexTurnMetadataResult{Found: true, Strong: true, Metadata: CodexTurnMetadata{SessionID: "session", ThreadID: "thread", TurnID: "turn", RequestKind: "caller-private-kind"}}}, wantModelClass: "unknown", wantPhase: "unknown"},
		{name: "other requested model", request: CodexProtocolRequest{Model: "caller-private-model"}, wantModelClass: "other", wantPhase: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyCodexRequestShape(tt.request, nil)
			if got.RequestedModelClass != tt.wantModelClass || got.RequestKind != tt.wantRequestKind || got.CompactionPhase != tt.wantPhase {
				t.Fatalf("shape = %#v, want model class %q request kind %q phase %q", got, tt.wantModelClass, tt.wantRequestKind, tt.wantPhase)
			}
		})
	}
}
