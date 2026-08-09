package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const codexSSEDefaultMaxEventBytes = 256 << 10

type CodexSSEEventKind string

const (
	CodexSSEMetadata   CodexSSEEventKind = "metadata"
	CodexSSECreated    CodexSSEEventKind = "created"
	CodexSSEDelta      CodexSSEEventKind = "delta"
	CodexSSEIgnored    CodexSSEEventKind = "ignored"
	CodexSSECompleted  CodexSSEEventKind = "completed"
	CodexSSEError      CodexSSEEventKind = "error"
	CodexSSERateLimits CodexSSEEventKind = "rate_limits"
	CodexSSEDone       CodexSSEEventKind = "done"
	CodexSSEUnknown    CodexSSEEventKind = "unknown"
	CodexSSEMalformed  CodexSSEEventKind = "malformed"
)

type CodexSSEObservation struct {
	Kind              CodexSSEEventKind
	Type              string
	Data              []byte
	Admits            bool
	EndTurn           *bool
	TurnState         string
	ResponseID        string
	HasEncryptedState bool
	Error             CodexWrappedError
	ParseError        error
}

type CodexSSEParser struct {
	maxEvent  int
	buffer    []byte
	dataLines [][]byte
	eventSize int
}

func NewCodexSSEParser(maxEvent int) *CodexSSEParser {
	if maxEvent <= 0 {
		maxEvent = codexSSEDefaultMaxEventBytes
	}
	return &CodexSSEParser{maxEvent: maxEvent}
}

func (p *CodexSSEParser) Feed(chunk []byte) ([]CodexSSEObservation, error) {
	p.buffer = append(p.buffer, chunk...)
	var observations []CodexSSEObservation
	for {
		index := bytes.IndexByte(p.buffer, '\n')
		if index < 0 {
			break
		}
		line := p.buffer[:index]
		p.buffer = p.buffer[index+1:]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		observation, ready, err := p.consumeLine(line)
		if err != nil {
			return nil, err
		}
		if ready {
			observations = append(observations, observation)
		}
	}
	if len(p.buffer)+p.eventSize > p.maxEvent {
		return nil, errors.New("Codex SSE event exceeds limit")
	}
	return observations, nil
}

func (p *CodexSSEParser) Finish() ([]CodexSSEObservation, error) {
	var observations []CodexSSEObservation
	if len(p.buffer) != 0 {
		observation, ready, err := p.consumeLine(bytes.TrimSuffix(p.buffer, []byte{'\r'}))
		if err != nil {
			return nil, err
		}
		if ready {
			observations = append(observations, observation)
		}
		p.buffer = nil
	}
	if len(p.dataLines) != 0 {
		observation, err := p.dispatch()
		if err != nil {
			return nil, err
		}
		observations = append(observations, observation)
	}
	return observations, nil
}

func (p *CodexSSEParser) consumeLine(line []byte) (CodexSSEObservation, bool, error) {
	if len(line) == 0 {
		if len(p.dataLines) == 0 {
			return CodexSSEObservation{}, false, nil
		}
		observation, err := p.dispatch()
		return observation, true, err
	}
	p.eventSize += len(line) + 1
	if p.eventSize > p.maxEvent {
		return CodexSSEObservation{}, false, errors.New("Codex SSE event exceeds limit")
	}
	if line[0] == ':' {
		return CodexSSEObservation{}, false, nil
	}
	field, value, found := bytes.Cut(line, []byte{':'})
	if !found {
		value = nil
	}
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	if string(field) == "data" {
		p.dataLines = append(p.dataLines, append([]byte(nil), value...))
	}
	return CodexSSEObservation{}, false, nil
}

func (p *CodexSSEParser) dispatch() (CodexSSEObservation, error) {
	data := bytes.Join(p.dataLines, []byte{'\n'})
	p.dataLines = nil
	p.eventSize = 0
	return classifyCodexSSEData(data), nil
}

func classifyCodexSSEData(data []byte) CodexSSEObservation {
	observation := CodexSSEObservation{
		Data:              append([]byte(nil), data...),
		HasEncryptedState: jsonContainsKey(data, "encrypted_content"),
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		observation.Kind = CodexSSEDone
		return observation
	}
	var envelope struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
		EndTurn  json.RawMessage `json:"end_turn"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		observation.Kind = CodexSSEMalformed
		observation.ParseError = err
		return observation
	}
	observation.Type = envelope.Type
	switch {
	case envelope.Type == "response.metadata" || envelope.Type == "codex.response.metadata":
		observation.Kind = CodexSSEMetadata
		state, _, err := parseCodexTurnStateObject(data)
		if err != nil {
			observation.Kind = CodexSSEMalformed
			observation.ParseError = err
		} else {
			observation.TurnState = state
		}
	case envelope.Type == "response.created":
		var response map[string]json.RawMessage
		if len(envelope.Response) == 0 || json.Unmarshal(envelope.Response, &response) != nil || response == nil {
			observation.Kind = CodexSSEMalformed
			observation.ParseError = errors.New("response.created requires response object")
			return observation
		}
		observation.Kind = CodexSSECreated
		observation.Admits = true
		if rawID, ok := response["id"]; ok {
			_ = json.Unmarshal(rawID, &observation.ResponseID)
		}
	case strings.HasSuffix(envelope.Type, ".delta"):
		observation.Kind = CodexSSEDelta
	case envelope.Type == "keepalive" ||
		envelope.Type == "response.content_part.added" ||
		envelope.Type == "response.content_part.done" ||
		envelope.Type == "response.custom_tool_call_input.done" ||
		envelope.Type == "response.function_call_arguments.done" ||
		envelope.Type == "response.in_progress" ||
		envelope.Type == "response.output_item.added" ||
		envelope.Type == "response.output_item.done" ||
		envelope.Type == "response.output_text.done" ||
		envelope.Type == "response.reasoning_summary_part.added" ||
		envelope.Type == "response.reasoning_summary_part.done" ||
		envelope.Type == "response.reasoning_summary_text.done" ||
		envelope.Type == "responsesapi.websocket_timing":
		observation.Kind = CodexSSEIgnored
	case envelope.Type == "response.completed":
		var response struct {
			EndTurn json.RawMessage `json:"end_turn"`
		}
		if len(envelope.Response) == 0 || json.Unmarshal(envelope.Response, &response) != nil {
			observation.Kind = CodexSSEMalformed
			observation.ParseError = errors.New("response.completed requires response object")
			return observation
		}
		observation.Kind = CodexSSECompleted
		var responseID struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(envelope.Response, &responseID)
		observation.ResponseID = responseID.ID
		endTurn := response.EndTurn
		if len(endTurn) == 0 {
			endTurn = envelope.EndTurn
		}
		if len(endTurn) != 0 && !bytes.Equal(endTurn, []byte("null")) {
			var value bool
			if err := json.Unmarshal(endTurn, &value); err != nil {
				observation.Kind = CodexSSEMalformed
				observation.ParseError = errors.New("response.completed end_turn must be boolean")
				return observation
			}
			observation.EndTurn = &value
		}
	case envelope.Type == "error":
		observation.Kind = CodexSSEError
		wrapped, err := ParseCodexWrappedError(data)
		if err != nil {
			observation.Kind = CodexSSEMalformed
			observation.ParseError = err
		} else {
			observation.Error = wrapped
		}
	case envelope.Type == "response.failed" || envelope.Type == "response.incomplete":
		observation.Kind = CodexSSEError
	case envelope.Type == "codex.rate_limits":
		observation.Kind = CodexSSERateLimits
	default:
		observation.Kind = CodexSSEUnknown
	}
	return observation
}

func ParseCodexSSE(body []byte, maxEvent int) ([]CodexSSEObservation, error) {
	parser := NewCodexSSEParser(maxEvent)
	observations, err := parser.Feed(body)
	if err != nil {
		return nil, fmt.Errorf("parse Codex SSE: %w", err)
	}
	final, err := parser.Finish()
	if err != nil {
		return nil, fmt.Errorf("finish Codex SSE: %w", err)
	}
	return append(observations, final...), nil
}
