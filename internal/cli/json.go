package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode"
)

// EncodeData encodes a declared resource. Callers must handle errors before
// returning an Outcome; a failed encoding is never a successful null resource.
func EncodeData(value any) (json.RawMessage, error) {
	return json.Marshal(value)
}

// WriteJSON prepares the complete envelope before touching the writer. Each
// call writes one record, so handlers can also use it for their JSONL events.
func WriteJSON(w io.Writer, path string, outcome Outcome) error {
	data := outcome.Data
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	errors, warnings := outcome.Errors, outcome.Warnings
	if errors == nil {
		errors = []Diagnostic{}
	}
	if warnings == nil {
		warnings = []Diagnostic{}
	}
	return writeJSONValue(w, struct {
		SchemaVersion int             `json:"schema_version"`
		Command       string          `json:"command"`
		OK            bool            `json:"ok"`
		Data          json.RawMessage `json:"data"`
		Errors        []Diagnostic    `json:"errors"`
		Warnings      []Diagnostic    `json:"warnings"`
	}{cliSchemaVersion, path, outcome.ExitCode == 0, data, errors, warnings})
}

func writeJSONValue(w io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeBytes(w, encoded)
}

// Do not retry a short write: stdout may already contain a partial record.
func writeBytes(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// HumanValue formats one interpolated scalar (or pointer to a scalar). Nulls
// use an em dash, decimals are locale independent, and all Unicode Cc controls
// are escaped. Adapters own their exact templates and ordered array loops;
// never pass a complete template here, since its line breaks are structural.
func HumanValue(value any) string {
	v := reflect.ValueOf(value)
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return "—"
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return "—"
	}
	var text string
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		text = strconv.FormatFloat(v.Float(), 'f', -1, v.Type().Bits())
	default:
		text = fmt.Sprint(v.Interface())
	}
	var escaped strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) {
			fmt.Fprintf(&escaped, `\u%04x`, r)
		} else {
			escaped.WriteRune(r)
		}
	}
	return escaped.String()
}
