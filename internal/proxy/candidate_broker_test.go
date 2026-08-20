package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCandidateBrokerConsumesBeforeProviderBytesAndWithholdsCredentialEcho(t *testing.T) {
	store, sourceDigest := newCandidateProviderBrokerStore(t)
	credential := []byte("provider-secret-token")
	scanner, err := NewCredentialEchoScanner(bytes.Repeat([]byte{0x51}, 32), "run-broker", [][]byte{credential})
	if err != nil {
		t.Fatal(err)
	}
	transport := &recordingCandidateProviderTransport{store: store, runID: "run-broker", response: CandidateProviderUpstreamResponseV1{
		StatusCode:  200,
		Headers:     http.Header{"Content-Type": {"application/json"}},
		EncodedBody: []byte(`{"result":"provider-secret-token"}`),
	}}
	broker := newCandidateProviderBrokerForTest(t, store, sourceDigest, scanner, transport, nil)
	granted, err := broker.Acquire(context.Background(), candidateAcquireForTest("http", digestBytes([]byte("request"))))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if granted.Capability.Signature == "" || granted.Capability.CapabilityDigest == "" {
		t.Fatalf("granted = %#v", granted)
	}
	receipt, err := broker.Exchange(context.Background(), granted.Capability, CandidateProviderExchange{Body: []byte("request")})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !transport.consumedBeforeCall || transport.calls != 1 {
		t.Fatalf("transport calls=%d consumedBeforeCall=%v", transport.calls, transport.consumedBeforeCall)
	}
	if receipt.Outcome != CandidateProviderEchoBlocked || receipt.Response != nil {
		t.Fatalf("receipt exposed echo = %#v", receipt)
	}
	if got := candidateBrokerJournalKinds(store, "run-broker"); !equalStrings(got, []string{"capability_issued", "capability_consumed", "capability_terminal"}) {
		t.Fatalf("journal kinds = %v", got)
	}
	if len(store.evidence["run-broker"]) != 1 {
		t.Fatalf("durable scan evidence count = %d", len(store.evidence["run-broker"]))
	}
}

func TestCandidateBrokerReopensConsumedCapabilityWithoutReplay(t *testing.T) {
	store, sourceDigest := newCandidateProviderBrokerStore(t)
	credential := []byte("provider-secret-token")
	scanner, err := NewCredentialEchoScanner(bytes.Repeat([]byte{0x52}, 32), "run-broker", [][]byte{credential})
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("crash after consumption")
	transport := &recordingCandidateProviderTransport{store: store, runID: "run-broker", response: CandidateProviderUpstreamResponseV1{StatusCode: 200, EncodedBody: []byte("safe")}}
	broker := newCandidateProviderBrokerForTest(t, store, sourceDigest, scanner, transport, func(phase string) error {
		if phase == "consumed_durable" {
			return crash
		}
		return nil
	})
	granted, err := broker.Acquire(context.Background(), candidateAcquireForTest("http", digestBytes([]byte("request"))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Exchange(context.Background(), granted.Capability, CandidateProviderExchange{Body: []byte("request")}); !errors.Is(err, crash) {
		t.Fatalf("first Exchange error = %v", err)
	}
	if transport.calls != 0 {
		t.Fatalf("provider called before crash: %d", transport.calls)
	}

	reopenedStore := reopenCandidateProviderBrokerStore(t, store)
	reopenedTransport := &recordingCandidateProviderTransport{store: reopenedStore, runID: "run-broker", response: transport.response}
	reopened := newCandidateProviderBrokerForTest(t, reopenedStore, sourceDigest, scanner, reopenedTransport, nil)
	if _, err := reopened.Exchange(context.Background(), granted.Capability, CandidateProviderExchange{Body: []byte("request")}); !errors.Is(err, ErrCandidateCapabilityIndeterminate) {
		t.Fatalf("replayed Exchange error = %v", err)
	}
	if reopenedTransport.calls != 0 {
		t.Fatalf("replay reached provider: %d", reopenedTransport.calls)
	}
}

func TestCandidateBrokerRejectsMalformedMethodBeforeIssue(t *testing.T) {
	store, sourceDigest := newCandidateProviderBrokerStore(t)
	scanner, err := NewCredentialEchoScanner(bytes.Repeat([]byte{0x58}, 32), "run-broker", [][]byte{[]byte("provider-secret-token")})
	if err != nil {
		t.Fatal(err)
	}
	transport := &recordingCandidateProviderTransport{store: store, runID: "run-broker"}
	broker := newCandidateProviderBrokerForTest(t, store, sourceDigest, scanner, transport, nil)
	acquire := candidateAcquireForTest("http", digestBytes([]byte("request")))
	acquire.Method = "POST BAD"
	if _, err := broker.Acquire(context.Background(), acquire); err == nil {
		t.Fatal("Acquire accepted malformed method")
	}
	if len(store.JournalRecords("run-broker")) != 0 {
		t.Fatal("malformed acquire reached durable issue")
	}
}

func TestCandidateBrokerClearsCompleteHTTPAndStreamingResponsesBeforeRelease(t *testing.T) {
	tests := []struct {
		protocol string
		response CandidateProviderUpstreamResponseV1
	}{
		{protocol: "http", response: CandidateProviderUpstreamResponseV1{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, EncodedBody: []byte(`{"ok":true}`)}},
		{protocol: "sse", response: CandidateProviderUpstreamResponseV1{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, EncodedBody: []byte("data: {\"type\":\"response.completed\"}\n\n"), Logical: []CandidateProviderLogicalFrameV1{{Kind: CandidateLogicalSSE, Payload: []byte(`{"type":"response.completed"}`)}}}},
		{protocol: "websocket", response: CandidateProviderUpstreamResponseV1{StatusCode: 101, Logical: []CandidateProviderLogicalFrameV1{{Kind: CandidateLogicalWebSocketText, Payload: []byte(`{"type":"response.completed"}`)}, {Kind: CandidateLogicalWebSocketClose, Payload: []byte("1000 normal")}}}},
	}
	for _, test := range tests {
		t.Run(test.protocol, func(t *testing.T) {
			store, sourceDigest := newCandidateProviderBrokerStore(t)
			scanner, err := NewCredentialEchoScanner(bytes.Repeat([]byte{0x53}, 32), "run-broker", [][]byte{[]byte("provider-secret-token")})
			if err != nil {
				t.Fatal(err)
			}
			transport := &recordingCandidateProviderTransport{store: store, runID: "run-broker", response: test.response}
			broker := newCandidateProviderBrokerForTest(t, store, sourceDigest, scanner, transport, nil)
			granted, err := broker.Acquire(context.Background(), candidateAcquireForTest(test.protocol, digestBytes([]byte("request"))))
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := broker.Exchange(context.Background(), granted.Capability, CandidateProviderExchange{Body: []byte("request")})
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Outcome != CandidateProviderDelivered || receipt.Response == nil || receipt.Response.StatusCode != test.response.StatusCode {
				t.Fatalf("receipt = %#v", receipt)
			}
		})
	}
}

type recordingCandidateProviderTransport struct {
	store              *CandidateBrokerStore
	runID              string
	response           CandidateProviderUpstreamResponseV1
	calls              int
	consumedBeforeCall bool
}

func (transport *recordingCandidateProviderTransport) Exchange(_ context.Context, request CandidateProviderUpstreamRequestV1) (CandidateProviderUpstreamResponseV1, error) {
	transport.calls++
	kinds := candidateBrokerJournalKinds(transport.store, transport.runID)
	transport.consumedBeforeCall = len(kinds) >= 2 && kinds[len(kinds)-1] == "capability_consumed"
	if request.Bearer != "provider-secret-token" || request.Origin != "https://provider.invalid" {
		return CandidateProviderUpstreamResponseV1{}, errors.New("broker omitted provider authority")
	}
	if request.Headers.Get("Content-Type") != "application/json" || request.Headers.Get("Authorization") != "Bearer provider-secret-token" {
		return CandidateProviderUpstreamResponseV1{}, errors.New("broker did not combine signed candidate headers with private provider authority")
	}
	return transport.response, nil
}

func candidateAcquireForTest(protocol string, bodyDigest string) CandidateProviderCapabilityAcquireV1 {
	return CandidateProviderCapabilityAcquireV1{
		SchemaVersion:     1,
		RequestID:         "request-1",
		Protocol:          protocol,
		OriginID:          "codex-official",
		Method:            http.MethodPost,
		Route:             "/responses",
		Headers:           http.Header{"Content-Type": {"application/json"}},
		BodyDigest:        bodyDigest,
		BodyLimit:         1024,
		ResponseLimit:     4096,
		DeadlineUnix:      time.Now().Add(time.Minute).Unix(),
		AccountKey:        providerCodex.AccountKey("acct-a"),
		Revision:          providerCodex.Revision("rev-a"),
		RouteBudgetDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
}

func newCandidateProviderBrokerForTest(t *testing.T, store *CandidateBrokerStore, sourceDigest string, scanner *CredentialEchoScanner, transport CandidateProviderTransport, hook func(string) error) *CandidateBroker {
	t.Helper()
	broker, err := NewCandidateBroker(CandidateBrokerConfig{
		RunID: "run-broker", SourceDigest: sourceDigest, Store: store, Scanner: scanner, Transport: transport,
		Provider:      CandidateProviderAuthority{OriginID: "codex-official", Origin: "https://provider.invalid", Bearer: "provider-secret-token"},
		CapabilityKey: bytes.Repeat([]byte{0x54}, 32), Random: bytes.NewReader(bytes.Repeat([]byte{0x55}, 4096)), Hook: hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

func newCandidateProviderBrokerStore(t *testing.T) (*CandidateBrokerStore, string) {
	t.Helper()
	fsys := fsutil.NewMemFS()
	if err := fsutil.EnsureSecureDirectory(fsys, "/candidate-provider-broker"); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/candidate-provider-broker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	store, err := OpenCandidateBrokerStore(context.Background(), fsys, directory, NewAuthorityObjectPublisher(fsys, bytes.NewReader(bytes.Repeat([]byte{0x56}, 16384))), bytes.Repeat([]byte{0x57}, 32), CandidateBrokerCaps{Runs: 3, RecordsPerRun: 32})
	if err != nil {
		t.Fatal(err)
	}
	source := CandidateValidationSourceV1{SchemaVersion: 1, RunID: "run-broker", Kind: CandidateSourceIngress, CatalogueDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	identity, err := store.PublishSource(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	return store, identity.Digest
}

func reopenCandidateProviderBrokerStore(t *testing.T, prior *CandidateBrokerStore) *CandidateBrokerStore {
	t.Helper()
	store, err := OpenCandidateBrokerStore(context.Background(), prior.inspector, prior.directory, prior.publisher, prior.key[:], prior.caps)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func candidateBrokerJournalKinds(store *CandidateBrokerStore, runID string) []string {
	records := store.JournalRecords(runID)
	kinds := make([]string, len(records))
	for index, record := range records {
		kinds[index] = record.Kind
	}
	return kinds
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
