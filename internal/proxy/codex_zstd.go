package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type CodexZstdLimits struct {
	MaxEncodedBytes int
	MaxDecodedBytes int
	MaxExpansion    int
}

var DefaultCodexZstdLimits = CodexZstdLimits{
	MaxEncodedBytes: 2 << 20,
	MaxDecodedBytes: codexProtocolMaxBytes,
	MaxExpansion:    128,
}

type CodexDecodedRequest struct {
	original []byte
	decoded  []byte
	encoding string
}

func (r CodexDecodedRequest) Decoded() []byte  { return append([]byte(nil), r.decoded...) }
func (r CodexDecodedRequest) Replay() []byte   { return append([]byte(nil), r.original...) }
func (r CodexDecodedRequest) Encoding() string { return r.encoding }

func parseCodexContentEncoding(header http.Header) (string, error) {
	var values []string
	for name, entries := range header {
		if !strings.EqualFold(name, "Content-Encoding") {
			continue
		}
		for _, entry := range entries {
			for _, value := range strings.Split(entry, ",") {
				value = strings.TrimSpace(strings.ToLower(value))
				if value == "" {
					return "", errors.New("Codex content encoding is empty")
				}
				values = append(values, value)
			}
		}
	}
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 {
		return "", errors.New("Codex request has multiple content encodings")
	}
	if values[0] != "identity" && values[0] != "zstd" {
		return "", fmt.Errorf("unsupported Codex content encoding %q", values[0])
	}
	return values[0], nil
}

func DecodeCodexRequest(body []byte, contentEncoding string, limits CodexZstdLimits) (CodexDecodedRequest, error) {
	if limits.MaxEncodedBytes <= 0 || limits.MaxDecodedBytes <= 0 || limits.MaxExpansion <= 0 {
		return CodexDecodedRequest{}, errors.New("invalid Codex zstd limits")
	}
	if len(body) > limits.MaxEncodedBytes {
		return CodexDecodedRequest{}, errors.New("Codex encoded request exceeds limit")
	}
	encoding := strings.TrimSpace(strings.ToLower(contentEncoding))
	if encoding == "" || encoding == "identity" {
		if len(body) > limits.MaxDecodedBytes {
			return CodexDecodedRequest{}, errors.New("Codex decoded request exceeds limit")
		}
		original := append([]byte(nil), body...)
		return CodexDecodedRequest{original: original, decoded: append([]byte(nil), body...), encoding: encoding}, nil
	}
	if encoding != "zstd" {
		return CodexDecodedRequest{}, fmt.Errorf("unsupported Codex content encoding %q", contentEncoding)
	}
	decodeLimit, ratioLimited := codexZstdDecodeLimit(len(body), limits)
	var header zstd.Header
	if err := header.Decode(body); err != nil {
		return CodexDecodedRequest{}, fmt.Errorf("decode Codex zstd header: %w", err)
	}
	if header.HasFCS {
		if header.FrameContentSize > uint64(limits.MaxDecodedBytes) {
			return CodexDecodedRequest{}, errors.New("Codex decoded request exceeds limit")
		}
		if ratioLimited && header.FrameContentSize > uint64(decodeLimit) {
			return CodexDecodedRequest{}, errors.New("Codex zstd expansion ratio exceeds limit")
		}
	}
	decodeTarget := make([]byte, 0, decodeLimit)
	decodeOptions := []zstd.DOption{
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(uint64(max(1024, limits.MaxDecodedBytes))),
		zstd.WithDecodeAllCapLimit(true),
	}
	decodeOptions = append(decodeOptions, zstd.WithDecoderMaxMemory(uint64(decodeLimit)))
	decoder, err := zstd.NewReader(nil, decodeOptions...)
	if err != nil {
		return CodexDecodedRequest{}, fmt.Errorf("create Codex zstd decoder: %w", err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(body, decodeTarget)
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) ||
			errors.Is(err, zstd.ErrWindowSizeExceeded) ||
			strings.HasPrefix(err.Error(), "output bigger than max block size") {
			if ratioLimited {
				return CodexDecodedRequest{}, errors.New("Codex zstd expansion ratio exceeds limit")
			}
			return CodexDecodedRequest{}, errors.New("Codex decoded request exceeds limit")
		}
		return CodexDecodedRequest{}, fmt.Errorf("decode Codex zstd request: %w", err)
	}
	if len(decoded) > limits.MaxDecodedBytes {
		return CodexDecodedRequest{}, errors.New("Codex decoded request exceeds limit")
	}
	if ratioLimited && len(decoded) > decodeLimit {
		return CodexDecodedRequest{}, errors.New("Codex zstd expansion ratio exceeds limit")
	}
	original := append([]byte(nil), body...)
	return CodexDecodedRequest{original: original, decoded: bytes.Clone(decoded), encoding: encoding}, nil
}

func codexZstdDecodeLimit(encodedBytes int, limits CodexZstdLimits) (int, bool) {
	decodeLimit := limits.MaxDecodedBytes
	if encodedBytes <= limits.MaxDecodedBytes/limits.MaxExpansion {
		expansionLimit := encodedBytes * limits.MaxExpansion
		if expansionLimit < decodeLimit {
			return expansionLimit, true
		}
	}
	return decodeLimit, false
}

func EncodeCodexRequest(body []byte, contentEncoding string, limits CodexZstdLimits) ([]byte, error) {
	if limits.MaxEncodedBytes <= 0 || limits.MaxDecodedBytes <= 0 || limits.MaxExpansion <= 0 {
		return nil, errors.New("invalid Codex zstd limits")
	}
	if len(body) > limits.MaxDecodedBytes {
		return nil, errors.New("Codex decoded request exceeds limit")
	}
	encoding := strings.TrimSpace(strings.ToLower(contentEncoding))
	if encoding == "" || encoding == "identity" {
		if len(body) > limits.MaxEncodedBytes {
			return nil, errors.New("Codex encoded request exceeds limit")
		}
		return bytes.Clone(body), nil
	}
	if encoding != "zstd" {
		return nil, fmt.Errorf("unsupported Codex content encoding %q", contentEncoding)
	}
	encoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(false),
		zstd.WithEncoderLevel(zstd.SpeedDefault),
	)
	if err != nil {
		return nil, fmt.Errorf("create Codex zstd encoder: %w", err)
	}
	defer encoder.Close()
	encoded := encoder.EncodeAll(body, nil)
	if len(encoded) > limits.MaxEncodedBytes {
		return nil, errors.New("Codex encoded request exceeds limit")
	}
	decodeLimit, ratioLimited := codexZstdDecodeLimit(len(encoded), limits)
	if ratioLimited && len(body) > decodeLimit {
		return nil, errors.New("Codex zstd expansion ratio exceeds limit")
	}
	return bytes.Clone(encoded), nil
}
