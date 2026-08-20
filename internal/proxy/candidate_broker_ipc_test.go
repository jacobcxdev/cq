package proxy

import (
	"bytes"
	"testing"
)

func TestBrokerIPCStrictlySeparatesAcquireAndExchangeWithoutProviderAuthority(t *testing.T) {
	request := CandidateBrokerIPCRequestV1{SchemaVersion: 1, Kind: CandidateBrokerIPCAcquire, Acquire: ptrCandidateAcquire(candidateAcquireForTest("http", digestBytes([]byte("request"))))}
	body, err := EncodeCandidateBrokerIPCRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("provider-secret-token")) || bytes.Contains(body, []byte("https://provider.invalid")) {
		t.Fatalf("IPC exposed provider authority: %s", body)
	}
	decoded, err := DecodeCandidateBrokerIPCRequest(body)
	if err != nil || decoded.Acquire == nil || decoded.Acquire.RequestID != "request-1" {
		t.Fatalf("Decode = %#v, %v", decoded, err)
	}

	malicious := []byte(`{"schema_version":1,"kind":"acquire","acquire":{},"bearer":"secret"}`)
	if _, err := DecodeCandidateBrokerIPCRequest(malicious); err == nil {
		t.Fatal("IPC accepted provider bearer field")
	}
	ambiguous := CandidateBrokerIPCRequestV1{
		SchemaVersion: 1,
		Kind:          CandidateBrokerIPCAcquire,
		Acquire:       request.Acquire,
		Capability:    &CandidateProviderRequestCapabilityV1{},
		Exchange:      &CandidateProviderExchange{},
	}
	if _, err := EncodeCandidateBrokerIPCRequest(ambiguous); err == nil {
		t.Fatal("IPC accepted acquire/exchange union ambiguity")
	}
}

func ptrCandidateAcquire(value CandidateProviderCapabilityAcquireV1) *CandidateProviderCapabilityAcquireV1 {
	return &value
}
