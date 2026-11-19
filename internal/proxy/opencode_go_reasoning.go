package proxy

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func (s *Server) sharedBudgetStreamNormaliser(selected sharedBudgetSelection, body []byte, emit func(string, []byte) error) func([]byte) ([]byte, error) {
	var normalise func([]byte) ([]byte, error)
	if selected.route.PlaintextCollaboration {
		normalise = plaintextCollaborationResponse
	}
	if selected.id != modelregistry.ProviderOpenCodeGo || !selected.route.OmitStreamOptions {
		return normalise
	}
	var request struct {
		StreamOptions struct {
			Delivery string `json:"reasoning_summary_delivery"`
		} `json:"stream_options"`
		Reasoning struct{ Summary string }
	}
	if json.Unmarshal(body, &request) != nil || request.StreamOptions.Delivery != "sequential_cutoff" || request.Reasoning.Summary == "none" {
		return normalise
	}
	for _, entry := range s.Catalog.SharedBudgetModels(selected.model) {
		if entry.Provider == selected.id && entry.SharedBudget != nil && entry.SharedBudget.NativeAPI == "chat-completions" {
			stream := openCodeGoReasoningStream{emit: emit, normalise: normalise}
			return stream.event
		}
	}
	return normalise
}

// OpenCode Go sends the entire reasoning block as one summary part and can
// close it after the next output item starts. Sequential-cutoff Codex clients
// display only completed summary sections for the currently active item.
// Split the live presentation into sections, keeping the original completed
// item and response intact for replay.
type openCodeGoReasoningStream struct {
	emit           func(string, []byte) error
	normalise      func([]byte) ([]byte, error)
	itemID         string
	pending        string
	outputIndex    int
	nextSummary    int
	sequenceOffset int64
	closed         bool
	receivedDeltas bool
}

type openCodeGoReasoningEvent struct {
	Type         string
	ItemID       string `json:"item_id"`
	Sequence     *int64 `json:"sequence_number"`
	OutputIndex  *int   `json:"output_index"`
	SummaryIndex *int   `json:"summary_index"`
	Delta        string
	Text         string
	Item         struct{ ID, Type string }
}

func (s *openCodeGoReasoningStream) event(data []byte) ([]byte, error) {
	var event openCodeGoReasoningEvent
	if json.Unmarshal(data, &event) != nil {
		return s.forward(data, nil)
	}

	switch event.Type {
	case "response.output_item.added", "response.output_item.done":
		added := event.Type == "response.output_item.added"
		if s.itemID != "" && !s.closed && (added && event.Item.ID != s.itemID || !added && event.Item.ID == s.itemID) {
			if err := s.finishSection(len(s.pending), event.Sequence); err != nil {
				return nil, err
			}
			s.closed = true
		}
		if added && event.Item.Type == "reasoning" && event.Item.ID != "" && event.OutputIndex != nil {
			s.itemID, s.outputIndex = event.Item.ID, *event.OutputIndex
			s.nextSummary, s.closed, s.receivedDeltas = 0, false, false
		}
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		if s.itemID != "" && event.ItemID == s.itemID && event.SummaryIndex != nil && *event.SummaryIndex == 0 {
			if !s.closed {
				switch event.Type {
				case "response.reasoning_summary_text.delta":
					s.receivedDeltas = true
					s.pending += event.Delta
					for length := reasoningSectionLength(s.pending); length > 0; length = reasoningSectionLength(s.pending) {
						if err := s.finishSection(length, event.Sequence); err != nil {
							return nil, err
						}
					}
				case "response.reasoning_summary_text.done":
					if !s.receivedDeltas {
						s.pending = event.Text
					}
					if err := s.finishSection(len(s.pending), event.Sequence); err != nil {
						return nil, err
					}
					s.closed = true
				}
			}
			if event.Sequence != nil {
				s.sequenceOffset--
			}
			return nil, nil
		}
	}
	return s.forward(data, event.Sequence)
}

func (s *openCodeGoReasoningStream) forward(data []byte, sequence *int64) ([]byte, error) {
	if s.normalise != nil {
		var err error
		data, err = s.normalise(data)
		if err != nil {
			return nil, err
		}
	}
	if sequence == nil || s.sequenceOffset == 0 {
		return data, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	fields["sequence_number"], _ = json.Marshal(*sequence + s.sequenceOffset)
	return json.Marshal(fields)
}

func (s *openCodeGoReasoningStream) finishSection(length int, sequence *int64) error {
	if length == 0 {
		return nil
	}
	text := s.pending[:length]
	s.pending = strings.Clone(s.pending[length:])
	fields := map[string]any{"item_id": s.itemID, "output_index": s.outputIndex, "summary_index": s.nextSummary}
	s.nextSummary++
	for _, event := range []struct {
		kind  string
		field string
		value any
	}{
		{"part.added", "part", map[string]string{"type": "summary_text", "text": ""}},
		{"text.delta", "delta", text},
		{"text.done", "text", text},
		{"part.done", "part", map[string]string{"type": "summary_text", "text": text}},
	} {
		kind := "response.reasoning_summary_" + event.kind
		fields["type"] = kind
		fields[event.field] = event.value
		if sequence != nil {
			fields["sequence_number"] = *sequence + s.sequenceOffset
			s.sequenceOffset++
		}
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		if err := s.emit(kind, data); err != nil {
			return err
		}
		delete(fields, event.field)
	}
	return nil
}

func reasoningSectionLength(text string) int {
	const limit = 384
	var lastSpace int
	var sentenceEnd bool
	for offset, r := range text {
		if offset >= limit {
			if lastSpace != 0 {
				return lastSpace
			}
			return offset
		}
		if r == '\n' || unicode.IsSpace(r) && sentenceEnd || strings.ContainsRune("。！？", r) {
			return offset + utf8.RuneLen(r)
		}
		if unicode.IsSpace(r) {
			lastSpace = offset + utf8.RuneLen(r)
		}
		sentenceEnd = r == '.' || r == '?' || r == '!'
	}
	return 0
}
