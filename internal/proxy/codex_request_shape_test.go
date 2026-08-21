package proxy

import (
	"errors"
	"net/http"
	"testing"
)

func TestParseCodexObservationRequestUsesFrozenAuthorityRules(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "duplicate request discriminator", body: `{"type":"response.create","type":"response.create"}`},
		{name: "case variant request discriminator", body: `{"type":"response.create","TYPE":"response.create"}`},
		{name: "escaped request discriminator", body: `{"type":"response.create","\u0074ype":"response.create"}`},
		{name: "duplicate JSON-RPC discriminator", body: `{"method":"response/create","method":"response/create"}`},
		{name: "case variant JSON-RPC discriminator", body: `{"method":"response/create","METHOD":"response/create"}`},
		{name: "escaped JSON-RPC discriminator", body: `{"method":"response/create","\u006dethod":"response/create"}`},
		{name: "case variant params authority", body: `{"params":{"model":"gpt-5.6-sol","MODEL":"gpt-5.6-sol"}}`},
		{name: "duplicate params container", body: `{"params":{},"PARAMS":{}}`},
		{name: "conflicting previous authority", body: `{"previous_response_id":"first","params":{"previous_response_id":"second"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseCodexObservationRequest([]byte(tt.body), nil); err == nil {
				t.Fatal("ambiguous observation authority accepted")
			}
		})
	}
}

func TestParseCodexObservationRequestAllowsMissingRoutingAuthority(t *testing.T) {
	t.Parallel()
	body := []byte(`{"type":"response.create","previous_response_id":"private-response","reasoning":{"effort":"high"}}`)
	request, err := parseCodexObservationRequest(body, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if request.Type != "response.create" || request.Model != "" || request.PreviousResponseID != "private-response" || !request.HasPreviousResponseID || request.RequestedReasoningEffort != "high" || !request.HasRequestedReasoningEffort || !request.RequestedReasoningEffortValid || request.Metadata.Found {
		t.Fatalf("request = %#v", request)
	}
}

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
		{name: "projected Sol label is raw input", request: CodexProtocolRequest{Model: "gpt_5_6_sol"}, wantModelClass: "other", wantPhase: "unknown"},
		{name: "unknown label is raw input", request: CodexProtocolRequest{Model: "unknown"}, wantModelClass: "other", wantPhase: "unknown"},
		{name: "projected Terra label is raw input", request: CodexProtocolRequest{Model: "gpt_5_6_terra"}, wantModelClass: "other", wantPhase: "unknown"},
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
