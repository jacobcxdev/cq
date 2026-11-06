package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

var (
	lowerHex64Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lowerHex40Pattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// CanonicalJSONV1 serialises a JSON value using the RFC 8785 JSON
// Canonicalization Scheme. It returns the canonical object bytes without a
// trailing newline.
func CanonicalJSONV1(value any) ([]byte, error) {
	if err := validateJSONStrings(reflect.ValueOf(value), make(map[visit]bool)); err != nil {
		return nil, err
	}
	marshalled, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal JSON value: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(marshalled))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode JSON value: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	canonical := make([]byte, 0, len(marshalled))
	canonical, err = appendCanonicalJSON(canonical, decoded)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

type visit struct {
	typeOf  reflect.Type
	pointer uintptr
}

func validateJSONStrings(value reflect.Value, seen map[visit]bool) error {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return validateJSONStrings(value.Elem(), seen)
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		key := visit{typeOf: value.Type(), pointer: value.Pointer()}
		if seen[key] {
			return nil
		}
		seen[key] = true
		defer delete(seen, key)
		return validateJSONStrings(value.Elem(), seen)
	}
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return fmt.Errorf("JSON string is not valid UTF-8")
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateJSONStrings(iterator.Key(), seen); err != nil {
				return err
			}
			if err := validateJSONStrings(iterator.Value(), seen); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if !value.IsNil() {
			key := visit{typeOf: value.Type(), pointer: value.Pointer()}
			if seen[key] {
				return nil
			}
			seen[key] = true
			defer delete(seen, key)
		}
		fallthrough
	case reflect.Array:
		for index := range value.Len() {
			if err := validateJSONStrings(value.Index(index), seen); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for index := range value.NumField() {
			if value.Type().Field(index).PkgPath == "" {
				if err := validateJSONStrings(value.Field(index), seen); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func appendCanonicalJSON(destination []byte, value any) ([]byte, error) {
	switch typed := value.(type) {
	case nil:
		return append(destination, "null"...), nil
	case bool:
		return strconv.AppendBool(destination, typed), nil
	case string:
		return appendCanonicalString(destination, typed), nil
	case json.Number:
		return appendCanonicalNumber(destination, typed)
	case []any:
		destination = append(destination, '[')
		for index, item := range typed {
			if index > 0 {
				destination = append(destination, ',')
			}
			var err error
			destination, err = appendCanonicalJSON(destination, item)
			if err != nil {
				return nil, err
			}
		}
		return append(destination, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(left, right int) bool {
			return lessUTF16(keys[left], keys[right])
		})
		destination = append(destination, '{')
		for index, key := range keys {
			if index > 0 {
				destination = append(destination, ',')
			}
			destination = appendCanonicalString(destination, key)
			destination = append(destination, ':')
			var err error
			destination, err = appendCanonicalJSON(destination, typed[key])
			if err != nil {
				return nil, err
			}
		}
		return append(destination, '}'), nil
	default:
		return nil, fmt.Errorf("unsupported JSON value %T", value)
	}
}

func appendCanonicalString(destination []byte, value string) []byte {
	const hex = "0123456789abcdef"
	destination = append(destination, '"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			destination = append(destination, '\\', byte(character))
		case '\b':
			destination = append(destination, `\b`...)
		case '\t':
			destination = append(destination, `\t`...)
		case '\n':
			destination = append(destination, `\n`...)
		case '\f':
			destination = append(destination, `\f`...)
		case '\r':
			destination = append(destination, `\r`...)
		default:
			if character < 0x20 {
				destination = append(destination, '\\', 'u', '0', '0', hex[byte(character)>>4], hex[byte(character)&0xf])
			} else {
				destination = utf8.AppendRune(destination, character)
			}
		}
	}
	return append(destination, '"')
}

func appendCanonicalNumber(destination []byte, number json.Number) ([]byte, error) {
	representation := number.String()
	value, err := strconv.ParseFloat(representation, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON number %q: %w", representation, err)
	}
	if value == 0 {
		return append(destination, '0'), nil
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("serialise JSON number %q: %w", representation, err)
	}
	return append(destination, canonical...), nil
}

func lessUTF16(left, right string) bool {
	leftUnits := utf16.Encode([]rune(left))
	rightUnits := utf16.Encode([]rune(right))
	for index := 0; index < min(len(leftUnits), len(rightUnits)); index++ {
		if leftUnits[index] != rightUnits[index] {
			return leftUnits[index] < rightUnits[index]
		}
	}
	return len(leftUnits) < len(rightUnits)
}

func readBoundedBytes(reader io.Reader, limit int) ([]byte, error) {
	result := make([]byte, 0, limit)
	buffer := make([]byte, 4<<10)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			if len(result)+count > limit {
				return nil, fmt.Errorf("input exceeds %d bytes", limit)
			}
			result = append(result, buffer[:count]...)
		}
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, io.ErrNoProgress
		}
	}
}

func decodeStrictJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return value, nil
}

func decodeStrictJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object member name is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate object member %q", key)
			}
			value, err := decodeStrictJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case '[':
		var array []any
		for decoder.More() {
			value, err := decodeStrictJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delimiter)
	}
}

func decodeClosedJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}
