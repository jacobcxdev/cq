package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	codexProtocolMaxBytes = 8 << 20
)

type CodexProtocolRequest struct {
	Type                          string
	Model                         string
	PreviousResponseID            string
	RequestedReasoningEffort      string
	HasRequestedReasoningEffort   bool
	RequestedReasoningEffortValid bool
	Metadata                      CodexTurnMetadataResult
	TurnState                     string
	HasTurnState                  bool
	HasEncryptedState             bool
}

func ParseCodexProtocolRequest(body []byte, directMetadata string, handshake *CodexTurnMetadata) (CodexProtocolRequest, error) {
	return parseCodexProtocolRequest(body, directMetadata, handshake, codexHTTPRequestMaxBytes)
}

func parseCodexProtocolRequest(body []byte, directMetadata string, handshake *CodexTurnMetadata, maxBytes int) (CodexProtocolRequest, error) {
	if codexLimitExceeded(len(body), maxBytes) {
		return CodexProtocolRequest{}, errors.New("Codex protocol request exceeds limit")
	}
	nestedLimit := maxBytes
	if nestedLimit == 0 {
		nestedLimit = len(body)
	}
	metadata, err := parseCodexTurnMetadataWithNestedLimit(body, directMetadata, handshake, nestedLimit)
	if err != nil {
		return CodexProtocolRequest{}, err
	}
	var envelope struct {
		Type               string          `json:"type"`
		Model              string          `json:"model"`
		PreviousResponseID string          `json:"previous_response_id"`
		Reasoning          json.RawMessage `json:"reasoning"`
		Params             json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return CodexProtocolRequest{}, fmt.Errorf("decode Codex protocol request: %w", err)
	}
	if len(envelope.Params) != 0 && !bytes.Equal(envelope.Params, []byte("null")) {
		var params struct {
			Model              string          `json:"model"`
			PreviousResponseID string          `json:"previous_response_id"`
			Reasoning          json.RawMessage `json:"reasoning"`
		}
		if err := json.Unmarshal(envelope.Params, &params); err != nil {
			return CodexProtocolRequest{}, fmt.Errorf("decode Codex protocol params: %w", err)
		}
		if envelope.Model == "" {
			envelope.Model = params.Model
		}
		if envelope.PreviousResponseID == "" {
			envelope.PreviousResponseID = params.PreviousResponseID
		}
		if len(envelope.Reasoning) == 0 || bytes.Equal(envelope.Reasoning, []byte("null")) {
			envelope.Reasoning = params.Reasoning
		}
	}
	reasoningEffort, hasReasoningEffort, reasoningEffortValid := parseCodexRequestedReasoningEffort(envelope.Reasoning)
	return CodexProtocolRequest{
		Type:                          envelope.Type,
		Model:                         envelope.Model,
		PreviousResponseID:            envelope.PreviousResponseID,
		RequestedReasoningEffort:      reasoningEffort,
		HasRequestedReasoningEffort:   hasReasoningEffort,
		RequestedReasoningEffortValid: reasoningEffortValid,
		Metadata:                      metadata,
		HasEncryptedState:             jsonContainsKey(body, "encrypted_content"),
	}, nil
}

func parseCodexRequestedReasoningEffort(raw json.RawMessage) (effort string, found, valid bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false, true
	}
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reasoning); err != nil {
		return "", true, false
	}
	effortRaw, found := reasoning["effort"]
	if !found || bytes.Equal(bytes.TrimSpace(effortRaw), []byte("null")) {
		return "", false, true
	}
	if err := json.Unmarshal(effortRaw, &effort); err != nil {
		return "", true, false
	}
	return effort, true, true
}

type CodexWrappedError struct {
	Found          bool
	Status         int
	ErrorType      string
	Code           string
	Message        string
	AuthFailure    bool
	HardUsageLimit bool
}

func ParseCodexWrappedError(payload []byte) (CodexWrappedError, error) {
	return parseCodexError(payload, 0, false)
}

func parseCodexHTTPError(payload []byte, transportStatus int) (CodexWrappedError, error) {
	return parseCodexError(payload, transportStatus, true)
}

func parseCodexError(payload []byte, transportStatus int, allowTransportStatus bool) (CodexWrappedError, error) {
	if len(payload) > codexProtocolMaxBytes {
		return CodexWrappedError{}, errors.New("Codex error event exceeds limit")
	}
	if err := validateCodexErrorAuthority(payload); err != nil {
		return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		return CodexWrappedError{}, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
	}
	typeValue, typePresent, typeValid, err := parseCodexErrorString(envelope["type"])
	if err != nil {
		return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
	}
	if typePresent && (!typeValid || typeValue != "error") {
		return CodexWrappedError{}, nil
	}
	if !allowTransportStatus && (!typePresent || typeValue != "error") {
		return CodexWrappedError{}, nil
	}
	statusField, err := parseCodexErrorStatus(envelope["status"])
	if err != nil {
		return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
	}
	statusCodeField, err := parseCodexErrorStatus(envelope["status_code"])
	if err != nil {
		return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
	}
	status, validStatus := resolveCodexErrorStatus(statusField, statusCodeField, transportStatus, allowTransportStatus)
	if !validStatus {
		status = 0
	}
	var nested map[string]json.RawMessage
	errorPayload := envelope["error"]
	hasError := len(errorPayload) != 0 && !bytes.Equal(bytes.TrimSpace(errorPayload), []byte("null"))
	if hasError {
		if err := json.Unmarshal(errorPayload, &nested); err != nil {
			return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
		}
	}
	errorType, _, errorTypeValid, err := parseCodexErrorString(nested["type"])
	if err != nil {
		return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
	}
	code, _, _, err := parseCodexErrorString(nested["code"])
	if err != nil {
		return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
	}
	message, _, _, err := parseCodexErrorString(nested["message"])
	if err != nil {
		return CodexWrappedError{}, fmt.Errorf("decode Codex error event: %w", err)
	}
	found := typePresent && typeValue == "error"
	if allowTransportStatus {
		found = hasError
	}
	return CodexWrappedError{
		Found:          found,
		Status:         status,
		ErrorType:      errorType,
		Code:           code,
		Message:        message,
		AuthFailure:    validStatus && (status == http.StatusUnauthorized || status == http.StatusForbidden),
		HardUsageLimit: validStatus && status == http.StatusTooManyRequests && errorTypeValid && errorType == "usage_limit_reached",
	}, nil
}

type codexErrorStatus struct {
	value          int
	present, valid bool
}

func resolveCodexErrorStatus(statusField, statusCodeField codexErrorStatus, transportStatus int, allowTransportStatus bool) (int, bool) {
	status := 0
	for _, field := range []codexErrorStatus{statusField, statusCodeField} {
		if !field.present {
			continue
		}
		if !field.valid || field.value <= 0 {
			return 0, false
		}
		if status != 0 && status != field.value {
			return 0, false
		}
		status = field.value
	}
	if allowTransportStatus {
		if transportStatus <= 0 {
			return 0, false
		}
		if status != 0 && status != transportStatus {
			return 0, false
		}
		return transportStatus, true
	}
	return status, true
}

func validateCodexErrorAuthority(payload []byte) error {
	return validateCodexJSONAuthority(payload, "Codex error", isCodexErrorAuthorityField)
}

type codexJSONAuthorityPredicate func(path []string, name string) bool

func validateCodexJSONAuthority(payload []byte, subject string, isAuthority codexJSONAuthorityPredicate) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := scanCodexJSONAuthority(decoder, nil, subject, isAuthority); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s has trailing data", subject)
		}
		return err
	}
	return nil
}

func scanCodexJSONAuthority(decoder *json.Decoder, path []string, subject string, isAuthority codexJSONAuthorityPredicate) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return fmt.Errorf("%s field name must be a string", subject)
			}
			if isAuthority(path, name) && !codexJSONASCIIName(name) {
				return fmt.Errorf("non-ASCII %s authority field %s", subject, name)
			}
			canonicalName := strings.ToLower(name)
			if isAuthority(path, name) {
				if _, duplicate := seen[canonicalName]; duplicate {
					return fmt.Errorf("duplicate %s authority field %s", subject, name)
				}
				seen[canonicalName] = struct{}{}
			}
			if err := scanCodexJSONAuthority(decoder, append(path, canonicalName), subject, isAuthority); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("%s object was not closed", subject)
		}
	case '[':
		for decoder.More() {
			if err := scanCodexJSONAuthority(decoder, path, subject, isAuthority); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("%s array was not closed", subject)
		}
	default:
		return fmt.Errorf("unexpected %s delimiter", subject)
	}
	return nil
}

func isCodexErrorAuthorityField(path []string, name string) bool {
	if len(path) == 0 {
		return codexJSONNameEqual(name, "type") || codexJSONNameEqual(name, "status") || codexJSONNameEqual(name, "status_code") || codexJSONNameEqual(name, "error")
	}
	return len(path) == 1 && codexJSONNameEqual(path[0], "error") && (codexJSONNameEqual(name, "type") || codexJSONNameEqual(name, "code"))
}

func codexJSONNameEqual(name, authority string) bool {
	return strings.EqualFold(name, authority)
}

func codexJSONASCIIName(name string) bool {
	for index := 0; index < len(name); index++ {
		if name[index] >= 0x80 {
			return false
		}
	}
	return true
}

func parseCodexErrorString(raw json.RawMessage) (string, bool, bool, error) {
	if len(raw) == 0 {
		return "", false, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true, false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, false, err
	}
	return value, true, true, nil
}

func parseCodexErrorStatus(raw json.RawMessage) (codexErrorStatus, error) {
	if len(raw) == 0 {
		return codexErrorStatus{}, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return codexErrorStatus{present: true}, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return codexErrorStatus{present: true}, err
	}
	return codexErrorStatus{value: value, present: true, valid: true}, nil
}

type CodexCompactObservation struct {
	ResponseID        string
	HasEncryptedState bool
}

func ParseCodexCompactResponse(body []byte) (CodexCompactObservation, error) {
	if len(body) > codexProtocolMaxBytes {
		return CodexCompactObservation{}, errors.New("Codex compact response exceeds limit")
	}
	if err := validateCodexJSONAuthority(body, "Codex compact response", isCodexCompactAuthorityField); err != nil {
		return CodexCompactObservation{}, fmt.Errorf("decode Codex compact response: %w", err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		return CodexCompactObservation{}, fmt.Errorf("decode Codex compact response: %w", err)
	}
	if value == nil {
		return CodexCompactObservation{}, errors.New("Codex compact response must be an object")
	}
	if errorValue, ok := codexJSONRawField(value, "error"); ok && !bytes.Equal(bytes.TrimSpace(errorValue), []byte("null")) {
		return CodexCompactObservation{}, errors.New("Codex compact response contains an error")
	}
	output, ok := codexJSONRawField(value, "output")
	if !ok || len(bytes.TrimSpace(output)) == 0 || bytes.TrimSpace(output)[0] != '[' {
		return CodexCompactObservation{}, errors.New("Codex compact response output must be an array")
	}
	var outputItems []json.RawMessage
	if err := json.Unmarshal(output, &outputItems); err != nil {
		return CodexCompactObservation{}, errors.New("Codex compact response output must be an array")
	}
	for _, item := range outputItems {
		if err := validateCodexCompactResponseItem(item); err != nil {
			return CodexCompactObservation{}, err
		}
	}
	var responseID string
	if raw, ok := codexJSONRawField(value, "id"); ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &responseID) != nil {
			return CodexCompactObservation{}, errors.New("Codex compact response id must be a string")
		}
	}
	return CodexCompactObservation{ResponseID: responseID, HasEncryptedState: jsonContainsKey(body, "encrypted_content")}, nil
}

func validateCodexCompactResponseItem(raw json.RawMessage) error {
	item, itemType, err := decodeCodexCompactTaggedObject(raw, "response item")
	if err != nil {
		return fmt.Errorf("Codex compact response output: %w", err)
	}
	fields, known := codexCompactResponseItemFields(itemType)
	if !known {
		// The pinned client preserves unknown string tags through serde(other).
		return nil
	}
	if err := validateCodexCompactKnownFields(raw, "Codex compact response "+itemType, fields...); err != nil {
		return err
	}
	requireString := func(field string) error {
		return requireCodexCompactString(item, field, itemType)
	}
	requireArray := func(field string) ([]json.RawMessage, error) {
		return requireCodexCompactArray(item, field, itemType)
	}
	switch itemType {
	case "additional_tools":
		if err := requireString("role"); err != nil {
			return err
		}
		_, err = requireArray("tools")
	case "message":
		if err := requireString("role"); err != nil {
			return err
		}
		var content []json.RawMessage
		if content, err = requireArray("content"); err == nil {
			err = validateCodexCompactTaggedItems(content, itemType+" content", validateCodexCompactMessageContentItem)
		}
	case "agent_message":
		if err := requireString("author"); err != nil {
			return err
		}
		if err := requireString("recipient"); err != nil {
			return err
		}
		var content []json.RawMessage
		if content, err = requireArray("content"); err == nil {
			err = validateCodexCompactTaggedItems(content, itemType+" content", validateCodexCompactAgentContentItem)
		}
	case "reasoning":
		var summary []json.RawMessage
		if summary, err = requireArray("summary"); err == nil {
			err = validateCodexCompactTaggedItems(summary, itemType+" summary", validateCodexCompactReasoningSummaryItem)
		}
	case "local_shell_call":
		var status string
		if status, err = requireCodexCompactStringValue(item, "status", itemType); err == nil && status != "completed" && status != "in_progress" && status != "incomplete" {
			err = fmt.Errorf("Codex compact response %s status is invalid", itemType)
		}
		if err == nil {
			err = validateCodexCompactLocalShellAction(item["action"])
		}
	case "function_call":
		for _, field := range []string{"name", "arguments", "call_id"} {
			if err := requireString(field); err != nil {
				return err
			}
		}
	case "tool_search_call":
		if err := requireString("execution"); err != nil {
			return err
		}
		if _, ok := item["arguments"]; !ok {
			err = fmt.Errorf("Codex compact response %s arguments is required", itemType)
		}
	case "function_call_output", "custom_tool_call_output":
		if err := requireString("call_id"); err != nil {
			return err
		}
		err = validateCodexCompactFunctionOutput(item["output"], itemType)
	case "custom_tool_call":
		for _, field := range []string{"call_id", "name", "input"} {
			if err := requireString(field); err != nil {
				return err
			}
		}
	case "tool_search_output":
		for _, field := range []string{"status", "execution"} {
			if err := requireString(field); err != nil {
				return err
			}
		}
		_, err = requireArray("tools")
	case "image_generation_call":
		for _, field := range []string{"status", "result"} {
			if err := requireString(field); err != nil {
				return err
			}
		}
	case "compaction", "compaction_summary":
		err = requireString("encrypted_content")
	case "web_search_call", "compaction_trigger", "context_compaction":
		// These pinned variants have no required fields beyond the discriminator.
	}
	if err != nil {
		return err
	}
	return validateCodexCompactOptionalResponseItemFields(item, itemType)
}

func codexCompactResponseItemFields(itemType string) ([]string, bool) {
	common := []string{"type", "id"}
	if itemType != "additional_tools" {
		common = append(common, "internal_chat_message_metadata_passthrough")
	}
	var fields []string
	switch itemType {
	case "additional_tools":
		fields = []string{"role", "tools"}
	case "message":
		fields = []string{"role", "content", "phase"}
	case "agent_message":
		fields = []string{"author", "recipient", "content"}
	case "reasoning":
		fields = []string{"summary", "content", "encrypted_content"}
	case "local_shell_call":
		fields = []string{"call_id", "status", "action"}
	case "function_call":
		fields = []string{"name", "namespace", "arguments", "call_id"}
	case "tool_search_call":
		fields = []string{"call_id", "status", "execution", "arguments"}
	case "function_call_output":
		fields = []string{"call_id", "output"}
	case "custom_tool_call":
		fields = []string{"status", "call_id", "name", "namespace", "input"}
	case "custom_tool_call_output":
		fields = []string{"call_id", "name", "output"}
	case "tool_search_output":
		fields = []string{"call_id", "status", "execution", "tools"}
	case "web_search_call":
		fields = []string{"status", "action"}
	case "image_generation_call":
		fields = []string{"status", "revised_prompt", "result"}
	case "compaction", "compaction_summary":
		fields = []string{"encrypted_content"}
	case "context_compaction":
		fields = []string{"encrypted_content"}
	case "compaction_trigger":
		return []string{"type"}, true
	default:
		return nil, false
	}
	return append(common, fields...), true
}

func validateCodexCompactKnownFields(raw json.RawMessage, subject string, fields ...string) error {
	return validateCodexJSONAuthority(raw, subject, func(path []string, name string) bool {
		if len(path) != 0 {
			return false
		}
		for _, field := range fields {
			if name == field {
				return true
			}
		}
		return false
	})
}

func validateCodexCompactOptionalResponseItemFields(item map[string]json.RawMessage, itemType string) error {
	if itemType != "compaction_trigger" {
		if err := validateCodexCompactOptionalString(item, "id", itemType); err != nil {
			return err
		}
		if itemType != "additional_tools" {
			if err := validateCodexCompactMetadata(item["internal_chat_message_metadata_passthrough"], itemType); err != nil {
				return err
			}
		}
	}
	switch itemType {
	case "message":
		return validateCodexCompactOptionalEnum(item, "phase", itemType, "commentary", "final_answer")
	case "reasoning":
		if err := validateCodexCompactOptionalString(item, "encrypted_content", itemType); err != nil {
			return err
		}
		content, present, err := optionalCodexCompactArray(item, "content", itemType)
		if err != nil || !present {
			return err
		}
		return validateCodexCompactTaggedItems(content, itemType+" content", validateCodexCompactReasoningContentItem)
	case "local_shell_call":
		return validateCodexCompactOptionalString(item, "call_id", itemType)
	case "function_call":
		return validateCodexCompactOptionalString(item, "namespace", itemType)
	case "tool_search_call":
		if err := validateCodexCompactOptionalString(item, "call_id", itemType); err != nil {
			return err
		}
		return validateCodexCompactOptionalString(item, "status", itemType)
	case "custom_tool_call":
		for _, field := range []string{"status", "namespace"} {
			if err := validateCodexCompactOptionalString(item, field, itemType); err != nil {
				return err
			}
		}
	case "custom_tool_call_output":
		return validateCodexCompactOptionalString(item, "name", itemType)
	case "tool_search_output":
		return validateCodexCompactOptionalString(item, "call_id", itemType)
	case "web_search_call":
		if err := validateCodexCompactOptionalString(item, "status", itemType); err != nil {
			return err
		}
		return validateCodexCompactWebSearchAction(item["action"])
	case "image_generation_call":
		return validateCodexCompactOptionalString(item, "revised_prompt", itemType)
	case "context_compaction":
		return validateCodexCompactOptionalString(item, "encrypted_content", itemType)
	}
	return nil
}

func validateCodexCompactOptionalString(object map[string]json.RawMessage, field, itemType string) error {
	raw, ok := object[field]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return fmt.Errorf("Codex compact response %s %s must be a string", itemType, field)
	}
	return nil
}

func validateCodexCompactOptionalEnum(object map[string]json.RawMessage, field, itemType string, allowed ...string) error {
	raw, ok := object[field]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return fmt.Errorf("Codex compact response %s %s must be a string", itemType, field)
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("Codex compact response %s %s is invalid", itemType, field)
}

func optionalCodexCompactArray(object map[string]json.RawMessage, field, itemType string) ([]json.RawMessage, bool, error) {
	raw, ok := object[field]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false, nil
	}
	items, err := decodeCodexCompactArray(raw, itemType+" "+field)
	return items, true, err
}

func validateCodexCompactMetadata(raw json.RawMessage, itemType string) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	if err := validateCodexCompactKnownFields(raw, "Codex compact response metadata", "turn_id"); err != nil {
		return err
	}
	var metadata map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' || json.Unmarshal(raw, &metadata) != nil || metadata == nil {
		return fmt.Errorf("Codex compact response %s metadata must be an object", itemType)
	}
	return validateCodexCompactOptionalString(metadata, "turn_id", itemType+" metadata")
}

func decodeCodexCompactArray(raw json.RawMessage, subject string) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("Codex compact response %s must be an array", subject)
	}
	var items []json.RawMessage
	if json.Unmarshal(trimmed, &items) != nil {
		return nil, fmt.Errorf("Codex compact response %s must be an array", subject)
	}
	return items, nil
}

func validateCodexCompactReasoningContentItem(raw json.RawMessage, item map[string]json.RawMessage, itemType string) error {
	if itemType != "reasoning_text" && itemType != "text" {
		return fmt.Errorf("Codex compact response reasoning content type %q is invalid", itemType)
	}
	if err := validateCodexCompactKnownFields(raw, "Codex compact response reasoning content", "type", "text"); err != nil {
		return err
	}
	return requireCodexCompactString(item, "text", itemType)
}

func validateCodexCompactWebSearchAction(raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	if err := validateCodexCompactKnownFields(raw, "Codex compact response web search action", "type"); err != nil {
		return err
	}
	action, actionType, err := decodeCodexCompactTaggedObject(raw, "web_search_call action")
	if err != nil {
		return err
	}
	fields := []string{"type"}
	switch actionType {
	case "search":
		fields = append(fields, "query", "queries")
	case "open_page":
		fields = append(fields, "url")
	case "find_in_page":
		fields = append(fields, "url", "pattern")
	default:
		return nil
	}
	if err := validateCodexCompactKnownFields(raw, "Codex compact response web search action", fields...); err != nil {
		return err
	}
	switch actionType {
	case "search":
		if err := validateCodexCompactOptionalString(action, "query", actionType); err != nil {
			return err
		}
		queries, present, err := optionalCodexCompactArray(action, "queries", actionType)
		if err != nil || !present {
			return err
		}
		for _, query := range queries {
			var value string
			if bytes.Equal(bytes.TrimSpace(query), []byte("null")) || json.Unmarshal(query, &value) != nil {
				return errors.New("Codex compact response web search queries must contain strings")
			}
		}
	case "open_page":
		return validateCodexCompactOptionalString(action, "url", actionType)
	case "find_in_page":
		for _, field := range []string{"url", "pattern"} {
			if err := validateCodexCompactOptionalString(action, field, actionType); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeCodexCompactTaggedObject(raw json.RawMessage, subject string) (map[string]json.RawMessage, string, error) {
	var object map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' || json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, "", fmt.Errorf("%s must be an object", subject)
	}
	rawType, ok := object["type"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawType), []byte("null")) {
		return nil, "", fmt.Errorf("%s type is required", subject)
	}
	var itemType string
	if json.Unmarshal(rawType, &itemType) != nil {
		return nil, "", fmt.Errorf("%s type must be a string", subject)
	}
	return object, itemType, nil
}

func requireCodexCompactString(object map[string]json.RawMessage, field, itemType string) error {
	_, err := requireCodexCompactStringValue(object, field, itemType)
	return err
}

func requireCodexCompactStringValue(object map[string]json.RawMessage, field, itemType string) (string, error) {
	raw, ok := object[field]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("Codex compact response %s %s is required", itemType, field)
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("Codex compact response %s %s must be a string", itemType, field)
	}
	return value, nil
}

func requireCodexCompactArray(object map[string]json.RawMessage, field, itemType string) ([]json.RawMessage, error) {
	raw, ok := object[field]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' {
		return nil, fmt.Errorf("Codex compact response %s %s must be an array", itemType, field)
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, fmt.Errorf("Codex compact response %s %s must be an array", itemType, field)
	}
	return items, nil
}

type codexCompactTaggedItemValidator func(json.RawMessage, map[string]json.RawMessage, string) error

func validateCodexCompactTaggedItems(items []json.RawMessage, subject string, validate codexCompactTaggedItemValidator) error {
	for _, raw := range items {
		if err := validateCodexCompactKnownFields(raw, "Codex compact response "+subject, "type"); err != nil {
			return err
		}
		item, itemType, err := decodeCodexCompactTaggedObject(raw, subject+" item")
		if err != nil {
			return err
		}
		if err := validate(raw, item, itemType); err != nil {
			return err
		}
	}
	return nil
}

func validateCodexCompactMessageContentItem(raw json.RawMessage, item map[string]json.RawMessage, itemType string) error {
	fields := []string{"type"}
	switch itemType {
	case "input_text", "output_text":
		fields = append(fields, "text")
	case "input_image":
		fields = append(fields, "image_url", "detail")
	case "input_audio":
		fields = append(fields, "audio_url")
	default:
		return fmt.Errorf("Codex compact response message content type %q is invalid", itemType)
	}
	if err := validateCodexCompactKnownFields(raw, "Codex compact response message content", fields...); err != nil {
		return err
	}
	switch itemType {
	case "input_text", "output_text":
		return requireCodexCompactString(item, "text", itemType)
	case "input_image":
		if err := requireCodexCompactString(item, "image_url", itemType); err != nil {
			return err
		}
		return validateCodexCompactOptionalEnum(item, "detail", itemType, "auto", "low", "high", "original")
	default:
		return requireCodexCompactString(item, "audio_url", itemType)
	}
}

func validateCodexCompactAgentContentItem(raw json.RawMessage, item map[string]json.RawMessage, itemType string) error {
	field := ""
	switch itemType {
	case "input_text":
		field = "text"
	case "encrypted_content":
		field = "encrypted_content"
	default:
		return fmt.Errorf("Codex compact response agent content type %q is invalid", itemType)
	}
	if err := validateCodexCompactKnownFields(raw, "Codex compact response agent content", "type", field); err != nil {
		return err
	}
	return requireCodexCompactString(item, field, itemType)
}

func validateCodexCompactReasoningSummaryItem(raw json.RawMessage, item map[string]json.RawMessage, itemType string) error {
	if itemType != "summary_text" {
		return fmt.Errorf("Codex compact response reasoning summary type %q is invalid", itemType)
	}
	if err := validateCodexCompactKnownFields(raw, "Codex compact response reasoning summary", "type", "text"); err != nil {
		return err
	}
	return requireCodexCompactString(item, "text", itemType)
}

func validateCodexCompactLocalShellAction(raw json.RawMessage) error {
	if err := validateCodexCompactKnownFields(raw, "Codex compact response local shell action", "type", "command", "timeout_ms", "working_directory", "env", "user"); err != nil {
		return err
	}
	action, actionType, err := decodeCodexCompactTaggedObject(raw, "local_shell_call action")
	if err != nil {
		return err
	}
	if actionType != "exec" {
		return fmt.Errorf("Codex compact response local_shell_call action type %q is invalid", actionType)
	}
	commands, err := requireCodexCompactArray(action, "command", "local_shell_call action")
	if err != nil {
		return err
	}
	for _, command := range commands {
		var value string
		if bytes.Equal(bytes.TrimSpace(command), []byte("null")) || json.Unmarshal(command, &value) != nil {
			return errors.New("Codex compact response local_shell_call command must contain strings")
		}
	}
	if rawTimeout, ok := action["timeout_ms"]; ok && !bytes.Equal(bytes.TrimSpace(rawTimeout), []byte("null")) {
		var timeout uint64
		if json.Unmarshal(rawTimeout, &timeout) != nil {
			return errors.New("Codex compact response local_shell_call timeout_ms must be an unsigned integer")
		}
	}
	for _, field := range []string{"working_directory", "user"} {
		if err := validateCodexCompactOptionalString(action, field, "local_shell_call action"); err != nil {
			return err
		}
	}
	if rawEnv, ok := action["env"]; ok && !bytes.Equal(bytes.TrimSpace(rawEnv), []byte("null")) {
		var env map[string]json.RawMessage
		if len(bytes.TrimSpace(rawEnv)) == 0 || bytes.TrimSpace(rawEnv)[0] != '{' || json.Unmarshal(rawEnv, &env) != nil || env == nil {
			return errors.New("Codex compact response local_shell_call env must be an object")
		}
		for _, rawValue := range env {
			var value string
			if bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) || json.Unmarshal(rawValue, &value) != nil {
				return errors.New("Codex compact response local_shell_call env values must be strings")
			}
		}
	}
	return nil
}

func validateCodexCompactFunctionOutput(raw json.RawMessage, itemType string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("Codex compact response %s output is required", itemType)
	}
	if trimmed[0] == '"' {
		var value string
		if json.Unmarshal(trimmed, &value) == nil {
			return nil
		}
	}
	if trimmed[0] != '[' {
		return fmt.Errorf("Codex compact response %s output must be a string or array", itemType)
	}
	var items []json.RawMessage
	if json.Unmarshal(trimmed, &items) != nil {
		return fmt.Errorf("Codex compact response %s output must be a string or array", itemType)
	}
	return validateCodexCompactTaggedItems(items, itemType+" output", validateCodexCompactFunctionOutputItem)
}

func validateCodexCompactFunctionOutputItem(raw json.RawMessage, item map[string]json.RawMessage, itemType string) error {
	fields := []string{"type"}
	switch itemType {
	case "input_text":
		fields = append(fields, "text")
	case "input_image":
		fields = append(fields, "image_url", "detail")
	case "input_audio":
		fields = append(fields, "audio_url")
	case "encrypted_content":
		fields = append(fields, "encrypted_content")
	default:
		return fmt.Errorf("Codex compact response function output type %q is invalid", itemType)
	}
	if err := validateCodexCompactKnownFields(raw, "Codex compact response function output", fields...); err != nil {
		return err
	}
	switch itemType {
	case "input_text":
		return requireCodexCompactString(item, "text", itemType)
	case "input_image":
		if err := requireCodexCompactString(item, "image_url", itemType); err != nil {
			return err
		}
		return validateCodexCompactOptionalEnum(item, "detail", itemType, "auto", "low", "high", "original")
	case "input_audio":
		return requireCodexCompactString(item, "audio_url", itemType)
	default:
		return requireCodexCompactString(item, "encrypted_content", itemType)
	}
}

func isCodexCompactAuthorityField(path []string, name string) bool {
	if len(path) == 0 {
		return codexJSONNameEqual(name, "id") || codexJSONNameEqual(name, "output") || codexJSONNameEqual(name, "error")
	}
	return len(path) == 1 && codexJSONNameEqual(path[0], "output") && codexJSONNameEqual(name, "type")
}

func codexJSONRawField(object map[string]json.RawMessage, field string) (json.RawMessage, bool) {
	for name, value := range object {
		if strings.EqualFold(name, field) {
			return value, true
		}
	}
	return nil, false
}

func validateCodexLifecycleAuthority(payload []byte) error {
	return validateCodexJSONAuthority(payload, "Codex lifecycle event", isCodexLifecycleAuthorityField)
}

func isCodexLifecycleAuthorityField(path []string, name string) bool {
	switch {
	case len(path) == 0:
		return codexJSONNameEqual(name, "type") || codexJSONNameEqual(name, "response") || codexJSONNameEqual(name, "end_turn") || codexJSONNameEqual(name, "headers")
	case len(path) == 1 && codexJSONNameEqual(path[0], "response"):
		return codexJSONNameEqual(name, "id") || codexJSONNameEqual(name, "end_turn")
	case len(path) == 1 && codexJSONNameEqual(path[0], "headers"):
		return codexJSONNameEqual(name, "x-codex-turn-state")
	default:
		return false
	}
}

func validateCodexUnaryAuthority(payload []byte) error {
	return validateCodexJSONAuthority(payload, "Codex unary response", func(path []string, name string) bool {
		return len(path) == 0 && codexJSONNameEqual(name, "status")
	})
}

func ParseCodexTurnStateHeader(header http.Header) (string, bool, error) {
	values := header.Values("x-codex-turn-state")
	if len(values) == 0 {
		return "", false, nil
	}
	value := values[0]
	if len(value) > codexTurnMetadataMaxBytes {
		return "", false, errors.New("Codex turn state header exceeds limit")
	}
	for _, other := range values[1:] {
		if other != value {
			return "", false, errors.New("conflicting Codex turn state headers")
		}
	}
	return value, value != "", nil
}

func parseCodexTurnStateObject(raw json.RawMessage) (string, bool, error) {
	var envelope struct {
		Headers map[string]json.RawMessage `json:"headers"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", false, err
	}
	for name, value := range envelope.Headers {
		if !strings.EqualFold(name, "x-codex-turn-state") {
			continue
		}
		var state string
		if err := json.Unmarshal(value, &state); err != nil {
			return "", false, errors.New("Codex turn state metadata must be a string")
		}
		if len(state) > codexTurnMetadataMaxBytes {
			return "", false, errors.New("Codex turn state metadata exceeds limit")
		}
		return state, state != "", nil
	}
	return "", false, nil
}

func jsonContainsKey(body []byte, key string) bool {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			for name, child := range typed {
				if name == key {
					return true
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}
