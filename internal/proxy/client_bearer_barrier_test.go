package proxy

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func TestClientBearerBarrierRequiresCompleteZeroByteSenderSet(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(strings.NewReader(strings.Repeat("k", ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	registry := ClientSenderRegistryV1{
		SchemaVersion: 1,
		Revision:      4,
		Senders: []ClientRequestSenderV1{
			{SenderID: "codex-cli", AdapterID: "codex_cli_v1", Stateful: true, CredentialDomains: []string{"codex_bearer"}, Transports: []string{"http", "websocket"}, HookSupported: true},
			{SenderID: "cq-config", AdapterID: "cq_config_read_per_call_v1", CredentialDomains: []string{"cq_local_token"}, Transports: []string{"http"}, HookSupported: true},
		},
	}
	evidence := []ClientSenderBarrierEvidenceV1{
		{SenderID: "codex-cli", CredentialDomain: "codex_bearer", Transport: "http", ForeignBindApplicationBytes: 0, ReleaseWindowApplicationBytes: 0},
		{SenderID: "codex-cli", CredentialDomain: "codex_bearer", Transport: "websocket", ForeignBindApplicationBytes: 0, ReleaseWindowApplicationBytes: 0},
		{SenderID: "cq-config", CredentialDomain: "cq_local_token", Transport: "http", ForeignBindApplicationBytes: 0, ReleaseWindowApplicationBytes: 0},
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	receipt, err := SignClientBearerBarrier(registry, evidence, now, now.Add(time.Hour), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	stopKey := []byte("01234567890123456789012345678901")
	stop, err := SignClientStopProof(ClientStopProofV1{RegistryDigest: receipt.RegistryDigest, EvidenceDigest: receipt.EvidenceDigest, ZeroActivePermits: true, ZeroActiveConnections: true, ZeroAdmittedWork: true, ObservedAt: now, ValidUntil: now.Add(5 * time.Second)}, stopKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateClientBearerBarrier(registry, receipt, stop, now.Add(time.Second), publicKey, stopKey); err != nil {
		t.Fatal(err)
	}

	missing := registry
	missing.Senders = missing.Senders[:1]
	if _, err := SignClientBearerBarrier(missing, evidence[:2], now, now.Add(time.Hour), privateKey); err == nil {
		t.Fatal("missing stateless CQ sender accepted")
	}
	tampered := evidence
	tampered[0].ForeignBindApplicationBytes = 1
	if _, err := SignClientBearerBarrier(registry, tampered, now, now.Add(time.Hour), privateKey); err == nil {
		t.Fatal("foreign-bind application byte accepted")
	}
}

func TestClientBearerBarrierRejectsMissingHookAndStaleStopProof(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(strings.NewReader(strings.Repeat("s", ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	registry := ClientSenderRegistryV1{SchemaVersion: 1, Revision: 1, Senders: []ClientRequestSenderV1{
		{SenderID: "cq-config", AdapterID: "cq_config_read_per_call_v1", CredentialDomains: []string{"cq_local_token"}, Transports: []string{"http"}},
	}}
	evidence := []ClientSenderBarrierEvidenceV1{{SenderID: "cq-config", CredentialDomain: "cq_local_token", Transport: "http"}}
	if _, err := SignClientBearerBarrier(registry, evidence, now, now.Add(time.Hour), privateKey); err == nil {
		t.Fatal("missing sender hook accepted")
	}
}
