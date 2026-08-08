package proxy

import (
	"bytes"
	"errors"
	"fmt"
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
	var header zstd.Header
	if err := header.Decode(body); err != nil {
		return CodexDecodedRequest{}, fmt.Errorf("decode Codex zstd header: %w", err)
	}
	if header.HasFCS {
		if header.FrameContentSize > uint64(limits.MaxDecodedBytes) {
			return CodexDecodedRequest{}, errors.New("Codex decoded request exceeds limit")
		}
		if header.FrameContentSize > uint64(len(body))*uint64(limits.MaxExpansion) {
			return CodexDecodedRequest{}, errors.New("Codex zstd expansion ratio exceeds limit")
		}
	}
	decoder, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(uint64(limits.MaxDecodedBytes)),
		zstd.WithDecoderMaxWindow(uint64(max(1024, limits.MaxDecodedBytes))),
	)
	if err != nil {
		return CodexDecodedRequest{}, fmt.Errorf("create Codex zstd decoder: %w", err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(body, nil)
	if err != nil {
		return CodexDecodedRequest{}, fmt.Errorf("decode Codex zstd request: %w", err)
	}
	if len(decoded) > limits.MaxDecodedBytes {
		return CodexDecodedRequest{}, errors.New("Codex decoded request exceeds limit")
	}
	if len(decoded) > len(body)*limits.MaxExpansion {
		return CodexDecodedRequest{}, errors.New("Codex zstd expansion ratio exceeds limit")
	}
	original := append([]byte(nil), body...)
	return CodexDecodedRequest{original: original, decoded: bytes.Clone(decoded), encoding: encoding}, nil
}
