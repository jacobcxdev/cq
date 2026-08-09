package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTurnMetadataFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		wantSource  CodexTurnMetadataSource
		wantSession string
		wantKind    CodexRequestKind
		wantPhase   CodexCompactionPhase
		wantErr     bool
	}{
		{"nested-string.json", CodexTurnMetadataNested, "session-a", CodexRequestTurn, "", false},
		{"nested-object.json", CodexTurnMetadataNested, "session-object", CodexRequestTurn, "", false},
		{"flat.json", CodexTurnMetadataFlat, "session-flat", CodexRequestTurn, "", false},
		{"prewarm.json", CodexTurnMetadataNested, "session-prewarm", CodexRequestPrewarm, "", false},
		{"memory.json", CodexTurnMetadataNested, "", CodexRequestMemory, "", false},
		{"compaction-pre-turn.json", CodexTurnMetadataNested, "session-c", CodexRequestCompaction, CodexCompactionPreTurn, false},
		{"compaction-mid-turn.json", CodexTurnMetadataNested, "session-c", CodexRequestCompaction, CodexCompactionMidTurn, false},
		{"compaction-standalone.json", CodexTurnMetadataNested, "session-c", CodexRequestCompaction, CodexCompactionStandalone, false},
		{"malformed.json", "", "", "", "", true},
		{"incomplete.json", "", "", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := os.ReadFile(filepath.Join("testdata", "codex-0.146", tc.name))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseCodexTurnMetadata(body, "", nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.Source != tc.wantSource || got.Metadata.SessionID != tc.wantSession || got.Metadata.RequestKind != tc.wantKind || got.Metadata.CompactionPhase != tc.wantPhase {
				t.Fatalf("metadata = %#v", got)
			}
		})
	}
}

func TestTurnMetadataPriority(t *testing.T) {
	t.Parallel()
	nested := `{"client_metadata":{"x-codex-turn-metadata":{"session_id":"nested","thread_id":"t","turn_id":"u","request_kind":"turn"},"session_id":"flat","thread_id":"t","turn_id":"u","request_kind":"turn"}}`
	header := `{"session_id":"header","thread_id":"t","turn_id":"u","request_kind":"turn"}`
	handshake := &CodexTurnMetadata{SessionID: "handshake", ThreadID: "t", TurnID: "u", RequestKind: CodexRequestTurn}
	got, err := ParseCodexTurnMetadata([]byte(nested), header, handshake)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != CodexTurnMetadataNested || got.Metadata.SessionID != "nested" || !got.Strong {
		t.Fatalf("metadata = %#v", got)
	}

	flat := `{"client_metadata":{"session_id":"flat","thread_id":"t","turn_id":"u","request_kind":"turn"}}`
	got, err = ParseCodexTurnMetadata([]byte(flat), header, handshake)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != CodexTurnMetadataHeader || got.Metadata.SessionID != "header" {
		t.Fatalf("metadata = %#v", got)
	}

	got, err = ParseCodexTurnMetadata([]byte(flat), "", handshake)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != CodexTurnMetadataFlat || got.Metadata.SessionID != "flat" {
		t.Fatalf("metadata = %#v", got)
	}

	got, err = ParseCodexTurnMetadata([]byte(`{}`), "", handshake)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != CodexTurnMetadataHandshake || got.Metadata.SessionID != "handshake" || got.Strong {
		t.Fatalf("metadata = %#v", got)
	}
}

func TestTurnMetadataAcceptsCurrentCompactionObject(t *testing.T) {
	t.Parallel()
	header := `{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"compaction","compaction":{"trigger":"automatic","reason":"context_window_exceeded","implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`
	got, err := ParseCodexTurnMetadata(nil, header, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.CompactionPhase != CodexCompactionMidTurn {
		t.Fatalf("metadata = %#v", got.Metadata)
	}
}

func TestTurnMetadataRejectsMalformedHigherPriority(t *testing.T) {
	t.Parallel()
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{","session_id":"flat","thread_id":"t","turn_id":"u","request_kind":"turn"}}`)
	if _, err := ParseCodexTurnMetadata(body, `{"session_id":"header","thread_id":"t","turn_id":"u","request_kind":"turn"}`, nil); err == nil {
		t.Fatal("expected malformed nested metadata error")
	}
}

func TestTurnMetadataBoundsAndOpaqueIDs(t *testing.T) {
	t.Parallel()
	if _, err := ParseCodexTurnMetadata(nil, strings.Repeat("x", codexTurnMetadataMaxBytes+1), nil); err == nil {
		t.Fatal("expected oversized header error")
	}
	opaque := `{"session_id":"001-session","thread_id":"thread/z","turn_id":"turn-0009","request_kind":"turn"}`
	got, err := ParseCodexTurnMetadata(nil, opaque, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.SessionID != "001-session" || got.Metadata.TurnID != "turn-0009" {
		t.Fatalf("opaque identifiers changed: %#v", got.Metadata)
	}
}
