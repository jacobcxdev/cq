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
