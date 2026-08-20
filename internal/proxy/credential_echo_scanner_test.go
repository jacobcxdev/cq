package proxy

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"testing"
)

func TestCredentialEchoScannerBlocksHeadersAndCrossChunkBody(t *testing.T) {
	credential := []byte("secret-access-token")
	scanner, err := NewCredentialEchoScanner(bytes.Repeat([]byte{0x31}, 32), "run-1", [][]byte{credential})
	if err != nil {
		t.Fatal(err)
	}
	headerEvidence := scanner.ScanHeaders(http.Header{"X-Provider-Metadata": {"prefix-secret-access-token-suffix"}})
	if headerEvidence.Outcome != CredentialEchoMatch || headerEvidence.EvidenceDigest == "" {
		t.Fatalf("header evidence = %#v", headerEvidence)
	}
	stream := scanner.NewStream()
	first, evidence, err := stream.Push([]byte("safe-secret-ac"), false)
	if err != nil || evidence.Outcome != CredentialEchoClear || bytes.Contains(first, []byte("secret")) {
		t.Fatalf("first = %q evidence=%#v err=%v", first, evidence, err)
	}
	second, evidence, err := stream.Push([]byte("cess-token-tail"), true)
	if err != nil || len(second) != 0 || evidence.Outcome != CredentialEchoMatch {
		t.Fatalf("second = %q evidence=%#v err=%v", second, evidence, err)
	}
}

func TestCredentialEchoScannerChecksEncodedAndDecodedRepresentations(t *testing.T) {
	credential := []byte("secret-access-token")
	scanner, err := NewCredentialEchoScanner(bytes.Repeat([]byte{0x32}, 32), "run-2", [][]byte{credential})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	_, _ = writer.Write([]byte("before-secret-access-token-after"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	evidence := scanner.ScanRepresentation(encoded.Bytes(), "gzip")
	if evidence.Outcome != CredentialEchoMatch {
		t.Fatalf("gzip evidence = %#v", evidence)
	}
	if bytes.Contains([]byte(evidence.EvidenceDigest), credential) {
		t.Fatal("evidence exposed credential")
	}
	if evidence := scanner.ScanRepresentation([]byte("safe"), "br"); evidence.Outcome != CredentialEchoIndeterminate {
		t.Fatalf("unknown encoding evidence = %#v", evidence)
	}
}
