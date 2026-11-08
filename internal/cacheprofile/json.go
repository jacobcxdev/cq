package cacheprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const maxBodyBytes = 10 << 20

// Reject duplicate keys: retaining whichever value happened to occur last
// would make an ambiguous usage authority or inference setting appear valid.
func decodeObject(body []byte) (map[string]any, error) {
	if len(body) > maxBodyBytes {
		return nil, errors.New("cache profiling input limit")
	}
	if !utf8.Valid(body) {
		return nil, errors.New("invalid cache profiling JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, errors.New("invalid cache profiling JSON")
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, errors.New("invalid cache profiling JSON")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("invalid cache profiling object")
	}
	return object, nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("JSON depth limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := make(map[string]any)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, exists := object[name]; exists {
				return nil, errors.New("duplicate object key")
			}
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid object end")
		}
		return object, nil
	case json.Delim('['):
		values := make([]any, 0)
		for decoder.More() {
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid array end")
		}
		return values, nil
	case json.Delim('}'), json.Delim(']'):
		return nil, errors.New("unexpected JSON delimiter")
	default:
		return token, nil
	}
}
