package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

var (
	ErrCandidateCapabilityConsumed      = errors.New("candidate provider capability already consumed")
	ErrCandidateCapabilityIndeterminate = errors.New("candidate provider capability outcome indeterminate")
)

type CandidateProviderBroker interface {
	Acquire(context.Context, CandidateProviderCapabilityAcquireV1) (CandidateProviderCapabilityGrantedV1, error)
	Exchange(context.Context, CandidateProviderRequestCapabilityV1, CandidateProviderExchange) (CandidateProviderTerminalReceiptV1, error)
}

type CandidateProviderCapabilityAcquireV1 struct {
	SchemaVersion     int                      `json:"schema_version"`
	RequestID         string                   `json:"request_id"`
	Protocol          string                   `json:"protocol"`
	OriginID          string                   `json:"origin_id"`
	Method            string                   `json:"method"`
	Route             string                   `json:"route"`
	RawQuery          string                   `json:"raw_query,omitempty"`
	Headers           http.Header              `json:"headers,omitempty"`
	BodyDigest        string                   `json:"body_digest"`
	BodyLimit         int64                    `json:"body_limit"`
	ResponseLimit     int64                    `json:"response_limit"`
	DeadlineUnix      int64                    `json:"deadline_unix"`
	AccountKey        providerCodex.AccountKey `json:"account_key"`
	Revision          providerCodex.Revision   `json:"revision"`
	RouteBudgetDigest string                   `json:"route_budget_digest"`
}

type CandidateProviderRequestCapabilityV1 struct {
	SchemaVersion     int                      `json:"schema_version"`
	RequestID         string                   `json:"request_id"`
	Protocol          string                   `json:"protocol"`
	OriginID          string                   `json:"origin_id"`
	Method            string                   `json:"method"`
	Route             string                   `json:"route"`
	RawQuery          string                   `json:"raw_query,omitempty"`
	Headers           http.Header              `json:"headers,omitempty"`
	HeaderDigest      string                   `json:"header_digest"`
	BodyDigest        string                   `json:"body_digest"`
	BodyLimit         int64                    `json:"body_limit"`
	ResponseLimit     int64                    `json:"response_limit"`
	DeadlineUnix      int64                    `json:"deadline_unix"`
	AccountKey        providerCodex.AccountKey `json:"account_key"`
	Revision          providerCodex.Revision   `json:"revision"`
	RouteBudgetDigest string                   `json:"route_budget_digest"`
	Nonce             string                   `json:"nonce"`
	CapabilityDigest  string                   `json:"capability_digest"`
	Signature         string                   `json:"signature"`
}

type CandidateProviderCapabilityGrantedV1 struct {
	SchemaVersion int                                  `json:"schema_version"`
	Capability    CandidateProviderRequestCapabilityV1 `json:"capability"`
}

type CandidateProviderExchange struct {
	Body []byte `json:"body"`
}

type CandidateProviderLogicalKind string

const (
	CandidateLogicalSSE                CandidateProviderLogicalKind = "sse"
	CandidateLogicalWebSocketText      CandidateProviderLogicalKind = "websocket_text"
	CandidateLogicalWebSocketBinary    CandidateProviderLogicalKind = "websocket_binary"
	CandidateLogicalWebSocketPing      CandidateProviderLogicalKind = "websocket_ping"
	CandidateLogicalWebSocketPong      CandidateProviderLogicalKind = "websocket_pong"
	CandidateLogicalWebSocketClose     CandidateProviderLogicalKind = "websocket_close"
	CandidateLogicalWebSocketHandshake CandidateProviderLogicalKind = "websocket_handshake_error"
)

type CandidateProviderLogicalFrameV1 struct {
	Kind    CandidateProviderLogicalKind `json:"kind"`
	Payload []byte                       `json:"payload"`
}

type CandidateProviderUpstreamRequestV1 struct {
	Origin   string
	Bearer   string
	Method   string
	Route    string
	RawQuery string
	Headers  http.Header
	Body     []byte
	Protocol string
}

type CandidateProviderUpstreamResponseV1 struct {
	StatusCode  int
	Headers     http.Header
	EncodedBody []byte
	Encoding    string
	Logical     []CandidateProviderLogicalFrameV1
}

type CandidateProviderTransport interface {
	Exchange(context.Context, CandidateProviderUpstreamRequestV1) (CandidateProviderUpstreamResponseV1, error)
}

type CandidateProviderAuthority struct {
	OriginID string
	Origin   string
	Bearer   string
}

type CandidateProviderOutcome string

const (
	CandidateProviderDelivered     CandidateProviderOutcome = "delivered"
	CandidateProviderEchoBlocked   CandidateProviderOutcome = "credential_echo_blocked"
	CandidateProviderIndeterminate CandidateProviderOutcome = "indeterminate"
	CandidateProviderFailed        CandidateProviderOutcome = "provider_failed"
)

type CandidateVisibleProviderResponseV1 struct {
	StatusCode  int                               `json:"status_code"`
	Headers     http.Header                       `json:"headers,omitempty"`
	EncodedBody []byte                            `json:"encoded_body,omitempty"`
	Encoding    string                            `json:"encoding,omitempty"`
	Logical     []CandidateProviderLogicalFrameV1 `json:"logical,omitempty"`
}

type CandidateProviderTerminalReceiptV1 struct {
	SchemaVersion    int                                 `json:"schema_version"`
	CapabilityDigest string                              `json:"capability_digest"`
	Outcome          CandidateProviderOutcome            `json:"outcome"`
	ResponseDigest   string                              `json:"response_digest,omitempty"`
	ScanDigest       string                              `json:"scan_digest"`
	Response         *CandidateVisibleProviderResponseV1 `json:"response,omitempty"`
}

type CandidateBrokerConfig struct {
	RunID         string
	SourceDigest  string
	Store         *CandidateBrokerStore
	Scanner       *CredentialEchoScanner
	Transport     CandidateProviderTransport
	Provider      CandidateProviderAuthority
	CapabilityKey []byte
	Random        io.Reader
	Hook          func(string) error
}

type candidateCapabilityState uint8

const (
	candidateCapabilityIssued candidateCapabilityState = iota + 1
	candidateCapabilityConsumed
	candidateCapabilityTerminal
)

// CandidateBroker is the only component holding provider authority. Candidate
// request and response types contain neither the bearer nor the provider URL.
type CandidateBroker struct {
	mu            sync.Mutex
	runID         string
	sourceDigest  string
	store         *CandidateBrokerStore
	scanner       *CredentialEchoScanner
	transport     CandidateProviderTransport
	provider      CandidateProviderAuthority
	capabilityKey [sha256.Size]byte
	random        io.Reader
	hook          func(string) error
	states        map[string]candidateCapabilityState
}

func NewCandidateBroker(config CandidateBrokerConfig) (*CandidateBroker, error) {
	if config.RunID == "" || !lowerHexDigest(config.SourceDigest) || config.Store == nil || config.Scanner == nil || config.Transport == nil ||
		config.Provider.OriginID == "" || config.Provider.Origin == "" || config.Provider.Bearer == "" || len(config.CapabilityKey) != sha256.Size || config.Random == nil {
		return nil, errors.New("candidate provider broker authority unavailable")
	}
	parsedOrigin, err := url.Parse(config.Provider.Origin)
	if err != nil || parsedOrigin.Scheme != "https" || parsedOrigin.Host == "" || parsedOrigin.User != nil || parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" {
		return nil, errors.New("candidate provider origin invalid")
	}
	broker := &CandidateBroker{
		runID: config.RunID, sourceDigest: config.SourceDigest, store: config.Store, scanner: config.Scanner,
		transport: config.Transport, provider: config.Provider, random: config.Random, hook: config.Hook,
		states: make(map[string]candidateCapabilityState),
	}
	copy(broker.capabilityKey[:], config.CapabilityKey)
	if err := broker.recoverStates(); err != nil {
		return nil, err
	}
	return broker, nil
}

func (b *CandidateBroker) Acquire(ctx context.Context, acquire CandidateProviderCapabilityAcquireV1) (CandidateProviderCapabilityGrantedV1, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return CandidateProviderCapabilityGrantedV1{}, err
	}
	if err := b.validateAcquire(acquire); err != nil {
		return CandidateProviderCapabilityGrantedV1{}, err
	}
	nonceBytes := make([]byte, 16)
	if _, err := io.ReadFull(b.random, nonceBytes); err != nil {
		return CandidateProviderCapabilityGrantedV1{}, err
	}
	headerDigest, err := candidateHeaderDigest(acquire.Headers)
	if err != nil {
		return CandidateProviderCapabilityGrantedV1{}, err
	}
	capability := CandidateProviderRequestCapabilityV1{
		SchemaVersion: 1, RequestID: acquire.RequestID, Protocol: acquire.Protocol, OriginID: acquire.OriginID,
		Method: acquire.Method, Route: acquire.Route, RawQuery: acquire.RawQuery, Headers: acquire.Headers.Clone(), HeaderDigest: headerDigest,
		BodyDigest: acquire.BodyDigest, BodyLimit: acquire.BodyLimit, ResponseLimit: acquire.ResponseLimit,
		DeadlineUnix: acquire.DeadlineUnix, AccountKey: acquire.AccountKey, Revision: acquire.Revision,
		RouteBudgetDigest: acquire.RouteBudgetDigest, Nonce: hex.EncodeToString(nonceBytes),
	}
	capability.Signature = b.signCapability(capability)
	capability.CapabilityDigest, err = candidateCapabilityDigest(capability)
	if err != nil {
		return CandidateProviderCapabilityGrantedV1{}, err
	}
	if _, exists := b.states[capability.CapabilityDigest]; exists {
		return CandidateProviderCapabilityGrantedV1{}, errors.New("candidate capability collision")
	}
	if _, err := b.store.AppendJournal(ctx, CandidateBrokerRecordV1{
		SchemaVersion: 1, RunID: b.runID, SourceDigest: b.sourceDigest, CapabilityDigest: capability.CapabilityDigest,
		Kind: "capability_issued", PayloadDigest: capability.CapabilityDigest,
	}); err != nil {
		return CandidateProviderCapabilityGrantedV1{}, err
	}
	b.states[capability.CapabilityDigest] = candidateCapabilityIssued
	if err := b.callHook("issued_durable"); err != nil {
		return CandidateProviderCapabilityGrantedV1{}, err
	}
	return CandidateProviderCapabilityGrantedV1{SchemaVersion: 1, Capability: capability}, nil
}

func (b *CandidateBroker) Exchange(ctx context.Context, capability CandidateProviderRequestCapabilityV1, exchange CandidateProviderExchange) (CandidateProviderTerminalReceiptV1, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}
	if err := b.verifyCapability(capability); err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}
	switch b.states[capability.CapabilityDigest] {
	case candidateCapabilityConsumed:
		return CandidateProviderTerminalReceiptV1{}, ErrCandidateCapabilityIndeterminate
	case candidateCapabilityTerminal:
		return CandidateProviderTerminalReceiptV1{}, ErrCandidateCapabilityConsumed
	case candidateCapabilityIssued:
	default:
		return CandidateProviderTerminalReceiptV1{}, errors.New("candidate capability was not issued")
	}
	if int64(len(exchange.Body)) > capability.BodyLimit || digestBytesForCandidateBroker(exchange.Body) != capability.BodyDigest {
		return CandidateProviderTerminalReceiptV1{}, errors.New("candidate provider request body mismatch")
	}
	if _, err := b.store.AppendJournal(ctx, CandidateBrokerRecordV1{
		SchemaVersion: 1, RunID: b.runID, SourceDigest: b.sourceDigest, CapabilityDigest: capability.CapabilityDigest,
		Kind: "capability_consumed", PayloadDigest: capability.CapabilityDigest,
	}); err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}
	b.states[capability.CapabilityDigest] = candidateCapabilityConsumed
	if err := b.callHook("consumed_durable"); err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}

	providerHeaders := capability.Headers.Clone()
	providerHeaders.Set("Authorization", "Bearer "+b.provider.Bearer)
	response, providerErr := b.transport.Exchange(ctx, CandidateProviderUpstreamRequestV1{
		Origin: b.provider.Origin, Bearer: b.provider.Bearer, Method: capability.Method, Route: capability.Route,
		RawQuery: capability.RawQuery, Headers: providerHeaders, Body: append([]byte(nil), exchange.Body...), Protocol: capability.Protocol,
	})
	receipt := b.scanResponse(capability, response, providerErr)
	echoCount := uint64(0)
	if receipt.Outcome == CandidateProviderEchoBlocked {
		echoCount = 1
	}
	if _, err := b.store.PublishScanEvidence(ctx, CandidateCredentialEchoScanEvidenceV1{
		SchemaVersion: 1, RunID: b.runID, SourceDigest: b.sourceDigest, ScanDigest: receipt.ScanDigest, EchoCount: echoCount,
	}); err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}
	if err := b.callHook("scan_evidence_durable"); err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}
	terminalBody, err := json.Marshal(receipt)
	if err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}
	terminalDigest, err := FramedSHA256Hex("cq/candidate-provider-terminal/v1\x00", terminalBody)
	if err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}
	if _, err := b.store.AppendJournal(ctx, CandidateBrokerRecordV1{
		SchemaVersion: 1, RunID: b.runID, SourceDigest: b.sourceDigest, CapabilityDigest: capability.CapabilityDigest,
		Kind: "capability_terminal", PayloadDigest: terminalDigest,
	}); err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}
	b.states[capability.CapabilityDigest] = candidateCapabilityTerminal
	if err := b.callHook("terminal_durable"); err != nil {
		return CandidateProviderTerminalReceiptV1{}, err
	}
	return receipt, nil
}

func (b *CandidateBroker) scanResponse(capability CandidateProviderRequestCapabilityV1, response CandidateProviderUpstreamResponseV1, providerErr error) CandidateProviderTerminalReceiptV1 {
	receipt := CandidateProviderTerminalReceiptV1{SchemaVersion: 1, CapabilityDigest: capability.CapabilityDigest}
	if providerErr != nil {
		evidence := b.scanner.ScanRepresentation(nil, "identity")
		receipt.Outcome = CandidateProviderFailed
		receipt.ScanDigest = evidence.EvidenceDigest
		return receipt
	}
	total := int64(len(response.EncodedBody))
	for _, frame := range response.Logical {
		total += int64(len(frame.Payload))
	}
	if total > capability.ResponseLimit || response.StatusCode < 100 || response.StatusCode > 599 {
		evidence := b.scanner.evidence(CredentialEchoIndeterminate, uint64(max(total, 0)))
		receipt.Outcome = CandidateProviderIndeterminate
		receipt.ScanDigest = evidence.EvidenceDigest
		return receipt
	}
	evidence := b.scanner.ScanHeaders(response.Headers)
	if evidence.Outcome == CredentialEchoClear {
		encoding := response.Encoding
		if encoding == "" {
			encoding = strings.ToLower(strings.TrimSpace(response.Headers.Get("Content-Encoding")))
		}
		evidence = b.scanner.ScanRepresentation(response.EncodedBody, encoding)
	}
	if evidence.Outcome == CredentialEchoClear && len(response.Logical) != 0 {
		stream := b.scanner.NewStream()
		for index, frame := range response.Logical {
			if !validCandidateLogicalKind(capability.Protocol, frame.Kind) {
				evidence = b.scanner.evidence(CredentialEchoIndeterminate, evidence.InspectedByteCount)
				break
			}
			_, evidence, _ = stream.Push(frame.Payload, index == len(response.Logical)-1)
			if evidence.Outcome != CredentialEchoClear {
				break
			}
		}
	}
	receipt.ScanDigest = evidence.EvidenceDigest
	if evidence.Outcome != CredentialEchoClear {
		if evidence.Outcome == CredentialEchoMatch {
			receipt.Outcome = CandidateProviderEchoBlocked
		} else {
			receipt.Outcome = CandidateProviderIndeterminate
		}
		return receipt
	}
	visible := &CandidateVisibleProviderResponseV1{
		StatusCode: response.StatusCode, Headers: response.Headers.Clone(), EncodedBody: append([]byte(nil), response.EncodedBody...), Encoding: response.Encoding,
		Logical: cloneCandidateLogicalFrames(response.Logical),
	}
	visibleBody, err := json.Marshal(visible)
	if err != nil {
		receipt.Outcome = CandidateProviderIndeterminate
		return receipt
	}
	receipt.ResponseDigest, err = FramedSHA256Hex("cq/candidate-visible-provider-response/v1\x00", visibleBody)
	if err != nil {
		receipt.Outcome = CandidateProviderIndeterminate
		return receipt
	}
	receipt.Outcome = CandidateProviderDelivered
	receipt.Response = visible
	return receipt
}

func (b *CandidateBroker) validateAcquire(acquire CandidateProviderCapabilityAcquireV1) error {
	if acquire.SchemaVersion != 1 || acquire.RequestID == "" || validateAuthorityEntryName("request-"+acquire.RequestID) != nil ||
		(acquire.Protocol != "http" && acquire.Protocol != "sse" && acquire.Protocol != "websocket") || acquire.OriginID != b.provider.OriginID ||
		acquire.Method == "" || !strings.HasPrefix(acquire.Route, "/") || strings.HasPrefix(acquire.Route, "//") ||
		acquire.BodyLimit <= 0 || acquire.ResponseLimit <= 0 || acquire.BodyLimit > 16<<20 || acquire.ResponseLimit > credentialEchoRepresentationLimit ||
		!lowerHexDigest(acquire.BodyDigest) || !lowerHexDigest(acquire.RouteBudgetDigest) || acquire.AccountKey == "" || acquire.Revision == "" ||
		acquire.DeadlineUnix <= time.Now().Unix() {
		return errors.New("invalid candidate provider capability request")
	}
	if strings.ContainsAny(acquire.Method, " \t\r\n") {
		return errors.New("invalid candidate provider method")
	}
	if _, err := url.ParseQuery(acquire.RawQuery); err != nil {
		return errors.New("invalid candidate provider query")
	}
	for name := range acquire.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "Authorization" || canonical == "X-Api-Key" || canonical == "Chatgpt-Account-Id" {
			return errors.New("candidate supplied provider authority header")
		}
	}
	return nil
}

func (b *CandidateBroker) verifyCapability(capability CandidateProviderRequestCapabilityV1) error {
	if capability.SchemaVersion != 1 || !lowerHexDigest(capability.CapabilityDigest) || !lowerHexDigest(capability.HeaderDigest) ||
		!lowerHexDigest(capability.BodyDigest) || !lowerHexDigest(capability.RouteBudgetDigest) || capability.OriginID != b.provider.OriginID || capability.DeadlineUnix <= time.Now().Unix() {
		return errors.New("candidate provider capability invalid")
	}
	headerDigest, err := candidateHeaderDigest(capability.Headers)
	if err != nil || !hmac.Equal([]byte(capability.HeaderDigest), []byte(headerDigest)) {
		return errors.New("candidate provider capability headers invalid")
	}
	wantSignature := b.signCapability(capability)
	if !hmac.Equal([]byte(capability.Signature), []byte(wantSignature)) {
		return errors.New("candidate provider capability signature invalid")
	}
	wantDigest, err := candidateCapabilityDigest(capability)
	if err != nil || !hmac.Equal([]byte(capability.CapabilityDigest), []byte(wantDigest)) {
		return errors.New("candidate provider capability digest invalid")
	}
	return nil
}

func (b *CandidateBroker) signCapability(capability CandidateProviderRequestCapabilityV1) string {
	capability.Signature = ""
	capability.CapabilityDigest = ""
	body, _ := json.Marshal(capability)
	mac, _ := FramedHMACSHA256Hex(b.capabilityKey[:], "cq/candidate-provider-capability-signature/v1\x00", body)
	return mac
}

func candidateCapabilityDigest(capability CandidateProviderRequestCapabilityV1) (string, error) {
	capability.CapabilityDigest = ""
	body, err := json.Marshal(capability)
	if err != nil {
		return "", err
	}
	return FramedSHA256Hex("cq/candidate-provider-capability/v1\x00", body)
}

func candidateHeaderDigest(header http.Header) (string, error) {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, http.CanonicalHeaderKey(name))
	}
	sort.Strings(names)
	canonical := make(http.Header, len(names))
	for _, name := range names {
		canonical[name] = append([]string(nil), header.Values(name)...)
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return FramedSHA256Hex("cq/candidate-provider-headers/v1\x00", body)
}

func digestBytesForCandidateBroker(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validCandidateLogicalKind(protocol string, kind CandidateProviderLogicalKind) bool {
	switch protocol {
	case "sse":
		return kind == CandidateLogicalSSE
	case "websocket":
		return kind == CandidateLogicalWebSocketText || kind == CandidateLogicalWebSocketBinary || kind == CandidateLogicalWebSocketPing || kind == CandidateLogicalWebSocketPong || kind == CandidateLogicalWebSocketClose || kind == CandidateLogicalWebSocketHandshake
	default:
		return false
	}
}

func cloneCandidateLogicalFrames(frames []CandidateProviderLogicalFrameV1) []CandidateProviderLogicalFrameV1 {
	result := make([]CandidateProviderLogicalFrameV1, len(frames))
	for index, frame := range frames {
		result[index] = CandidateProviderLogicalFrameV1{Kind: frame.Kind, Payload: append([]byte(nil), frame.Payload...)}
	}
	return result
}

func (b *CandidateBroker) recoverStates() error {
	for _, record := range b.store.JournalRecords(b.runID) {
		if record.CapabilityDigest == "" {
			continue
		}
		if !lowerHexDigest(record.CapabilityDigest) {
			return errors.New("candidate capability journal digest invalid")
		}
		state := b.states[record.CapabilityDigest]
		switch record.Kind {
		case "capability_issued":
			if state != 0 || record.PayloadDigest != record.CapabilityDigest {
				return errors.New("candidate capability issued journal invalid")
			}
			b.states[record.CapabilityDigest] = candidateCapabilityIssued
		case "capability_consumed":
			if state != candidateCapabilityIssued || record.PayloadDigest != record.CapabilityDigest {
				return errors.New("candidate capability consumed journal invalid")
			}
			b.states[record.CapabilityDigest] = candidateCapabilityConsumed
		case "capability_terminal":
			if state != candidateCapabilityConsumed {
				return errors.New("candidate capability terminal journal invalid")
			}
			b.states[record.CapabilityDigest] = candidateCapabilityTerminal
		default:
			return fmt.Errorf("candidate capability journal kind %q invalid", record.Kind)
		}
	}
	return nil
}

func (b *CandidateBroker) callHook(phase string) error {
	if b.hook == nil {
		return nil
	}
	return b.hook(phase)
}
