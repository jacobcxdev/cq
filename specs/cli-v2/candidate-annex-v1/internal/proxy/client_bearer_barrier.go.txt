package proxy

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"
)

var ErrClientBearerBarrier = errors.New("client bearer barrier unavailable")

type ClientRequestSenderV1 struct {
	SenderID          string   `json:"sender_id"`
	AdapterID         string   `json:"adapter_id"`
	Stateful          bool     `json:"stateful"`
	CredentialDomains []string `json:"credential_domains"`
	Transports        []string `json:"transports"`
	HookSupported     bool     `json:"hook_supported"`
}

type ClientSenderRegistryV1 struct {
	SchemaVersion int                     `json:"schema_version"`
	Revision      uint64                  `json:"revision"`
	Senders       []ClientRequestSenderV1 `json:"senders"`
}

type ClientSenderBarrierEvidenceV1 struct {
	SenderID                      string `json:"sender_id"`
	CredentialDomain              string `json:"credential_domain"`
	Transport                     string `json:"transport"`
	ForeignBindApplicationBytes   uint64 `json:"foreign_bind_application_bytes"`
	ReleaseWindowApplicationBytes uint64 `json:"release_window_application_bytes"`
}

type ClientBearerBarrierReceiptV1 struct {
	SchemaVersion   int       `json:"schema_version"`
	RegistryDigest  string    `json:"registry_digest"`
	EvidenceDigest  string    `json:"evidence_digest"`
	IssuedAt        time.Time `json:"issued_at"`
	NotAfter        time.Time `json:"not_after"`
	IssuerPublicKey string    `json:"issuer_public_key"`
	Signature       string    `json:"signature"`
	Digest          string    `json:"digest"`
}

type ClientStopProofV1 struct {
	SchemaVersion         int       `json:"schema_version"`
	OperationID           string    `json:"operation_id,omitempty"`
	RegistryDigest        string    `json:"registry_digest"`
	EvidenceDigest        string    `json:"evidence_digest"`
	ZeroActivePermits     bool      `json:"zero_active_permits"`
	ZeroActiveConnections bool      `json:"zero_active_connections"`
	ZeroAdmittedWork      bool      `json:"zero_admitted_work"`
	ObservedAt            time.Time `json:"observed_at"`
	ValidUntil            time.Time `json:"valid_until"`
	MAC                   string    `json:"mac"`
}

func SignClientBearerBarrier(registry ClientSenderRegistryV1, evidence []ClientSenderBarrierEvidenceV1, issuedAt, notAfter time.Time, privateKey ed25519.PrivateKey) (ClientBearerBarrierReceiptV1, error) {
	registry, evidence, err := validateClientBarrierInputs(registry, evidence)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize || !issuedAt.Equal(issuedAt.UTC()) || !notAfter.Equal(notAfter.UTC()) || !issuedAt.Before(notAfter) || notAfter.After(issuedAt.Add(24*time.Hour)) {
		return ClientBearerBarrierReceiptV1{}, ErrClientBearerBarrier
	}
	registryDigest := clientBarrierDigest("cq/client-sender-registry/v1\x00", registry)
	evidenceDigest := clientBarrierDigest("cq/client-bearer-barrier-evidence/v1\x00", evidence)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	receipt := ClientBearerBarrierReceiptV1{SchemaVersion: 1, RegistryDigest: registryDigest, EvidenceDigest: evidenceDigest, IssuedAt: issuedAt, NotAfter: notAfter, IssuerPublicKey: hex.EncodeToString(publicKey)}
	message := clientBarrierCanonical(receipt, true)
	receipt.Signature = hex.EncodeToString(ed25519.Sign(privateKey, append([]byte("cq/client-bearer-barrier-signature/v1\x00"), message...)))
	receipt.Digest = clientBarrierDigest("cq/client-bearer-barrier/v1\x00", receipt)
	return receipt, nil
}

func SignClientStopProof(proof ClientStopProofV1, key []byte) (ClientStopProofV1, error) {
	if len(key) != sha256.Size || !lowerHexDigest(proof.RegistryDigest) || !lowerHexDigest(proof.EvidenceDigest) || !proof.ZeroActivePermits || !proof.ZeroActiveConnections || !proof.ZeroAdmittedWork || !proof.ObservedAt.Equal(proof.ObservedAt.UTC()) || !proof.ValidUntil.Equal(proof.ValidUntil.UTC()) || !proof.ObservedAt.Before(proof.ValidUntil) || proof.ValidUntil.After(proof.ObservedAt.Add(5*time.Second)) {
		return ClientStopProofV1{}, ErrClientBearerBarrier
	}
	proof.SchemaVersion = 1
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/client-stop-proof/v1\x00"))
	_, _ = mac.Write(clientBarrierCanonical(proof, false))
	proof.MAC = hex.EncodeToString(mac.Sum(nil))
	return proof, nil
}

func ValidateClientBearerBarrier(registry ClientSenderRegistryV1, receipt ClientBearerBarrierReceiptV1, stop ClientStopProofV1, now time.Time, publicKey ed25519.PublicKey, stopKey []byte) error {
	canonicalRegistry, _, err := validateClientBarrierInputs(registry, makeClientBarrierShape(registry))
	if err != nil || len(publicKey) != ed25519.PublicKeySize || len(stopKey) != sha256.Size || !now.Equal(now.UTC()) || now.Before(receipt.IssuedAt) || !now.Before(receipt.NotAfter) {
		return ErrClientBearerBarrier
	}
	if receipt.SchemaVersion != 1 || receipt.RegistryDigest != clientBarrierDigest("cq/client-sender-registry/v1\x00", canonicalRegistry) || receipt.IssuerPublicKey != hex.EncodeToString(publicKey) || receipt.Digest != clientBarrierDigest("cq/client-bearer-barrier/v1\x00", receipt) {
		return ErrClientBearerBarrier
	}
	signature, err := hex.DecodeString(receipt.Signature)
	if err != nil || !ed25519.Verify(publicKey, append([]byte("cq/client-bearer-barrier-signature/v1\x00"), clientBarrierCanonical(receipt, true)...), signature) {
		return ErrClientBearerBarrier
	}
	expectedStop, err := SignClientStopProof(ClientStopProofV1{SchemaVersion: stop.SchemaVersion, OperationID: stop.OperationID, RegistryDigest: stop.RegistryDigest, EvidenceDigest: stop.EvidenceDigest, ZeroActivePermits: stop.ZeroActivePermits, ZeroActiveConnections: stop.ZeroActiveConnections, ZeroAdmittedWork: stop.ZeroAdmittedWork, ObservedAt: stop.ObservedAt, ValidUntil: stop.ValidUntil}, stopKey)
	if err != nil || !hmac.Equal([]byte(expectedStop.MAC), []byte(stop.MAC)) || stop.RegistryDigest != receipt.RegistryDigest || stop.EvidenceDigest != receipt.EvidenceDigest || now.Before(stop.ObservedAt) || !now.Before(stop.ValidUntil) {
		return ErrClientBearerBarrier
	}
	return nil
}

func validateClientBarrierInputs(registry ClientSenderRegistryV1, evidence []ClientSenderBarrierEvidenceV1) (ClientSenderRegistryV1, []ClientSenderBarrierEvidenceV1, error) {
	if registry.SchemaVersion != 1 || registry.Revision == 0 || len(registry.Senders) == 0 || len(registry.Senders) > 17 {
		return ClientSenderRegistryV1{}, nil, ErrClientBearerBarrier
	}
	seenSenders := make(map[string]struct{}, len(registry.Senders))
	hasCQ := false
	for index := range registry.Senders {
		sender := &registry.Senders[index]
		sort.Strings(sender.CredentialDomains)
		sort.Strings(sender.Transports)
		if sender.SenderID == "" || sender.AdapterID == "" || !sender.HookSupported || len(sender.Transports) == 0 || !uniqueClosedStrings(sender.CredentialDomains, []string{"claude_bearer", "codex_bearer", "cq_local_token"}, true) || !uniqueClosedStrings(sender.Transports, []string{"compact", "http", "retained", "websocket"}, false) {
			return ClientSenderRegistryV1{}, nil, ErrClientBearerBarrier
		}
		if _, duplicate := seenSenders[sender.SenderID]; duplicate {
			return ClientSenderRegistryV1{}, nil, ErrClientBearerBarrier
		}
		seenSenders[sender.SenderID] = struct{}{}
		hasCQ = hasCQ || sender.AdapterID == "cq_config_read_per_call_v1" && !sender.Stateful && slices.Equal(sender.CredentialDomains, []string{"cq_local_token"})
	}
	if !hasCQ {
		return ClientSenderRegistryV1{}, nil, ErrClientBearerBarrier
	}
	sort.Slice(registry.Senders, func(i, j int) bool { return registry.Senders[i].SenderID < registry.Senders[j].SenderID })
	expected := make(map[string]struct{})
	for _, sender := range registry.Senders {
		for _, domain := range sender.CredentialDomains {
			for _, transport := range sender.Transports {
				expected[sender.SenderID+"\x00"+domain+"\x00"+transport] = struct{}{}
			}
		}
	}
	seenEvidence := make(map[string]struct{}, len(evidence))
	for _, item := range evidence {
		key := item.SenderID + "\x00" + item.CredentialDomain + "\x00" + item.Transport
		if _, ok := expected[key]; !ok || item.ForeignBindApplicationBytes != 0 || item.ReleaseWindowApplicationBytes != 0 {
			return ClientSenderRegistryV1{}, nil, ErrClientBearerBarrier
		}
		if _, duplicate := seenEvidence[key]; duplicate {
			return ClientSenderRegistryV1{}, nil, ErrClientBearerBarrier
		}
		seenEvidence[key] = struct{}{}
	}
	if len(seenEvidence) != len(expected) {
		return ClientSenderRegistryV1{}, nil, ErrClientBearerBarrier
	}
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].SenderID != evidence[j].SenderID {
			return evidence[i].SenderID < evidence[j].SenderID
		}
		if evidence[i].CredentialDomain != evidence[j].CredentialDomain {
			return evidence[i].CredentialDomain < evidence[j].CredentialDomain
		}
		return evidence[i].Transport < evidence[j].Transport
	})
	return registry, evidence, nil
}

func makeClientBarrierShape(registry ClientSenderRegistryV1) []ClientSenderBarrierEvidenceV1 {
	var evidence []ClientSenderBarrierEvidenceV1
	for _, sender := range registry.Senders {
		for _, domain := range sender.CredentialDomains {
			for _, transport := range sender.Transports {
				evidence = append(evidence, ClientSenderBarrierEvidenceV1{SenderID: sender.SenderID, CredentialDomain: domain, Transport: transport})
			}
		}
	}
	return evidence
}

func uniqueClosedStrings(values, allowed []string, allowEmpty bool) bool {
	if !allowEmpty && len(values) == 0 {
		return false
	}
	for index, value := range values {
		if index > 0 && values[index-1] >= value {
			return false
		}
		if !slices.Contains(allowed, value) {
			return false
		}
	}
	return true
}

func clientBarrierCanonical(value any, clearSignature bool) []byte {
	switch typed := value.(type) {
	case ClientBearerBarrierReceiptV1:
		typed.Digest = ""
		if clearSignature {
			typed.Signature = ""
		}
		value = typed
	case ClientStopProofV1:
		typed.MAC = ""
		value = typed
	}
	body, _ := json.Marshal(value)
	return body
}

func clientBarrierDigest(domain string, value any) string {
	body := clientBarrierCanonical(value, false)
	digest := sha256.Sum256(append([]byte(domain), body...))
	return hex.EncodeToString(digest[:])
}
