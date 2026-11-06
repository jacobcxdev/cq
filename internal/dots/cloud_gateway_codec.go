package dots

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const cloudGatewayMaxEnvelopeBytes = 150 * 1024

// cloudGatewayEnvelope identifies only direct RPC envelopes. Chunked and unknown
// relay events remain opaque and cannot establish ownership of a native thread.
type cloudGatewayEnvelope struct {
	Kind     string
	ClientID string
	StreamID string
	Sequence uint64
	Method   string
	ID       json.RawMessage
	ThreadID string
	Fields   cloudMessage
	Message  cloudMessage
}

func decodeCloudGatewayEnvelope(raw []byte) (cloudGatewayEnvelope, bool) {
	fields, ok := cloudGatewayObject(raw)
	if !ok {
		return cloudGatewayEnvelope{}, false
	}
	envelope := cloudGatewayEnvelope{
		Kind:     cloudHarnessString(fields, "type"),
		ClientID: cloudHarnessString(fields, "client_id"),
		StreamID: cloudHarnessString(fields, "stream_id"),
		Fields:   fields,
	}
	if (envelope.Kind != "client_message" && envelope.Kind != "server_message") || !cloudGatewayIdentifier(envelope.ClientID) || !cloudGatewayIdentifier(envelope.StreamID) {
		return cloudGatewayEnvelope{}, false
	}
	if sequence, present := fields["seq_id"]; present {
		if json.Unmarshal(sequence, &envelope.Sequence) != nil || bytes.Equal(sequence, []byte("null")) || envelope.Sequence == 0 {
			return cloudGatewayEnvelope{}, false
		}
	} else if envelope.Kind == "server_message" {
		return cloudGatewayEnvelope{}, false
	}
	message, ok := cloudGatewayObject(fields["message"])
	if !ok {
		return cloudGatewayEnvelope{}, false
	}
	if version, present := message["jsonrpc"]; present && string(version) != `"2.0"` {
		return cloudGatewayEnvelope{}, false
	}
	if id, present := message["id"]; present {
		var text string
		var number int64
		if (json.Unmarshal(id, &text) != nil || !cloudGatewayIdentifier(text)) && json.Unmarshal(id, &number) != nil {
			return cloudGatewayEnvelope{}, false
		}
		if bytes.Equal(id, []byte("null")) {
			return cloudGatewayEnvelope{}, false
		}
		envelope.ID = id
	}
	_, hasMethod := message["method"]
	_, hasResult := message["result"]
	errorRaw, hasError := message["error"]
	if hasMethod {
		envelope.Method = cloudHarnessString(message, "method")
		if !cloudGatewayIdentifier(envelope.Method) || hasResult || hasError {
			return cloudGatewayEnvelope{}, false
		}
		if paramsRaw, present := message["params"]; present && !bytes.Equal(paramsRaw, []byte("null")) {
			params, valid := cloudGatewayObject(paramsRaw)
			if !valid {
				return cloudGatewayEnvelope{}, false
			}
			if _, present := params["threadId"]; present {
				envelope.ThreadID = cloudHarnessString(params, "threadId")
				if !cloudGatewayIdentifier(envelope.ThreadID) {
					return cloudGatewayEnvelope{}, false
				}
			}
		}
	} else {
		if envelope.ID == nil || hasResult == hasError {
			return cloudGatewayEnvelope{}, false
		}
		if hasError {
			failure, valid := cloudGatewayObject(errorRaw)
			var code int64
			var text string
			if !valid || failure["code"] == nil || bytes.Equal(failure["code"], []byte("null")) || json.Unmarshal(failure["code"], &code) != nil || failure["message"] == nil || bytes.Equal(failure["message"], []byte("null")) || json.Unmarshal(failure["message"], &text) != nil {
				return cloudGatewayEnvelope{}, false
			}
		}
	}
	envelope.Message = message
	return envelope, true
}

// cloudGatewayPrivateReply replaces only responses owned by this nonce. A valid
// active Pong consumes the same server sequence without exposing private RPCs;
// replay receives the same replacement and cumulative ACKs keep their meaning.
func cloudGatewayPrivateReply(raw []byte, prefix string) ([]byte, cloudMessage, bool) {
	if !cloudGatewayPrivatePrefix(prefix) {
		return raw, nil, false
	}
	envelope, ok := decodeCloudGatewayEnvelope(raw)
	if !ok || envelope.Kind != "server_message" || envelope.Method != "" {
		return raw, nil, false
	}
	var id string
	if json.Unmarshal(envelope.ID, &id) != nil || !strings.HasPrefix(id, prefix) || len(id) == len(prefix) {
		return raw, nil, false
	}
	fields := envelope.Fields
	delete(fields, "message")
	fields["type"] = json.RawMessage(`"pong"`)
	fields["status"] = json.RawMessage(`"active"`)
	replacement, err := json.Marshal(fields)
	if err != nil {
		return raw, nil, false
	}
	return replacement, envelope.Message, true
}

func cloudGatewayPrivatePrefix(prefix string) bool {
	parts := strings.Split(prefix, "/")
	return len(parts) == 3 && parts[0] == "cq-harness" && cloudGatewayIdentifier(parts[1]) && len(parts[1]) <= 128 && parts[2] == ""
}

func cloudGatewayIdentifier(value string) bool {
	if value == "" || len(value) > 1024 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

// Reject duplicate keys at each interpreted boundary instead of assigning an
// ambiguous frame to a thread based on encoding/json's last-key-wins behaviour.
func cloudGatewayObject(raw []byte) (cloudMessage, bool) {
	if len(raw) == 0 || len(raw) > cloudGatewayMaxEnvelopeBytes || !utf8.Valid(raw) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, false
	}
	result := make(cloudMessage)
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return nil, false
		}
		if _, duplicate := result[name]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		result[name] = value
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, false
	}
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return result, true
}
