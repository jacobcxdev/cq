package codex

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// RefreshMutationRecorder selects bounded pre-effect authority and records its
// terminal release. Implementations must serialize both methods.
type RefreshMutationRecorder interface {
	SelectRefreshMutation(string, CandidateRef, Revision, RefreshMutationCapacity) (RefreshMutationSelection, error)
	CompleteRefreshMutation(string, string) (string, error)
}

type RefreshMutationSelection struct {
	ReservationDigest   string
	CapacityLeaseDigest string
}

type RefreshMutationRecovery struct {
	OperationID string
	Ref         CandidateRef
	Revision    Revision
	Selection   RefreshMutationSelection
}

type refreshCapacityLeaseV1 struct {
	SchemaVersion int                          `json:"schema_version"`
	OperationID   string                       `json:"operation_id"`
	Capacity      RefreshMutationCapacity      `json:"capacity"`
	Occupied      CredentialAuthorityOccupancy `json:"occupied"`
	Reserved      CredentialAuthorityOccupancy `json:"reserved"`
}

const (
	credentialAuthorityStableOperationFiles = 12
	credentialAuthorityStableOperationBytes = 2555904 + credentialOwnerRefreshResultEnvelopeMaxBytes
	credentialAuthorityStableOperationUnits = 12
)

func (l refreshCapacityLeaseV1) validate() error {
	if l.SchemaVersion != 1 || l.OperationID == "" || l.Capacity.Validate() != nil || l.Occupied.Files < 0 || l.Occupied.Bytes < 0 || l.Occupied.Units < 0 || l.Reserved.Files < 0 || l.Reserved.Bytes < 0 || l.Reserved.Units < 0 {
		return errors.New("invalid refresh capacity lease")
	}
	if l.Occupied.Files+l.Reserved.Files != l.Capacity.CredentialFiles || l.Occupied.Bytes+l.Reserved.Bytes != l.Capacity.CredentialBytes || l.Occupied.Units+l.Reserved.Units != l.Capacity.TotalUnits {
		return errors.New("refresh capacity lease does not reserve exact remaining capacity")
	}
	return nil
}

type refreshReservationV1 struct {
	SchemaVersion       int          `json:"schema_version"`
	OperationID         string       `json:"operation_id"`
	Ref                 CandidateRef `json:"ref"`
	Revision            Revision     `json:"revision"`
	CapacityLeaseDigest string       `json:"capacity_lease_digest"`
}

// RefreshMutationCapacity is the closed CU-2 admission ledger.
type RefreshMutationCapacity struct {
	MutationChildren        int
	OAuthChildren           int
	Decisions               int
	OperatorSlots           int
	CredentialFiles         int
	WireFrames              int
	TotalUnits              int
	TotalBytes              int64
	CredentialBytes         int64
	OuterObjectBytes        int
	FixedDeltaBytes         int
	PlanDeltaBytes          int
	ReauthDeltaBytes        int
	SelectedLeaseDeltaBytes int
	TerminalLeaseDeltaBytes int
	DecisionDeltaBytes      int
	OAuthDeltaBytes         int
	MutationDeltaBytes      int
	OuterBaseObjects        int
	OAuthResultObjects      int
	ControlProgress         bool
}

func (c RefreshMutationCapacity) Validate() error {
	limits := []struct {
		name        string
		actual, max int64
	}{
		{"mutation children", int64(c.MutationChildren), 256},
		{"OAuth children", int64(c.OAuthChildren), 64},
		{"decisions", int64(c.Decisions), 64},
		{"operator slots", int64(c.OperatorSlots), 2921},
		{"credential files", int64(c.CredentialFiles), 573},
		{"wire frames", int64(c.WireFrames), 331},
		{"total units", int64(c.TotalUnits), 3825},
		{"total bytes", c.TotalBytes, 522141696},
		{"credential bytes", c.CredentialBytes, 12656640},
		{"outer object bytes", int64(c.OuterObjectBytes), 262144},
		{"fixed delta bytes", int64(c.FixedDeltaBytes), 32768},
		{"plan delta bytes", int64(c.PlanDeltaBytes), 98304},
		{"reauth delta bytes", int64(c.ReauthDeltaBytes), 512},
		{"selected lease delta bytes", int64(c.SelectedLeaseDeltaBytes), 62},
		{"terminal lease delta bytes", int64(c.TerminalLeaseDeltaBytes), 62},
		{"decision delta bytes", int64(c.DecisionDeltaBytes), 256},
		{"OAuth delta bytes", int64(c.OAuthDeltaBytes), 384},
		{"mutation delta bytes", int64(c.MutationDeltaBytes), 320},
		{"outer base objects", int64(c.OuterBaseObjects), 32},
		{"OAuth result objects", int64(c.OAuthResultObjects), 64},
	}
	if c.ControlProgress {
		return errors.New("refresh mutation progress frames are forbidden")
	}
	for _, limit := range limits {
		if limit.actual != limit.max {
			return fmt.Errorf("refresh mutation %s does not equal the closed capacity", limit.name)
		}
	}
	return nil
}

func fullRefreshMutationCapacity() RefreshMutationCapacity {
	return RefreshMutationCapacity{
		MutationChildren:        256,
		OAuthChildren:           64,
		Decisions:               64,
		OperatorSlots:           2921,
		CredentialFiles:         573,
		WireFrames:              331,
		TotalUnits:              3825,
		TotalBytes:              522141696,
		CredentialBytes:         12656640,
		OuterObjectBytes:        262144,
		FixedDeltaBytes:         32768,
		PlanDeltaBytes:          98304,
		ReauthDeltaBytes:        512,
		SelectedLeaseDeltaBytes: 62,
		TerminalLeaseDeltaBytes: 62,
		DecisionDeltaBytes:      256,
		OAuthDeltaBytes:         384,
		MutationDeltaBytes:      320,
		OuterBaseObjects:        32,
		OAuthResultObjects:      64,
	}
}

type RefreshCredentialMutationSourceActionMapEntryV1 struct {
	SchemaVersion        int     `json:"schema_version"`
	SourceRowRefDigest   string  `json:"source_row_ref_digest"`
	CatalogueRowOrdinal  int     `json:"catalogue_row_ordinal"`
	EntrypointID         string  `json:"entrypoint_id"`
	EntrypointSymbol     string  `json:"entrypoint_symbol"`
	ImplementationSymbol string  `json:"implementation_symbol"`
	CallChainDigest      string  `json:"call_chain_digest"`
	SourceID             string  `json:"source_id"`
	WriterKind           string  `json:"writer_kind"`
	LegacyOperationShape *string `json:"legacy_operation_shape"`
	Provider             string  `json:"provider"`
	SourceKind           string  `json:"source_kind"`
	ActionKind           string  `json:"action_kind"`
	TargetSelectorKind   string  `json:"target_selector_kind"`
	TargetSemanticsRule  string  `json:"target_semantics_rule"`
}

type RefreshCredentialMutationSourceActionMapV1 struct {
	SchemaVersion                        int                                               `json:"schema_version"`
	AuthorityBaselineCommit              string                                            `json:"authority_baseline_commit"`
	ReleaseSourceCommit                  string                                            `json:"release_source_commit"`
	LegacyAtomicWriterReachabilityDigest string                                            `json:"legacy_atomic_writer_reachability_catalogue_digest"`
	Entries                              []RefreshCredentialMutationSourceActionMapEntryV1 `json:"entries"`
	EntrySetDigest                       string                                            `json:"entry_set_digest"`
	IssuerPublicKey                      string                                            `json:"issuer_public_key"`
	Signature                            string                                            `json:"signature,omitempty"`
}

func sourceActionTuple(e RefreshCredentialMutationSourceActionMapEntryV1) string {
	return fmt.Sprintf("%020d\x00%s\x00%s\x00%s\x00%s\x00%s", e.CatalogueRowOrdinal, e.EntrypointID, e.CallChainDigest, e.SourceID, e.SourceKind, e.Provider)
}

func validateSourceActionEntries(entries []RefreshCredentialMutationSourceActionMapEntryV1) error {
	if len(entries) == 0 {
		return errors.New("refresh source-action map is empty")
	}
	rows := make(map[string]struct{}, len(entries))
	tuples := make(map[string]struct{}, len(entries))
	previous := ""
	for i, entry := range entries {
		if entry.SchemaVersion != 1 || entry.SourceRowRefDigest == "" || entry.CatalogueRowOrdinal < 1 || entry.EntrypointID == "" || entry.EntrypointSymbol == "" || entry.ImplementationSymbol == "" || entry.CallChainDigest == "" || entry.SourceID == "" {
			return errors.New("invalid refresh source-action map entry")
		}
		if !validRefreshSourceActionTuple(entry) {
			return errors.New("unknown refresh source-action tuple")
		}
		tuple := sourceActionTuple(entry)
		if i > 0 && strings.Compare(previous, tuple) >= 0 {
			return errors.New("refresh source-action map entries are not canonical")
		}
		previous = tuple
		if _, exists := rows[entry.SourceRowRefDigest]; exists {
			return errors.New("duplicate refresh source row")
		}
		if _, exists := tuples[tuple]; exists {
			return errors.New("duplicate refresh source callsite")
		}
		rows[entry.SourceRowRefDigest] = struct{}{}
		tuples[tuple] = struct{}{}
	}
	return nil
}

func validRefreshSourceActionTuple(entry RefreshCredentialMutationSourceActionMapEntryV1) bool {
	if (entry.Provider != "claude" && entry.Provider != "codex") || !containsString([]string{"anonymous_sync", "automatic_claude_persistence", "cq_store_persistence", "codex_refresh_persistence", "oauth_reauth"}, entry.SourceKind) {
		return false
	}
	legacyIs := func(want string) bool {
		return entry.LegacyOperationShape != nil && *entry.LegacyOperationShape == want
	}
	switch entry.TargetSemanticsRule {
	case "atomic_replace_from_catalogue_grammars_v1":
		return entry.WriterKind == "atomic_file" && entry.LegacyOperationShape == nil && entry.ActionKind == "replace" && entry.TargetSelectorKind == "replace_file"
	case "file_delete_from_catalogue_grammar_v1":
		return entry.WriterKind == "file_delete" && (legacyIs("file_unlink") || legacyIs("descriptor_unlink")) && entry.ActionKind == "file_delete" && entry.TargetSelectorKind == "delete_file"
	case "quota_cache_generation_from_callsite_v1":
		return entry.WriterKind == "file_delete" && legacyIs("file_unlink") && entry.ActionKind == "cache_generation_invalidate" && entry.TargetSelectorKind == "cache_generation"
	case "claude_platform_update_or_add_v1":
		return entry.WriterKind == "native_keychain_upsert" && legacyIs("platform_update_or_add") && entry.ActionKind == "native_keychain_upsert" && entry.TargetSelectorKind == "native_upsert"
	case "claude_cq_set_delete_retry_set_v1":
		return entry.WriterKind == "native_keychain_upsert" && legacyIs("cq_set_delete_retry_set") && entry.ActionKind == "native_keychain_upsert" && entry.TargetSelectorKind == "native_upsert"
	case "claude_platform_repeated_delete_v1":
		return entry.WriterKind == "native_keychain_delete" && legacyIs("platform_repeated_service_delete") && entry.ActionKind == "native_keychain_delete" && entry.TargetSelectorKind == "native_delete"
	case "claude_cq_manifest_selector_delete_v1":
		return entry.WriterKind == "native_keychain_delete" && legacyIs("cq_manifest_selector_delete") && entry.ActionKind == "native_keychain_delete" && entry.TargetSelectorKind == "native_delete"
	default:
		return false
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func framedSHA256(domain string, payload []byte) string {
	input := make([]byte, 0, len(domain)+4+len(payload))
	input = append(input, domain...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	input = append(input, length[:]...)
	input = append(input, payload...)
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:])
}

func SignRefreshCredentialMutationSourceActionMap(privateKey ed25519.PrivateKey, baseline, release, catalogue string, entries []RefreshCredentialMutationSourceActionMapEntryV1) (RefreshCredentialMutationSourceActionMapV1, error) {
	if len(privateKey) != ed25519.PrivateKeySize || baseline == "" || release == "" || catalogue == "" {
		return RefreshCredentialMutationSourceActionMapV1{}, errors.New("refresh source-action signing authority unavailable")
	}
	canonicalEntries := append([]RefreshCredentialMutationSourceActionMapEntryV1(nil), entries...)
	sort.Slice(canonicalEntries, func(i, j int) bool {
		return sourceActionTuple(canonicalEntries[i]) < sourceActionTuple(canonicalEntries[j])
	})
	if err := validateSourceActionEntries(canonicalEntries); err != nil {
		return RefreshCredentialMutationSourceActionMapV1{}, err
	}
	entryBytes, err := json.Marshal(canonicalEntries)
	if err != nil {
		return RefreshCredentialMutationSourceActionMapV1{}, err
	}
	result := RefreshCredentialMutationSourceActionMapV1{
		SchemaVersion: 1, AuthorityBaselineCommit: baseline, ReleaseSourceCommit: release,
		LegacyAtomicWriterReachabilityDigest: catalogue, Entries: canonicalEntries,
		EntrySetDigest:  framedSHA256("cq/operator-control/refresh-source-action-map-entry-set/v1\x00", entryBytes),
		IssuerPublicKey: base64.RawURLEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey)),
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return RefreshCredentialMutationSourceActionMapV1{}, err
	}
	result.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return result, nil
}

func ReleaseBuildAuthorityPublicKeyFingerprint(publicKey ed25519.PublicKey) string {
	return framedSHA256("cq/release-build-authority/public-key-fingerprint/v1\x00", publicKey)
}

func (m RefreshCredentialMutationSourceActionMapV1) Verify(expectedPublicKey ed25519.PublicKey, expectedFingerprint, expectedBaseline, expectedRelease, expectedCatalogue string) error {
	if m.SchemaVersion != 1 || m.AuthorityBaselineCommit == "" || m.ReleaseSourceCommit == "" || m.LegacyAtomicWriterReachabilityDigest == "" {
		return errors.New("invalid refresh source-action map")
	}
	if len(expectedPublicKey) != ed25519.PublicKeySize || expectedFingerprint == "" || ReleaseBuildAuthorityPublicKeyFingerprint(expectedPublicKey) != expectedFingerprint {
		return errors.New("refresh source-action release authority pin unavailable")
	}
	if expectedBaseline == "" || expectedRelease == "" || expectedCatalogue == "" || m.AuthorityBaselineCommit != expectedBaseline || m.ReleaseSourceCommit != expectedRelease || m.LegacyAtomicWriterReachabilityDigest != expectedCatalogue {
		return errors.New("refresh source-action release identity mismatch")
	}
	if err := validateSourceActionEntries(m.Entries); err != nil {
		return err
	}
	entryBytes, err := json.Marshal(m.Entries)
	if err != nil || m.EntrySetDigest != framedSHA256("cq/operator-control/refresh-source-action-map-entry-set/v1\x00", entryBytes) {
		return errors.New("invalid refresh source-action entry-set digest")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(m.IssuerPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid refresh source-action issuer")
	}
	if !hmac.Equal(publicKey, expectedPublicKey) {
		return errors.New("refresh source-action issuer does not match release authority")
	}
	signature, err := base64.RawURLEncoding.DecodeString(m.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("invalid refresh source-action signature")
	}
	unsigned := m
	unsigned.Signature = ""
	payload, err := json.Marshal(unsigned)
	if err != nil || !ed25519.Verify(publicKey, payload, signature) {
		return errors.New("invalid refresh source-action signature")
	}
	return nil
}

// RefreshMutationStore persists one capacity-bound reservation and retains
// its backend mutation gate until the terminal release is selected.
type RefreshMutationStore struct{ chain *credentialAuthorityChain }

func OpenRefreshMutationStore(ctx context.Context, backend CredentialAuthorityBackend, key []byte, hook func(string) error) (*RefreshMutationStore, error) {
	chain, err := openCredentialAuthorityChain(ctx, backend, "refresh-mutation", key, hook)
	if err != nil {
		return nil, err
	}
	return &RefreshMutationStore{chain: chain}, nil
}

func (s *RefreshMutationStore) SelectRefreshMutation(operationID string, ref CandidateRef, revision Revision, capacity RefreshMutationCapacity) (RefreshMutationSelection, error) {
	if operationID == "" || ref.CandidateID == "" || revision == "" {
		return RefreshMutationSelection{}, errors.New("refresh mutation authority unavailable")
	}
	if err := capacity.Validate(); err != nil {
		return RefreshMutationSelection{}, err
	}
	if err := s.chain.acquireAndReopenGate(); err != nil {
		return RefreshMutationSelection{}, err
	}
	selected := false
	defer func() {
		if !selected {
			s.chain.abandonGate()
		}
	}()
	if recovery, ok, err := s.RecoverSelectedRefreshMutation(); err != nil {
		return RefreshMutationSelection{}, err
	} else if ok {
		if recovery.OperationID != operationID || recovery.Ref != ref || recovery.Revision != revision {
			return RefreshMutationSelection{}, errors.New("refresh mutation operation already selected")
		}
		selected = true
		return recovery.Selection, nil
	}
	occupancyBackend, ok := s.chain.backend.(CredentialAuthorityOccupancyBackend)
	if !ok {
		return RefreshMutationSelection{}, errors.New("credential authority occupancy capability unavailable")
	}
	occupied, err := occupancyBackend.CredentialAuthorityOccupancy(s.chain.ctx)
	if err != nil || occupied.Files < 0 || occupied.Bytes < 0 || occupied.Units < 0 {
		return RefreshMutationSelection{}, errors.New("credential authority occupancy unavailable")
	}
	reserved := CredentialAuthorityOccupancy{
		Files: capacity.CredentialFiles - occupied.Files,
		Bytes: capacity.CredentialBytes - occupied.Bytes,
		Units: capacity.TotalUnits - occupied.Units,
	}
	if reserved.Files < credentialAuthorityStableOperationFiles || reserved.Bytes < credentialAuthorityStableOperationBytes || reserved.Units < credentialAuthorityStableOperationUnits {
		return RefreshMutationSelection{}, errors.New("refresh mutation capacity unavailable")
	}
	leaseBody, err := json.Marshal(refreshCapacityLeaseV1{SchemaVersion: 1, OperationID: operationID, Capacity: capacity, Occupied: occupied, Reserved: reserved})
	if err != nil {
		return RefreshMutationSelection{}, err
	}
	leaseDigest := framedSHA256("cq/credential-owner/refresh-mutation/capacity-lease/v1\x00", leaseBody)
	leaseName := "refresh-capacity-lease-" + leaseDigest + ".json"
	if _, err := s.chain.publishOrAdopt(leaseName, leaseBody); err != nil {
		return RefreshMutationSelection{}, err
	}
	reopenedLease, _, err := s.chain.backend.Read(s.chain.ctx, leaseName, int64(len(leaseBody))+1)
	if err != nil || !hmac.Equal(reopenedLease, leaseBody) {
		return RefreshMutationSelection{}, errors.New("refresh capacity lease reopen mismatch")
	}
	reservation := refreshReservationV1{SchemaVersion: 1, OperationID: operationID, Ref: ref, Revision: revision, CapacityLeaseDigest: leaseDigest}
	body, err := json.Marshal(reservation)
	if err != nil {
		return RefreshMutationSelection{}, err
	}
	reservationDigest := framedSHA256("cq/credential-owner/refresh-mutation/reservation/v1\x00", body)
	reservationName := "refresh-reservation-" + reservationDigest + ".json"
	if _, err := s.chain.publishOrAdopt(reservationName, body); err != nil {
		return RefreshMutationSelection{}, err
	}
	reopenedReservation, _, err := s.chain.backend.Read(s.chain.ctx, reservationName, int64(len(body))+1)
	if err != nil || !hmac.Equal(reopenedReservation, body) {
		return RefreshMutationSelection{}, errors.New("refresh reservation reopen mismatch")
	}
	if _, err := s.chain.selectOperation(operationID, reservationDigest); err != nil {
		return RefreshMutationSelection{}, err
	}
	selected = true
	return RefreshMutationSelection{ReservationDigest: reservationDigest, CapacityLeaseDigest: leaseDigest}, nil
}

func (s *RefreshMutationStore) RecoverSelectedRefreshMutation() (RefreshMutationRecovery, bool, error) {
	s.chain.mu.Lock()
	defer s.chain.mu.Unlock()
	if s.chain.anchor == nil || s.chain.anchor.Phase != "selected" || s.chain.selected == nil {
		return RefreshMutationRecovery{}, false, nil
	}
	reservationDigest := s.chain.selected.ValueDigest
	reservationBody, _, err := s.chain.backend.Read(s.chain.ctx, "refresh-reservation-"+reservationDigest+".json", 64<<10)
	if err != nil {
		return RefreshMutationRecovery{}, false, err
	}
	if framedSHA256("cq/credential-owner/refresh-mutation/reservation/v1\x00", reservationBody) != reservationDigest {
		return RefreshMutationRecovery{}, false, errors.New("refresh mutation reservation digest mismatch")
	}
	var reservation refreshReservationV1
	if err := json.Unmarshal(reservationBody, &reservation); err != nil || reservation.SchemaVersion != 1 || reservation.OperationID != s.chain.anchor.OperationID || reservation.Ref.CandidateID == "" || reservation.Revision == "" || reservation.CapacityLeaseDigest == "" {
		return RefreshMutationRecovery{}, false, errors.New("invalid selected refresh mutation reservation")
	}
	leaseName := "refresh-capacity-lease-" + reservation.CapacityLeaseDigest + ".json"
	leaseBody, _, err := s.chain.backend.Read(s.chain.ctx, leaseName, 64<<10)
	if err != nil || framedSHA256("cq/credential-owner/refresh-mutation/capacity-lease/v1\x00", leaseBody) != reservation.CapacityLeaseDigest {
		return RefreshMutationRecovery{}, false, errors.New("refresh mutation capacity lease digest mismatch")
	}
	var lease refreshCapacityLeaseV1
	if err := json.Unmarshal(leaseBody, &lease); err != nil || lease.OperationID != reservation.OperationID || lease.validate() != nil {
		return RefreshMutationRecovery{}, false, errors.New("invalid selected refresh mutation capacity lease")
	}
	return RefreshMutationRecovery{
		OperationID: reservation.OperationID,
		Ref:         reservation.Ref,
		Revision:    reservation.Revision,
		Selection:   RefreshMutationSelection{ReservationDigest: reservationDigest, CapacityLeaseDigest: reservation.CapacityLeaseDigest},
	}, true, nil
}

func (s *RefreshMutationStore) CompleteRefreshMutation(operationID, credentialReceiptDigest string) (string, error) {
	_, terminalDigest, err := s.chain.terminalise(operationID, credentialReceiptDigest)
	return terminalDigest, err
}

func (s *RefreshMutationStore) Close() error { return s.chain.Close() }
