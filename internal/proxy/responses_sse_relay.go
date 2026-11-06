package proxy

import (
	"encoding/json"
	"errors"
	"io"
)

// relayResponsesSSE is shared by native subscription adapters. Completion is
// required before a continuation is retained; a disconnected stream is never
// replayed by this layer.
func relayResponsesSSE(reader io.Reader, name string, normalize func([]byte) ([]byte, error), emit func(string, []byte) error) (string, []json.RawMessage, error) {
	parser := NewCodexSSEParser(1 << 20)
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			events, err := parser.Feed(buffer[:n])
			if err != nil {
				return "", nil, errors.New("invalid " + name + " SSE stream")
			}
			for _, event := range events {
				if event.Kind == CodexSSEDone {
					continue
				}
				if !json.Valid(event.Data) {
					return "", nil, errors.New("invalid " + name + " SSE event")
				}
				data, err := normalize(event.Data)
				if err != nil {
					return "", nil, errors.New("invalid " + name + " SSE event")
				}
				if len(data) == 0 {
					continue
				}
				var normalized struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(data, &normalized)
				kind := event.Type
				if normalized.Type != "" {
					kind = normalized.Type
				}
				if err := emit(kind, data); err != nil {
					return "", nil, err
				}
				if event.Type == "response.completed" {
					var completed struct {
						Response struct {
							ID     string            `json:"id"`
							Output []json.RawMessage `json:"output"`
						} `json:"response"`
					}
					if json.Unmarshal(data, &completed) != nil || completed.Response.ID == "" {
						return "", nil, errors.New("invalid " + name + " completed response")
					}
					return completed.Response.ID, completed.Response.Output, nil
				}
				switch event.Type {
				case "response.failed", "response.incomplete", "error":
					return "", nil, errors.New(name + " response failed")
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return "", nil, errors.New(name + " stream ended before response completion")
			}
			return "", nil, errors.New(name + " HTTP stream read failed")
		}
	}
}
