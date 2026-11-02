package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

func TestNormaliseZAIResponsePlaintextCollaborationCalls(t *testing.T) {
	for _, name := range []string{"spawn_agent", "send_message", "followup_task"} {
		for _, shape := range []string{"item", "response", "bare"} {
			for _, marker := range []string{"", `,"encrypted_function_args":null`} {
				t.Run(name+"/"+shape+"/"+marker, func(t *testing.T) {
					call := `{"type":"function_call","namespace":"collaboration","name":"` + name + `","arguments":"{\"message\":\"task text\"}","call_id":"call-1","extra":9007199254740993` + marker + `}`
					var body string
					switch shape {
					case "item":
						body = `{"type":"response.output_item.done","sequence_number":9007199254740995,"item":` + call + `}`
					case "response":
						body = `{"type":"response.completed","response":{"id":"response-1","output":[` + call + `],"extra":9007199254740995}}`
					case "bare":
						body = `{"id":"response-1","output":[` + call + `],"extra":9007199254740995}`
					}
					got, err := normaliseZAIResponse([]byte(body))
					if err != nil {
						t.Fatal(err)
					}
					var envelope map[string]json.RawMessage
					if err := json.Unmarshal(got, &envelope); err != nil {
						t.Fatal(err)
					}
					var item map[string]json.RawMessage
					if shape == "item" {
						if err := json.Unmarshal(envelope["item"], &item); err != nil {
							t.Fatal(err)
						}
					} else {
						response := envelope
						if shape == "response" {
							if err := json.Unmarshal(envelope["response"], &response); err != nil {
								t.Fatal(err)
							}
						}
						var output []map[string]json.RawMessage
						if err := json.Unmarshal(response["output"], &output); err != nil {
							t.Fatal(err)
						}
						if len(output) != 1 {
							t.Fatalf("output count = %d", len(output))
						}
						item = output[0]
					}
					if string(item["encrypted_function_args"]) != "[]" {
						t.Fatalf("marker = %s", item["encrypted_function_args"])
					}
					if string(item["extra"]) != "9007199254740993" || !bytes.Contains(got, []byte("9007199254740995")) {
						t.Fatalf("number precision changed: %s", got)
					}
					var arguments string
					if err := json.Unmarshal(item["arguments"], &arguments); err != nil || arguments != `{"message":"task text"}` {
						t.Fatalf("arguments = %q, %v", arguments, err)
					}
					second, err := normaliseZAIResponse(got)
					if err != nil || !bytes.Equal(second, got) {
						t.Fatalf("second normalisation changed output: %v", err)
					}
				})
			}
		}
	}
}

func TestNormaliseZAIResponsePreservesUnrelatedAndEncryptedCalls(t *testing.T) {
	tests := []string{
		" {\r\n \"type\": \"response.created\", \"response\": {\"id\": \"r\"} } \n",
		`{"item":{"type":"function_call","namespace":"functions","name":"spawn_agent"}}`,
		`{"item":{"type":"function_call","namespace":"collaboration","name":"wait_agent"}}`,
		`{"item":{"type":"message","namespace":"collaboration","name":"spawn_agent"}}`,
		`{"item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","encrypted_function_args":[]}}`,
		`{"item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","encrypted_function_args":["encrypted-task"]}}`,
		`{"item":null,"response":null,"output":null}`,
		`{"output":[{"type":"message","content":[{"type":"output_text","text":"unchanged"}]}]}`,
		`[]`,
		`null`,
	}
	for _, body := range tests {
		got, err := normaliseZAIResponse([]byte(body))
		if err != nil || string(got) != body {
			t.Fatalf("unchanged response modified: got %s, error %v", got, err)
		}
	}
	for _, body := range []string{"", "not-json", `{"item":`} {
		if _, err := normaliseZAIResponse([]byte(body)); err == nil {
			t.Fatalf("invalid JSON accepted: %q", body)
		}
	}
}

func TestNormaliseZAIResponsePreservesCompleteEnvelope(t *testing.T) {
	const body = `{"type":"response.completed","sequence_number":9007199254740997,"response":{"id":"response-1","model":"glm-test","output":[{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"task text","call_id":"call-1"},{"type":"function_call","namespace":"collaboration","name":"send_message","encrypted_function_args":["ciphertext"],"arguments":"encrypted"},{"type":"function_call","namespace":"functions","name":"spawn_agent","arguments":"unrelated"}],"usage":{"input_tokens":9007199254740995}},"unknown":{"value":9007199254740993}}`
	const expected = `{"type":"response.completed","sequence_number":9007199254740997,"response":{"id":"response-1","model":"glm-test","output":[{"type":"function_call","namespace":"collaboration","name":"spawn_agent","encrypted_function_args":[],"arguments":"task text","call_id":"call-1"},{"type":"function_call","namespace":"collaboration","name":"send_message","encrypted_function_args":["ciphertext"],"arguments":"encrypted"},{"type":"function_call","namespace":"functions","name":"spawn_agent","arguments":"unrelated"}],"usage":{"input_tokens":9007199254740995}},"unknown":{"value":9007199254740993}}`
	got, err := normaliseZAIResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var values [2]any
	for i, data := range [][]byte{got, []byte(expected)} {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&values[i]); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(values[0], values[1]) {
		t.Fatalf("response changed unrelated fields: got %s", got)
	}
}

func TestCopyZAIResponseSSEPreservesFramingAndNormalisesMultilineData(t *testing.T) {
	const prefix = ": keepalive\r\nid: event-1\r\nevent: response.output_item.done\r\nretry: 1500\r\n"
	const body = prefix + "data: {\"type\":\"response.output_item.done\",\"item\":\r\n: between data lines\r\ndata: {\"type\":\"function_call\",\"namespace\":\"collaboration\",\"name\":\"spawn_agent\",\"arguments\":\"task text\"}}\r\n\r\n"
	const suffix = ": tail\r\ndata: [DONE]\r\n\r\n"
	for _, chunk := range []int{1, 2, 7, 31, 32768} {
		t.Run(strconv.Itoa(chunk), func(t *testing.T) {
			var output bytes.Buffer
			if err := copyZAIResponseSSE(&output, &zaiOutputChunkReader{reader: strings.NewReader(body + suffix), chunk: chunk}); err != nil {
				t.Fatal(err)
			}
			got := output.String()
			if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) || !strings.Contains(got, "\r\n: between data lines\r\ndata: \r\n\r\n") {
				t.Fatalf("SSE framing changed: %q", got)
			}
			if strings.Count(got, "\r\n") != strings.Count(body+suffix, "\r\n") {
				t.Fatal("line endings changed")
			}
			observations, err := ParseCodexSSE(output.Bytes(), zaiMaxSSEEventBytes)
			if err != nil || len(observations) != 2 || observations[1].Kind != CodexSSEDone {
				t.Fatalf("invalid SSE: %#v, %v", observations, err)
			}
			var event struct {
				Item struct {
					Arguments string            `json:"arguments"`
					Marker    []json.RawMessage `json:"encrypted_function_args"`
				} `json:"item"`
			}
			if err := json.Unmarshal(observations[0].Data, &event); err != nil || event.Item.Marker == nil || len(event.Item.Marker) != 0 || event.Item.Arguments != "task text" {
				t.Fatalf("normalised event = %s, error %v", observations[0].Data, err)
			}
		})
	}
}

func TestCopyZAIResponseSSEPreservesUnchangedAndFailedStreams(t *testing.T) {
	tests := []string{
		"upstream-response",
		": comment\n\nevent: ping\ndata: not-json\n\n",
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n",
		"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"error\":{\"message\":\"failure\"}}}\n\n",
		"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"r\"}}\r\n\r\n",
		"data: {\"type\":\"error\",\"error\":{\"message\":\"failure\"}}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}",
		"data: [DONE]\n\n",
		"data\ndata: {\"type\":\"response.created\"}\n\n",
	}
	for _, body := range tests {
		var output bytes.Buffer
		if err := copyZAIResponseSSE(&output, iotest.OneByteReader(strings.NewReader(body))); err != nil {
			t.Fatal(err)
		}
		if output.String() != body {
			t.Fatalf("stream changed: got %q, want %q", output.String(), body)
		}
	}
}

func TestCopyZAIResponseSSEDoesNotCompleteFailedOrTruncatedStreams(t *testing.T) {
	const call = `data: {"type":"response.output_item.done","item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"task"}}`
	for _, terminal := range []string{"response.failed", "response.incomplete", "error", ""} {
		t.Run(terminal, func(t *testing.T) {
			body, suffix := call, ""
			if terminal != "" {
				suffix = "data: {\"type\":\"" + terminal + "\",\"error\":{\"message\":\"failure\"}}\n\n"
				body += "\n\n" + suffix
			}
			var output bytes.Buffer
			if err := copyZAIResponseSSE(&output, iotest.OneByteReader(strings.NewReader(body))); err != nil {
				t.Fatal(err)
			}
			got := output.String()
			if !strings.Contains(got, `"encrypted_function_args":[]`) || strings.Contains(got, "response.completed") || !strings.HasSuffix(got, suffix) {
				t.Fatalf("incorrect stream lifecycle: %q", got)
			}
			if terminal == "" && strings.HasSuffix(got, "\n") {
				t.Fatal("truncated event gained framing")
			}
		})
	}
}

func TestCopyZAIResponseSSEBoundedAndPropagatesIOErrors(t *testing.T) {
	for _, body := range []string{
		strings.Repeat("x", zaiMaxSSEEventBytes+1),
		strings.Repeat("data: x\n", zaiMaxSSEEventBytes/8+1) + "\n",
	} {
		var output bytes.Buffer
		if err := copyZAIResponseSSE(&output, strings.NewReader(body)); err == nil || output.Len() != 0 {
			t.Fatalf("oversized event accepted: output %d, error %v", output.Len(), err)
		}
	}
	const body = ": comment\n\n"
	wantErr := errors.New("reader failure")
	var output bytes.Buffer
	if err := copyZAIResponseSSE(&output, io.MultiReader(strings.NewReader(body), iotest.ErrReader(wantErr))); !errors.Is(err, wantErr) || output.String() != body {
		t.Fatalf("reader error = %v, output %q", err, output.String())
	}
	if err := copyZAIResponseSSE(zaiOutputErrorWriter{}, strings.NewReader(body)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer error = %v", err)
	}
	if err := copyZAIResponseSSE(zaiOutputShortWriter{}, strings.NewReader(body)); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v", err)
	}
}

func TestCopyZAIResponseSSEEventLimitIncludesNormalisedOutput(t *testing.T) {
	body := strings.Repeat("x", zaiMaxSSEEventBytes-2) + "\n\n"
	var output bytes.Buffer
	if err := copyZAIResponseSSE(&output, strings.NewReader(body)); err != nil || output.String() != body {
		t.Fatalf("exact event limit rejected: %v", err)
	}
	const base = "data: {\"item\":{\"type\":\"function_call\",\"namespace\":\"collaboration\",\"name\":\"spawn_agent\"},\"padding\":\"\"}\n\n"
	body = strings.Replace(base, `"padding":""`, `"padding":"`+strings.Repeat("x", zaiMaxSSEEventBytes-len(base))+`"`, 1)
	if len(body) != zaiMaxSSEEventBytes {
		t.Fatalf("test event length = %d", len(body))
	}
	output.Reset()
	if err := copyZAIResponseSSE(&output, strings.NewReader(body)); err == nil || output.Len() != 0 {
		t.Fatalf("expanded event exceeded output limit: bytes=%d, error=%v", output.Len(), err)
	}
}

type zaiOutputChunkReader struct {
	reader io.Reader
	chunk  int
}

func (r *zaiOutputChunkReader) Read(p []byte) (int, error) {
	return r.reader.Read(p[:min(len(p), r.chunk)])
}

type zaiOutputErrorWriter struct{}

func (zaiOutputErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type zaiOutputShortWriter struct{}

func (zaiOutputShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
