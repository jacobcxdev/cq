package proxy

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"sort"

	"github.com/klauspost/compress/zstd"
)

const credentialEchoRepresentationLimit = 64 << 20

type CredentialEchoOutcome string

const (
	CredentialEchoClear         CredentialEchoOutcome = "clear"
	CredentialEchoMatch         CredentialEchoOutcome = "match"
	CredentialEchoIndeterminate CredentialEchoOutcome = "indeterminate"
)

type CredentialEchoScanEvidenceV1 struct {
	SchemaVersion      int                   `json:"schema_version"`
	Outcome            CredentialEchoOutcome `json:"outcome"`
	InspectedByteCount uint64                `json:"inspected_byte_count"`
	EvidenceDigest     string                `json:"evidence_digest"`
}

type CredentialEchoScanner struct {
	key        [sha256.Size]byte
	patterns   [][]byte
	maxPattern int
}

func NewCredentialEchoScanner(root []byte, runID string, credentials [][]byte) (*CredentialEchoScanner, error) {
	if len(root) != sha256.Size || runID == "" || len(credentials) == 0 {
		return nil, errors.New("credential echo scanner authority unavailable")
	}
	purposeKey, err := DeriveAuthorityKey(root, "cq/credential-echo-scanner/v1", sha256.Size)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, purposeKey)
	_, _ = mac.Write([]byte("cq/credential-echo-run/v1\x00"))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(runID)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write([]byte(runID))
	scanner := &CredentialEchoScanner{}
	copy(scanner.key[:], mac.Sum(nil))
	for _, credential := range credentials {
		if len(credential) == 0 || len(credential) > 16<<10 {
			return nil, errors.New("credential echo pattern invalid")
		}
		pattern := bytes.Clone(credential)
		scanner.patterns = append(scanner.patterns, pattern)
		if len(pattern) > scanner.maxPattern {
			scanner.maxPattern = len(pattern)
		}
	}
	return scanner, nil
}

func (scanner *CredentialEchoScanner) ScanHeaders(header http.Header) CredentialEchoScanEvidenceV1 {
	if scanner == nil {
		return CredentialEchoScanEvidenceV1{SchemaVersion: 1, Outcome: CredentialEchoIndeterminate}
	}
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	var inspected uint64
	for _, name := range names {
		nameBytes := []byte(name)
		inspected += uint64(len(nameBytes))
		if scanner.contains(nameBytes) {
			return scanner.evidence(CredentialEchoMatch, inspected)
		}
		for _, value := range header[name] {
			valueBytes := []byte(value)
			inspected += uint64(len(valueBytes))
			if scanner.contains(valueBytes) {
				return scanner.evidence(CredentialEchoMatch, inspected)
			}
		}
	}
	return scanner.evidence(CredentialEchoClear, inspected)
}

func (scanner *CredentialEchoScanner) ScanRepresentation(encoded []byte, encoding string) CredentialEchoScanEvidenceV1 {
	if scanner == nil || len(encoded) > credentialEchoRepresentationLimit {
		return CredentialEchoScanEvidenceV1{SchemaVersion: 1, Outcome: CredentialEchoIndeterminate}
	}
	inspected := uint64(len(encoded))
	if scanner.contains(encoded) {
		return scanner.evidence(CredentialEchoMatch, inspected)
	}
	var decoded []byte
	switch encoding {
	case "", "identity":
		return scanner.evidence(CredentialEchoClear, inspected)
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(encoded))
		if err != nil {
			return scanner.evidence(CredentialEchoIndeterminate, inspected)
		}
		decoded, err = io.ReadAll(io.LimitReader(reader, credentialEchoRepresentationLimit+1))
		closeErr := reader.Close()
		if err != nil || closeErr != nil || len(decoded) > credentialEchoRepresentationLimit {
			return scanner.evidence(CredentialEchoIndeterminate, inspected)
		}
	case "zstd":
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecodeAllCapLimit(true), zstd.WithDecoderMaxMemory(credentialEchoRepresentationLimit))
		if err != nil {
			return scanner.evidence(CredentialEchoIndeterminate, inspected)
		}
		decoded, err = decoder.DecodeAll(encoded, make([]byte, 0, min(len(encoded)*4, credentialEchoRepresentationLimit)))
		decoder.Close()
		if err != nil || len(decoded) > credentialEchoRepresentationLimit {
			return scanner.evidence(CredentialEchoIndeterminate, inspected)
		}
	default:
		return scanner.evidence(CredentialEchoIndeterminate, inspected)
	}
	inspected += uint64(len(decoded))
	if scanner.contains(decoded) {
		return scanner.evidence(CredentialEchoMatch, inspected)
	}
	return scanner.evidence(CredentialEchoClear, inspected)
}

func (scanner *CredentialEchoScanner) contains(value []byte) bool {
	for _, pattern := range scanner.patterns {
		if bytes.Contains(value, pattern) {
			return true
		}
	}
	return false
}

func (scanner *CredentialEchoScanner) evidence(outcome CredentialEchoOutcome, inspected uint64) CredentialEchoScanEvidenceV1 {
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], inspected)
	digest, err := FramedHMACSHA256Hex(scanner.key[:], "cq/credential-echo-evidence/v1\x00", []byte(outcome), count[:])
	if err != nil {
		return CredentialEchoScanEvidenceV1{SchemaVersion: 1, Outcome: CredentialEchoIndeterminate, InspectedByteCount: inspected}
	}
	return CredentialEchoScanEvidenceV1{SchemaVersion: 1, Outcome: outcome, InspectedByteCount: inspected, EvidenceDigest: digest}
}

type CredentialEchoStreamScanner struct {
	scanner   *CredentialEchoScanner
	tail      []byte
	inspected uint64
	terminal  bool
}

func (scanner *CredentialEchoScanner) NewStream() *CredentialEchoStreamScanner {
	return &CredentialEchoStreamScanner{scanner: scanner}
}

func (stream *CredentialEchoStreamScanner) Push(chunk []byte, final bool) ([]byte, CredentialEchoScanEvidenceV1, error) {
	if stream == nil || stream.scanner == nil || stream.terminal {
		return nil, CredentialEchoScanEvidenceV1{}, errors.New("credential echo stream unavailable")
	}
	if stream.inspected+uint64(len(chunk)) > credentialEchoRepresentationLimit {
		stream.terminal = true
		return nil, stream.scanner.evidence(CredentialEchoIndeterminate, stream.inspected), errors.New("credential echo stream limit exceeded")
	}
	stream.inspected += uint64(len(chunk))
	combined := make([]byte, 0, len(stream.tail)+len(chunk))
	combined = append(combined, stream.tail...)
	combined = append(combined, chunk...)
	if stream.scanner.contains(combined) {
		stream.terminal = true
		stream.tail = nil
		return nil, stream.scanner.evidence(CredentialEchoMatch, stream.inspected), nil
	}
	if final {
		stream.terminal = true
		stream.tail = nil
		return combined, stream.scanner.evidence(CredentialEchoClear, stream.inspected), nil
	}
	retained := max(0, stream.scanner.maxPattern-1)
	if retained > len(combined) {
		retained = len(combined)
	}
	safeLength := len(combined) - retained
	safe := bytes.Clone(combined[:safeLength])
	stream.tail = bytes.Clone(combined[safeLength:])
	return safe, stream.scanner.evidence(CredentialEchoClear, stream.inspected), nil
}
