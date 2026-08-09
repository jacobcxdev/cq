package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const codexSSEDefaultMaxEventBytes = 256 << 10

var (
	ErrCodexSSETruncated   = fmt.Errorf("Codex SSE stream ended before blank-line termination: %w", io.ErrUnexpectedEOF)
	errCodexSSEEventTooBig = errors.New("Codex SSE event exceeds limit")
)

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
	maxEvent    int
	buffer      []byte
	data        []byte
	hasData     bool
	eventSize   int
	terminalErr error
}

func NewCodexSSEParser(maxEvent int) *CodexSSEParser {
	if maxEvent <= 0 {
		maxEvent = codexSSEDefaultMaxEventBytes
	}
	return &CodexSSEParser{maxEvent: maxEvent}
}

func (p *CodexSSEParser) Feed(chunk []byte) ([]CodexSSEObservation, error) {
	if p.terminalErr != nil {
		return nil, p.terminalErr
	}
	var observations []CodexSSEObservation
	for len(chunk) != 0 {
		index := bytes.IndexByte(chunk, '\n')
		if index < 0 {
			if err := p.appendBuffer(chunk); err != nil {
				return observations, p.fail(err)
			}
			break
		}

		fragment := chunk[:index]
		bufferLen := len(p.buffer)
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\r' {
			fragment = fragment[:len(fragment)-1]
		} else if len(fragment) == 0 && bufferLen > 0 && p.buffer[bufferLen-1] == '\r' {
			bufferLen--
		}
		lineLen := bufferLen + len(fragment)
		additional := 0
		if lineLen != 0 {
			additional = lineLen + 1
		}
		if additional > p.maxEvent-p.eventSize {
			return observations, p.fail(errCodexSSEEventTooBig)
		}

		var line []byte
		if len(p.buffer) == 0 {
			line = fragment
		} else {
			p.buffer = p.buffer[:bufferLen]
			if err := p.appendBuffer(fragment); err != nil {
				return observations, p.fail(err)
			}
			line = p.buffer
			p.buffer = nil
		}
		observation, ready, err := p.consumeLine(line)
		if err != nil {
			return observations, p.fail(err)
		}
		if ready {
			observations = append(observations, observation)
		}
		chunk = chunk[index+1:]
	}
	return observations, nil
}

func (p *CodexSSEParser) appendBuffer(fragment []byte) error {
	needed := len(p.buffer) + len(fragment)
	if needed > p.maxEvent-p.eventSize {
		return errCodexSSEEventTooBig
	}
	if needed > cap(p.buffer) {
		if cap(p.data)+needed > p.maxEvent {
			data := make([]byte, len(p.data))
			copy(data, p.data)
			p.data = data
		}
		available := p.maxEvent - cap(p.data)
		if needed > available {
			return errCodexSSEEventTooBig
		}
		capacity := cap(p.buffer) * 2
		if capacity < 64 {
			capacity = 64
		}
		if capacity < needed {
			capacity = needed
		}
		if capacity > available {
			capacity = available
		}
		buffer := make([]byte, len(p.buffer), capacity)
		copy(buffer, p.buffer)
		p.buffer = buffer
	}
	p.buffer = append(p.buffer, fragment...)
	return nil
}

func (p *CodexSSEParser) Finish() ([]CodexSSEObservation, error) {
	if p.terminalErr != nil {
		return nil, p.terminalErr
	}
	if len(p.buffer) != 0 || p.hasData || p.eventSize != 0 {
		return nil, p.fail(ErrCodexSSETruncated)
	}
	return nil, nil
}

func (p *CodexSSEParser) fail(err error) error {
	if p.terminalErr != nil {
		return p.terminalErr
	}
	p.buffer = nil
	p.data = nil
	p.hasData = false
	p.eventSize = 0
	p.terminalErr = err
	return err
}

func (p *CodexSSEParser) consumeLine(line []byte) (CodexSSEObservation, bool, error) {
	if len(line) == 0 {
		if !p.hasData {
			p.eventSize = 0
			return CodexSSEObservation{}, false, nil
		}
		observation, err := p.dispatch()
		return observation, true, err
	}
	p.eventSize += len(line) + 1
	if p.eventSize > p.maxEvent {
		return CodexSSEObservation{}, false, errCodexSSEEventTooBig
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
		if err := p.appendDataLine(value); err != nil {
			return CodexSSEObservation{}, false, err
		}
	}
	return CodexSSEObservation{}, false, nil
}

func (p *CodexSSEParser) appendDataLine(value []byte) error {
	needed := len(p.data) + len(value)
	if p.hasData {
		needed++
	}
	if needed > p.maxEvent {
		return errCodexSSEEventTooBig
	}
	if needed > cap(p.data) {
		capacity := cap(p.data) * 2
		if capacity < 64 {
			capacity = 64
		}
		if capacity < needed {
			capacity = needed
		}
		if capacity > p.maxEvent {
			capacity = p.maxEvent
		}
		data := make([]byte, len(p.data), capacity)
		copy(data, p.data)
		p.data = data
	}
	if p.hasData {
		p.data = append(p.data, '\n')
	}
	p.data = append(p.data, value...)
	p.hasData = true
	return nil
}

func (p *CodexSSEParser) dispatch() (CodexSSEObservation, error) {
	data := p.data
	p.data = nil
	p.hasData = false
	p.eventSize = 0
	return classifyCodexSSEData(data), nil
}

func classifyCodexSSEData(data []byte) CodexSSEObservation {
	observation := CodexSSEObservation{
		Data: append([]byte(nil), data...),
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		observation.Kind = CodexSSEDone
		return observation
	}
	if err := validateCodexLifecycleAuthority(data); err != nil {
		observation.Kind = CodexSSEMalformed
		observation.ParseError = err
		return observation
	}
	observation.HasEncryptedState = jsonContainsKey(data, "encrypted_content")
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
		if rawID, ok := codexJSONRawField(response, "id"); ok {
			if bytes.Equal(bytes.TrimSpace(rawID), []byte("null")) || json.Unmarshal(rawID, &observation.ResponseID) != nil {
				observation.Kind = CodexSSEMalformed
				observation.Admits = false
				observation.ParseError = errors.New("response.created id must be a string")
				return observation
			}
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
		var response map[string]json.RawMessage
		if len(envelope.Response) == 0 || json.Unmarshal(envelope.Response, &response) != nil || response == nil {
			observation.Kind = CodexSSEMalformed
			observation.ParseError = errors.New("response.completed requires response object")
			return observation
		}
		observation.Kind = CodexSSECompleted
		if rawID, ok := codexJSONRawField(response, "id"); ok {
			if bytes.Equal(bytes.TrimSpace(rawID), []byte("null")) || json.Unmarshal(rawID, &observation.ResponseID) != nil {
				observation.Kind = CodexSSEMalformed
				observation.ParseError = errors.New("response.completed id must be a string")
				return observation
			}
		}
		endTurn, hasEndTurn := codexJSONRawField(response, "end_turn")
		if hasEndTurn && len(envelope.EndTurn) != 0 {
			observation.Kind = CodexSSEMalformed
			observation.ParseError = errors.New("response.completed has conflicting end_turn authority")
			return observation
		}
		if !hasEndTurn {
			endTurn = envelope.EndTurn
		}
		if len(endTurn) != 0 {
			var value bool
			if bytes.Equal(bytes.TrimSpace(endTurn), []byte("null")) || json.Unmarshal(endTurn, &value) != nil {
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
