package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const CodexLeaseAttemptPolicyVersion uint32 = 1

var (
	ErrCodexLeaseAttemptLimit    = errors.New("Codex lease attempt limit reached")
	ErrCodexLeaseInvalidMutation = errors.New("invalid Codex lane mutation")
)

type CodexLeaseStaleMutationError struct {
	Component string
	Detail    string
}

func (err *CodexLeaseStaleMutationError) Error() string {
	if err.Detail == "" {
		return fmt.Sprintf("%s: %s", ErrCodexLeaseStaleMutation, err.Component)
	}
	return fmt.Sprintf("%s: %s: %s", ErrCodexLeaseStaleMutation, err.Component, err.Detail)
}

func (err *CodexLeaseStaleMutationError) Unwrap() error { return ErrCodexLeaseStaleMutation }

type CodexJournalRecordIdentity struct {
	LaneDigest    string
	TurnDigest    string
	ModeEpoch     uint64
	Authoritative bool
}

func (identity CodexJournalRecordIdentity) IsZero() bool {
	return identity == (CodexJournalRecordIdentity{})
}

type CodexAttemptFence struct {
	RequestGeneration uint64
	Generation        uint64
	Revision          uint64
}

type CodexLeaseRecordFence struct {
	Record            CodexJournalRecordIdentity
	Revision          uint64
	Lease             uint64
	RequestGeneration uint64
	CurrentAttempt    uint64
	TouchedAttempts   []CodexAttemptFence
}

type CodexLeaseGenerationFence struct {
	Journal        uint64
	Lane           uint64
	Current        CodexJournalRecordIdentity
	Last           CodexJournalRecordIdentity
	TouchedRecords []CodexLeaseRecordFence
}

// CodexLaneMutation is an explicit semantic after-image patch. A nil Lane
// leaves the lane pointer unchanged. Records not named in UpsertRecords or
// DeleteRecords survive unchanged; omission never deletes durable authority.
// Generation and timestamp fields are store-owned and must be zero in inputs.
type CodexLaneMutation struct {
	Lane                     *CodexJournalLane
	BeginRequest             *CodexJournalRecordIdentity
	MigrateTurnStateLatch    *CodexJournalRecordIdentity
	AccountUnavailable       *CodexJournalRecordIdentity
	QuotaExhausted           *CodexJournalRecordIdentity
	CompleteUnavailableCycle *CodexJournalRecordIdentity
	UpsertRecords            []CodexJournalRecordV2
	DeleteRecords            []CodexJournalRecordIdentity
}

func (record CodexJournalRecordV2) Identity() CodexJournalRecordIdentity {
	return CodexJournalRecordIdentity{
		LaneDigest:    codexJournalLaneDigest(record.SessionHash, record.ThreadHash, record.NamespaceHash),
		TurnDigest:    record.TurnHash,
		ModeEpoch:     record.ModeEpoch,
		Authoritative: record.Authoritative,
	}
}

func codexJournalLaneDigest(sessionHash, threadHash, namespaceHash string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(sessionHash))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(threadHash))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(namespaceHash))
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

func codexLeaseAttemptPlanDigest(key []byte, slots []CodexAttemptSlot) string {
	canonical := append([]CodexAttemptSlot(nil), slots...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Index < canonical[j].Index })
	payload, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("plan"))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (store *CodexLeaseStore) CommitLane(expected CodexLeaseGenerationFence, mutation CodexLaneMutation) (CodexLeaseGenerationFence, error) {
	return store.commitLane(expected, mutation, nil)
}

// commitLane runs capture after the verified after-image is installed and
// while store.mu is still held. capture must detach any data it retains.
func (store *CodexLeaseStore) commitLane(expected CodexLeaseGenerationFence, mutation CodexLaneMutation, capture func(CodexLeaseGenerationFence, codexLeaseJournalEnvelopeV2)) (CodexLeaseGenerationFence, error) {
	if store == nil {
		return CodexLeaseGenerationFence{}, ErrCodexLeaseWriterUnavailable
	}
	operation, err := store.beginOperation()
	if err != nil {
		return CodexLeaseGenerationFence{}, err
	}
	defer operation.Release()

	store.mu.Lock()
	defer store.mu.Unlock()
	commitStarted := time.Now()
	defer func() {
		lanes, records := 0, 0
		if store.v2 != nil {
			lanes, records = len(store.v2.Lanes), len(store.v2.Records)
		}
		codexProcessRuntimeObservability.recordJournalCommit(time.Since(commitStarted), len(store.journalBytes), lanes, records, store.poisoned != nil)
	}()
	if store.closed {
		return CodexLeaseGenerationFence{}, ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return CodexLeaseGenerationFence{}, fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if store.v2 == nil {
		return CodexLeaseGenerationFence{}, fmt.Errorf("%w: schema-v2 journal unavailable", ErrCodexLeaseTrustLost)
	}
	if store.v2.Cutover.State != CodexLeaseCutoverComplete || !store.v2.Cutover.NoLegacyAuthority {
		return CodexLeaseGenerationFence{}, ErrCodexLegacyQuarantine
	}

	next, post, err := store.applyCodexLaneMutationLocked(expected, mutation)
	if err != nil {
		return CodexLeaseGenerationFence{}, err
	}
	retentionSweepAt := time.Time{}
	if store.policy.Retention > 0 && store.policy.Now != nil {
		now := store.policy.Now().UTC()
		if store.lastRetentionSweep.IsZero() || !now.Before(store.lastRetentionSweep.Add(codexLeaseRetentionSweepInterval)) {
			next, _, err = compactCodexLeaseV2Envelope(next, now, store.policy.Retention)
			if err != nil {
				return CodexLeaseGenerationFence{}, fmt.Errorf("%w: retention sweep: %w", ErrCodexLeaseInvalidMutation, err)
			}
			retentionSweepAt = now
		}
	}
	if err := validateCodexLeaseRepresentedModes(representedCodexLeaseAuthoritativeEpochs(next.Records), store.modes); err != nil {
		return CodexLeaseGenerationFence{}, fmt.Errorf("%w: strict candidate validation: %w", ErrCodexLeaseInvalidMutation, err)
	}
	installedEnvelope, data, err := store.prepareV2Envelope(next)
	if err != nil {
		return CodexLeaseGenerationFence{}, fmt.Errorf("%w: strict candidate validation: %w", ErrCodexLeaseInvalidMutation, err)
	}
	beforeReplace := func() error {
		if err := store.revalidateV2InstalledLocked(); err != nil {
			return err
		}
		if store.v2.Generation != expected.Journal {
			return staleCodexLeaseMutation("journal_generation", fmt.Sprintf("have %d, expected %d", store.v2.Generation, expected.Journal))
		}
		return nil
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(store.inspector, store.directory, store.journalName, data, beforeReplace); err != nil {
		if fsutil.AtomicWriteOutcome(err) == fsutil.CommitIndeterminate || errors.Is(err, ErrCodexLeaseTrustLost) {
			store.poisoned = err
		}
		return CodexLeaseGenerationFence{}, fmt.Errorf("commit Codex lane mutation: %w", err)
	}
	installed, installedID, err := fsutil.ReadSecureFileInDirectoryWithIdentity(store.inspector, store.directory, store.journalName, codexLeaseJournalMaxBytes)
	if err != nil || !bytes.Equal(installed, data) {
		poison := &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Op: "verify committed Codex lane mutation", Err: err}
		if err == nil {
			poison.Err = errors.New("installed bytes differ from committed bytes")
		}
		store.poisoned = poison
		return CodexLeaseGenerationFence{}, fmt.Errorf("%w: %w", ErrCodexLeaseStorePoisoned, poison)
	}
	store.v2 = &installedEnvelope
	store.generation = installedEnvelope.Generation
	store.journalBytes = installed
	store.journalID = installedID
	if !retentionSweepAt.IsZero() {
		store.lastRetentionSweep = retentionSweepAt
	}
	if capture != nil {
		capture(cloneCodexLeaseGenerationFence(post), installedEnvelope)
	}
	return cloneCodexLeaseGenerationFence(post), nil
}

func (store *CodexLeaseStore) applyCodexLaneMutationLocked(expected CodexLeaseGenerationFence, mutation CodexLaneMutation) (codexLeaseJournalEnvelopeV2, CodexLeaseGenerationFence, error) {
	if expected.Journal != store.v2.Generation {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, staleCodexLeaseMutation("journal_generation", fmt.Sprintf("have %d, expected %d", store.v2.Generation, expected.Journal))
	}
	if store.v2.Generation == math.MaxUint64 {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: journal generation overflow", ErrCodexLeaseInvalidMutation)
	}
	if mutation.Lane == nil && mutation.CompleteUnavailableCycle == nil && len(mutation.UpsertRecords) == 0 && len(mutation.DeleteRecords) == 0 {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: empty mutation", ErrCodexLeaseInvalidMutation)
	}
	if err := validateCodexLaneMutationOwnedFields(mutation); err != nil {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
	}

	next := cloneCodexLeaseV2Envelope(*store.v2)
	recordIndex := make(map[CodexJournalRecordIdentity]int, len(next.Records))
	originalRecords := make(map[CodexJournalRecordIdentity]CodexJournalRecordV2, len(next.Records))
	for index, record := range next.Records {
		identity := record.Identity()
		if identity.IsZero() {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: zero stored record identity", ErrCodexLeaseTrustLost)
		}
		if _, duplicate := recordIndex[identity]; duplicate {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: duplicate stored record identity", ErrCodexLeaseTrustLost)
		}
		recordIndex[identity] = index
		originalRecords[identity] = record
	}
	laneIndex := make(map[string]int, len(next.Lanes))
	for index, lane := range next.Lanes {
		digest := codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
		if digest == codexJournalLaneDigest("", "", "") || lane.SessionHash == "" || lane.ThreadHash == "" || lane.NamespaceHash == "" {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: invalid stored lane identity", ErrCodexLeaseTrustLost)
		}
		if _, duplicate := laneIndex[digest]; duplicate {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: duplicate stored lane identity", ErrCodexLeaseTrustLost)
		}
		laneIndex[digest] = index
	}

	targetLane, err := codexLaneMutationTarget(mutation)
	if err != nil {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
	}
	var beginRequest CodexJournalRecordIdentity
	if mutation.BeginRequest != nil {
		beginRequest = *mutation.BeginRequest
		if beginRequest.IsZero() || beginRequest.LaneDigest != targetLane || len(mutation.UpsertRecords) != 1 || mutation.UpsertRecords[0].Identity() != beginRequest || len(mutation.DeleteRecords) != 0 || mutation.Lane != nil {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: BeginRequest must name its sole existing record upsert", ErrCodexLeaseInvalidMutation)
		}
	}
	var migrateTurnStateLatch CodexJournalRecordIdentity
	if mutation.MigrateTurnStateLatch != nil {
		migrateTurnStateLatch = *mutation.MigrateTurnStateLatch
		if migrateTurnStateLatch.IsZero() || migrateTurnStateLatch != beginRequest {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: turn-state latch migration must name BeginRequest record", ErrCodexLeaseInvalidMutation)
		}
	}
	var accountUnavailable CodexJournalRecordIdentity
	if mutation.AccountUnavailable != nil {
		accountUnavailable = *mutation.AccountUnavailable
		if accountUnavailable.IsZero() || accountUnavailable.LaneDigest != targetLane || len(mutation.UpsertRecords) != 1 || mutation.UpsertRecords[0].Identity() != accountUnavailable || len(mutation.DeleteRecords) != 0 || mutation.Lane != nil || mutation.BeginRequest != nil {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: account unavailability must name its sole existing record upsert", ErrCodexLeaseInvalidMutation)
		}
	}
	var quotaExhausted CodexJournalRecordIdentity
	if mutation.QuotaExhausted != nil {
		quotaExhausted = *mutation.QuotaExhausted
		if quotaExhausted.IsZero() || quotaExhausted != accountUnavailable {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: quota exhaustion must name its sole existing record upsert", ErrCodexLeaseInvalidMutation)
		}
	}
	var completeUnavailableCycle CodexJournalRecordIdentity
	if mutation.CompleteUnavailableCycle != nil {
		completeUnavailableCycle = *mutation.CompleteUnavailableCycle
		if completeUnavailableCycle.IsZero() || completeUnavailableCycle.LaneDigest != targetLane || len(mutation.UpsertRecords) != 0 || len(mutation.DeleteRecords) != 0 || mutation.Lane != nil || mutation.BeginRequest != nil || mutation.AccountUnavailable != nil || mutation.QuotaExhausted != nil {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: unavailable cycle completion must name one current record", ErrCodexLeaseInvalidMutation)
		}
	}
	storedLaneIndex, laneExists := laneIndex[targetLane]
	var storedLane CodexJournalLane
	if laneExists {
		storedLane = next.Lanes[storedLaneIndex]
	}
	storedCurrent, err := codexLaneCurrentIdentity(storedLane, recordIndex, next.Records)
	if err != nil {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
	}
	storedLast, err := codexLaneLastIdentity(storedLane, recordIndex, next.Records)
	if err != nil {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
	}
	restartingFailedLast := false
	if mutation.BeginRequest != nil && storedCurrent.IsZero() && beginRequest == storedLast {
		if index, found := recordIndex[beginRequest]; found {
			restartingFailedLast = codexLeaseRestartableFailedHead(next.Records[index])
		}
	}
	if mutation.BeginRequest != nil && beginRequest != storedCurrent && !restartingFailedLast {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: BeginRequest must target the current lane head", ErrCodexLeaseInvalidMutation)
	}
	if !completeUnavailableCycle.IsZero() && completeUnavailableCycle != storedCurrent {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: unavailable cycle completion must target the current lane head", ErrCodexLeaseInvalidMutation)
	}
	if !laneExists {
		if expected.Lane != 0 || !expected.Current.IsZero() || !expected.Last.IsZero() {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, staleCodexLeaseMutation("lane_absence", "lane appeared or expected tuple is nonzero")
		}
	} else if expected.Lane != storedLane.Generation {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, staleCodexLeaseMutation("lane_generation", fmt.Sprintf("have %d, expected %d", storedLane.Generation, expected.Lane))
	} else if expected.Current != storedCurrent {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, staleCodexLeaseMutation("lane_current", "current tuple changed")
	} else if expected.Last != storedLast {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, staleCodexLeaseMutation("lane_last", "last tuple changed")
	}

	fences, err := validateCodexLeaseRecordFences(expected.TouchedRecords, recordIndex, next.Records)
	if err != nil {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
	}
	requiredFences := make(map[CodexJournalRecordIdentity]struct{})
	upsertIDs := make(map[CodexJournalRecordIdentity]struct{}, len(mutation.UpsertRecords))
	deleteIDs := make(map[CodexJournalRecordIdentity]struct{}, len(mutation.DeleteRecords))
	if !completeUnavailableCycle.IsZero() {
		requiredFences[completeUnavailableCycle] = struct{}{}
		record := next.Records[recordIndex[completeUnavailableCycle]]
		if record.PredecessorTurnHash != "" {
			requiredFences[CodexJournalRecordIdentity{LaneDigest: targetLane, TurnDigest: record.PredecessorTurnHash, ModeEpoch: record.PredecessorModeEpoch, Authoritative: record.PredecessorAuthoritative}] = struct{}{}
		}
	}
	for _, record := range mutation.UpsertRecords {
		identity := record.Identity()
		if identity.IsZero() || identity.LaneDigest != targetLane {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: upsert record is outside target lane", ErrCodexLeaseInvalidMutation)
		}
		if _, duplicate := upsertIDs[identity]; duplicate {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: duplicate record upsert", ErrCodexLeaseInvalidMutation)
		}
		upsertIDs[identity] = struct{}{}
		requiredFences[identity] = struct{}{}
		if record.PredecessorTurnHash != "" {
			predecessor := CodexJournalRecordIdentity{LaneDigest: targetLane, TurnDigest: record.PredecessorTurnHash, ModeEpoch: record.PredecessorModeEpoch, Authoritative: record.PredecessorAuthoritative}
			requiredFences[predecessor] = struct{}{}
		}
	}
	for _, identity := range mutation.DeleteRecords {
		if identity.IsZero() || identity.LaneDigest != targetLane {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: deletion is outside target lane", ErrCodexLeaseInvalidMutation)
		}
		if _, duplicate := deleteIDs[identity]; duplicate {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: duplicate record deletion", ErrCodexLeaseInvalidMutation)
		}
		if _, overlap := upsertIDs[identity]; overlap {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: record cannot be upserted and deleted", ErrCodexLeaseInvalidMutation)
		}
		deleteIDs[identity] = struct{}{}
		requiredFences[identity] = struct{}{}
	}
	for _, input := range mutation.UpsertRecords {
		old, exists := originalRecords[input.Identity()]
		if !exists {
			continue
		}
		oldAttempt, oldFound := codexLeaseAttemptByGeneration(old.Attempts, old.CurrentAttemptGeneration)
		inputAttempt, inputFound := codexLeaseAttemptByGeneration(input.Attempts, old.CurrentAttemptGeneration)
		narrowTerminalTransition := old.State == LeaseProvisional && input.State == LeaseOrphaned
		narrowTerminalTransition = narrowTerminalTransition || (oldFound && inputFound && oldAttempt.State == CodexAttemptPrepared && inputAttempt.State == CodexAttemptAbandonedBeforeDispatch)
		if narrowTerminalTransition && (len(mutation.UpsertRecords) != 1 || len(mutation.DeleteRecords) != 0 || mutation.Lane != nil) {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: narrow terminal transition must be the sole record mutation", ErrCodexLeaseInvalidMutation)
		}
	}
	for identity := range requiredFences {
		if _, ok := fences[identity]; !ok {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: missing fence for touched record", ErrCodexLeaseInvalidMutation)
		}
	}
	for identity := range fences {
		if _, ok := requiredFences[identity]; !ok {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: unrelated record fence", ErrCodexLeaseInvalidMutation)
		}
	}
	if !completeUnavailableCycle.IsZero() {
		record := next.Records[recordIndex[completeUnavailableCycle]]
		if !record.EverAdmitted || record.State != LeaseBoundQuiescent || codexLeaseCurrentAttemptState(record) != CodexAttemptAccountUnavailable || record.RoutingRefs != 0 || record.AttemptRefs != 0 || record.ResponseObserverRefs != 0 || !record.SocketLineageExtinct || len(storedLane.RequestUnavailableAccountHashes) == 0 || !codexLeaseExactCurrentAttemptFence(fences[completeUnavailableCycle], record) {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: unavailable cycle is not terminal and surfaced", ErrCodexLeaseInvalidMutation)
		}
	}

	wallNow := store.policy.Now().UTC()
	now := monotonicCodexLeaseTime(wallNow, storedLane.LastObservedAt)
	for identity := range deleteIDs {
		index, exists := recordIndex[identity]
		if !exists || codexLeaseRetentionActionFor(next.Records[index], wallNow, store.policy.Retention) != codexLeaseRetentionDelete {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: record deletion is not retention-eligible", ErrCodexLeaseInvalidMutation)
		}
	}
	appendGenerations := make(map[CodexJournalRecordIdentity]uint64)
	firstAdmissions := make(map[CodexJournalRecordIdentity]CodexJournalRecordV2)
	affinityRefreshes := make(map[CodexJournalRecordIdentity]struct{})
	cacheAdmissions := make(map[CodexJournalRecordIdentity]CodexJournalRecordV2)
	accountUnavailableHash := ""
	quotaExhaustedHash := ""
	quotaProbeRecoveryHash := ""
	requestCompleted := false
	predecessorChanged := false
	for _, input := range mutation.UpsertRecords {
		identity := input.Identity()
		index, exists := recordIndex[identity]
		var old CodexJournalRecordV2
		if exists {
			old = next.Records[index]
			if old.PredecessorTurnHash != input.PredecessorTurnHash || old.PredecessorModeEpoch != input.PredecessorModeEpoch || old.PredecessorAuthoritative != input.PredecessorAuthoritative {
				predecessorChanged = true
			}
		} else if input.PredecessorTurnHash != "" {
			predecessorChanged = true
		}
		beginRequestAffinityReset := identity == beginRequest && codexLeaseAffinityInvalidationBeginRequest(old, input, storedLane, next.AffinityInvalidationGeneration)
		result, appended, firstAdmission, err := store.buildCodexLeaseRecordAfterImage(old, exists, input, identity == beginRequest, identity == migrateTurnStateLatch, beginRequestAffinityReset, fences[identity], now)
		if err != nil {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
		}
		if appended != 0 {
			appendGenerations[identity] = appended
		}
		if firstAdmission {
			firstAdmissions[identity] = result
			if exists && codexLeaseAccountUnavailableAdmission(old, input) && storedLane.LastAdmissionJournalGeneration != 0 && codexLaneAffinityJournalGeneration(storedLane) <= next.AffinityInvalidationGeneration {
				affinityRefreshes[identity] = struct{}{}
			}
		}
		if codexLeaseCacheAdmission(old, result, exists) {
			cacheAdmissions[identity] = result
		}
		if identity == accountUnavailable {
			accountUnavailableHash, err = codexLeaseAccountUnavailableTransitionHash(old, exists, result)
			if err != nil {
				return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
			}
			if identity == quotaExhausted {
				quotaExhaustedHash = accountUnavailableHash
			}
		}
		requestCompleted = requestCompleted || codexLeaseRequestCompleted(old, exists, result)
		if hash, recovered := codexLeaseQuotaProbeRecoveryHash(old, exists, result); recovered {
			if quotaProbeRecoveryHash != "" && !constantTimeCodexLeaseDigestEqual(quotaProbeRecoveryHash, hash) {
				return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: one mutation completed multiple quota probes", ErrCodexLeaseInvalidMutation)
			}
			quotaProbeRecoveryHash = hash
		}
		if exists {
			next.Records[index] = result
		} else {
			recordIndex[identity] = len(next.Records)
			next.Records = append(next.Records, result)
		}
	}

	if len(deleteIDs) != 0 {
		kept := next.Records[:0]
		for _, record := range next.Records {
			if _, remove := deleteIDs[record.Identity()]; remove {
				continue
			}
			kept = append(kept, record)
		}
		next.Records = kept
		recordIndex = make(map[CodexJournalRecordIdentity]int, len(next.Records))
		for index, record := range next.Records {
			recordIndex[record.Identity()] = index
		}
	}

	desiredLane := storedLane
	if mutation.Lane != nil {
		desiredLane = *mutation.Lane
		if laneExists {
			copyCodexLaneAffinity(&desiredLane, storedLane)
			if sameCodexLaneRequestScope(storedLane, desiredLane) {
				desiredLane.RequestUnavailableAccountHashes = cloneCodexLeaseSlice(storedLane.RequestUnavailableAccountHashes)
			}
			desiredLane.QuotaExhaustedAccountHashes = cloneCodexLeaseSlice(storedLane.QuotaExhaustedAccountHashes)
		}
	}
	if !laneExists && mutation.Lane == nil {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: new lane requires an explicit after-image", ErrCodexLeaseInvalidMutation)
	}
	if restartingFailedLast {
		desiredLane.CurrentTurnHash = beginRequest.TurnDigest
		desiredLane.CurrentModeEpoch = beginRequest.ModeEpoch
		desiredLane.CurrentAuthoritative = beginRequest.Authoritative
		desiredLane.RequestUnavailableAccountHashes = nil
	}
	if accountUnavailableHash != "" && !slices.Contains(desiredLane.RequestUnavailableAccountHashes, accountUnavailableHash) {
		desiredLane.RequestUnavailableAccountHashes = append(desiredLane.RequestUnavailableAccountHashes, accountUnavailableHash)
		sort.Strings(desiredLane.RequestUnavailableAccountHashes)
	}
	if quotaExhaustedHash != "" {
		if !slices.Contains(desiredLane.QuotaExhaustedAccountHashes, quotaExhaustedHash) {
			desiredLane.QuotaExhaustedAccountHashes = append(desiredLane.QuotaExhaustedAccountHashes, quotaExhaustedHash)
			sort.Strings(desiredLane.QuotaExhaustedAccountHashes)
		}
	}
	if requestCompleted {
		desiredLane.RequestUnavailableAccountHashes = nil
	}
	if !completeUnavailableCycle.IsZero() {
		desiredLane.RequestUnavailableAccountHashes = nil
	}
	if quotaProbeRecoveryHash != "" {
		for index, accountHash := range desiredLane.QuotaExhaustedAccountHashes {
			if !constantTimeCodexLeaseDigestEqual(accountHash, quotaProbeRecoveryHash) {
				continue
			}
			desiredLane.QuotaExhaustedAccountHashes = append(desiredLane.QuotaExhaustedAccountHashes[:index], desiredLane.QuotaExhaustedAccountHashes[index+1:]...)
			break
		}
	}
	if !beginRequest.IsZero() {
		if err := validateCodexLeaseBeginRequestAvailability(desiredLane, next.Records[recordIndex[beginRequest]]); err != nil {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
		}
	}
	for _, record := range mutation.UpsertRecords {
		identity := record.Identity()
		stored := next.Records[recordIndex[identity]]
		if stored.State == LeaseFailedUnadmitted && (storedCurrent == identity || codexLaneTupleIdentity(desiredLane, true) == identity) {
			desiredLane.CurrentTurnHash = ""
			desiredLane.CurrentModeEpoch = 0
			desiredLane.CurrentAuthoritative = false
			desiredLane.LastTurnHash = identity.TurnDigest
			desiredLane.LastModeEpoch = identity.ModeEpoch
			desiredLane.LastAuthoritative = identity.Authoritative
		}
	}
	if codexJournalLaneDigest(desiredLane.SessionHash, desiredLane.ThreadHash, desiredLane.NamespaceHash) != targetLane {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: lane after-image changed lane identity", ErrCodexLeaseInvalidMutation)
	}
	if err := validateCodexLeaseHeadTransition(laneExists, storedCurrent, storedLast, desiredLane, originalRecords, recordIndex, next.Records, upsertIDs); err != nil {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
	}
	for identity, admitted := range firstAdmissions {
		if !codexLeaseAffinityEligible(admitted) {
			continue
		}
		if identity != codexLaneTupleIdentity(desiredLane, true) {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: first turn admission is not lane current", ErrCodexLeaseInvalidMutation)
		}
		desiredLane.LastAdmittedAccountHash = admitted.AccountHash
		desiredLane.LastAdmittedTurnHash = admitted.TurnHash
		desiredLane.LastAdmittedModeEpoch = admitted.ModeEpoch
		desiredLane.LastAdmittedAuthoritative = admitted.Authoritative
		desiredLane.LastAdmissionJournalGeneration = admitted.AdmissionJournalGeneration
		desiredLane.LastAdmittedAt = admitted.AdmittedAt
		if _, refreshed := affinityRefreshes[identity]; refreshed {
			desiredLane.AffinityRefreshJournalGeneration = next.Generation + 1
		} else if admitted.AdmissionJournalGeneration == next.Generation+1 {
			desiredLane.AffinityRefreshJournalGeneration = 0
		}
	}
	for identity, admitted := range cacheAdmissions {
		if !codexLeaseAffinityEligible(admitted) {
			continue
		}
		if identity != codexLaneTupleIdentity(desiredLane, true) {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: cache admission is not lane current", ErrCodexLeaseInvalidMutation)
		}
		desiredLane.LastCacheAdmittedAt = now
		desiredLane.LastCacheEffectiveModel = admitted.EffectiveModel
	}
	pointerChanged := !sameCodexLanePointers(storedLane, desiredLane)
	var laneGeneration uint64
	switch {
	case !laneExists:
		laneGeneration = 1
	case pointerChanged || predecessorChanged:
		if storedLane.Generation == math.MaxUint64 {
			return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: lane generation overflow", ErrCodexLeaseInvalidMutation)
		}
		laneGeneration = storedLane.Generation + 1
	default:
		laneGeneration = storedLane.Generation
	}
	desiredLane.Generation = laneGeneration
	if mutation.Lane != nil || pointerChanged || predecessorChanged || len(mutation.UpsertRecords) != 0 || len(mutation.DeleteRecords) != 0 {
		desiredLane.LastObservedAt = now
	}
	if laneExists {
		next.Lanes[storedLaneIndex] = desiredLane
	} else {
		next.Lanes = append(next.Lanes, desiredLane)
	}
	for identity := range upsertIDs {
		index := recordIndex[identity]
		next.Records[index].LaneGeneration = laneGeneration
	}
	for identity := range upsertIDs {
		index := recordIndex[identity]
		record := &next.Records[index]
		if record.PredecessorTurnHash == "" {
			record.PredecessorGeneration = 0
			continue
		}
		predecessor := CodexJournalRecordIdentity{LaneDigest: targetLane, TurnDigest: record.PredecessorTurnHash, ModeEpoch: record.PredecessorModeEpoch, Authoritative: record.PredecessorAuthoritative}
		predecessorIndex, ok := recordIndex[predecessor]
		if !ok {
			old, existed := originalRecords[identity]
			if !existed || old.PredecessorTurnHash != record.PredecessorTurnHash || old.PredecessorModeEpoch != record.PredecessorModeEpoch || old.PredecessorAuthoritative != record.PredecessorAuthoritative || old.PredecessorGeneration == 0 {
				return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: cannot manufacture an absent predecessor link", ErrCodexLeaseInvalidMutation)
			}
			if prior, existed := originalRecords[predecessor]; existed && prior.RecordGeneration != old.PredecessorGeneration {
				return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, fmt.Errorf("%w: stored predecessor fence changed", ErrCodexLeaseTrustLost)
			}
			record.PredecessorGeneration = old.PredecessorGeneration
			continue
		}
		record.PredecessorGeneration = next.Records[predecessorIndex].RecordGeneration
	}

	next.Generation++
	canonicaliseCodexLeaseV2(&next)
	post, err := buildCodexLeasePostFence(next, targetLane, expected.TouchedRecords, appendGenerations)
	if err != nil {
		return codexLeaseJournalEnvelopeV2{}, CodexLeaseGenerationFence{}, err
	}
	return next, post, nil
}

func validateCodexLeaseHeadTransition(laneExists bool, storedCurrent, storedLast CodexJournalRecordIdentity, desiredLane CodexJournalLane, originalRecords map[CodexJournalRecordIdentity]CodexJournalRecordV2, recordIndex map[CodexJournalRecordIdentity]int, records []CodexJournalRecordV2, upsertIDs map[CodexJournalRecordIdentity]struct{}) error {
	desiredCurrent := codexLaneTupleIdentity(desiredLane, true)
	desiredLast := codexLaneTupleIdentity(desiredLane, false)
	newRecords := make([]CodexJournalRecordIdentity, 0, 1)
	for identity := range upsertIDs {
		if _, exists := originalRecords[identity]; !exists {
			newRecords = append(newRecords, identity)
		}
	}
	if !laneExists {
		if len(newRecords) != 1 || desiredCurrent != newRecords[0] || desiredLast != newRecords[0] {
			return fmt.Errorf("%w: new lane must point current and last at its sole reserving record", ErrCodexLeaseInvalidMutation)
		}
		created := records[recordIndex[newRecords[0]]]
		if created.PredecessorTurnHash != "" {
			return fmt.Errorf("%w: first lane record cannot name a predecessor", ErrCodexLeaseInvalidMutation)
		}
		return nil
	}

	if len(newRecords) == 0 {
		if desiredCurrent == storedCurrent && desiredLast == storedLast {
			return nil
		}
		if storedCurrent.IsZero() && desiredCurrent == storedLast && desiredLast == storedLast {
			prior, existed := originalRecords[storedLast]
			_, updated := upsertIDs[storedLast]
			if existed && updated && codexLeaseRestartableFailedHead(prior) {
				return nil
			}
		}
		if desiredCurrent.IsZero() && desiredLast == storedLast && !storedCurrent.IsZero() {
			current, exists := recordIndex[storedCurrent]
			if exists && records[current].State == LeaseFailedUnadmitted {
				return nil
			}
		}
		return fmt.Errorf("%w: existing lane head cannot be cleared, replaced, or resurrected without a new successor", ErrCodexLeaseInvalidMutation)
	}
	if len(newRecords) != 1 {
		return fmt.Errorf("%w: lane mutation may create only one successor", ErrCodexLeaseInvalidMutation)
	}
	successorIdentity := newRecords[0]
	if desiredCurrent != successorIdentity || desiredLast != successorIdentity {
		return fmt.Errorf("%w: new record must become both current and last", ErrCodexLeaseInvalidMutation)
	}
	if storedLast.IsZero() {
		return fmt.Errorf("%w: successor requires the prior lane last record", ErrCodexLeaseInvalidMutation)
	}
	successor := records[recordIndex[successorIdentity]]
	if successor.PredecessorTurnHash != storedLast.TurnDigest || successor.PredecessorModeEpoch != storedLast.ModeEpoch || successor.PredecessorAuthoritative != storedLast.Authoritative {
		return fmt.Errorf("%w: successor does not name the prior lane last record", ErrCodexLeaseInvalidMutation)
	}
	predecessorIndex, exists := recordIndex[storedLast]
	if !exists {
		return fmt.Errorf("%w: successor predecessor is absent", ErrCodexLeaseInvalidMutation)
	}
	predecessor := records[predecessorIndex]
	if storedCurrent.IsZero() {
		if predecessor.State != LeaseFailedUnadmitted && predecessor.State != LeaseSuperseded && predecessor.State != LeaseExpired {
			return fmt.Errorf("%w: successor follows a non-terminal last record", ErrCodexLeaseInvalidMutation)
		}
		return nil
	}
	if storedCurrent != storedLast {
		return fmt.Errorf("%w: lane current and last authority diverged", ErrCodexLeaseTrustLost)
	}
	prior := originalRecords[storedLast]
	if prior.State != LeaseContinuationPending && prior.State != LeaseBoundQuiescent && prior.State != LeaseOrphaned && !codexLeaseAbandonedUnadmittedHead(prior) {
		return fmt.Errorf("%w: predecessor work is not replaceable", ErrCodexLeaseInvalidMutation)
	}
	if _, updated := upsertIDs[storedLast]; !updated || predecessor.State != LeaseSuperseded || predecessor.RoutingRefs != 0 || predecessor.AttemptRefs != 0 || predecessor.ResponseObserverRefs != 0 {
		return fmt.Errorf("%w: predecessor must be fenced, drained, and superseded with its successor", ErrCodexLeaseInvalidMutation)
	}
	return nil
}

func (store *CodexLeaseStore) buildCodexLeaseRecordAfterImage(old CodexJournalRecordV2, exists bool, input CodexJournalRecordV2, beginRequest, migrateTurnStateLatch, affinityInvalidationReset bool, fence CodexLeaseRecordFence, now time.Time) (CodexJournalRecordV2, uint64, bool, error) {
	result := cloneCodexJournalRecordV2(input)
	bindingReassignment := exists && codexLeaseAccountUnavailableAdmission(old, input)
	bindingReset := exists && beginRequest && (codexLeaseAccountUnavailableBeginRequest(old, input) || affinityInvalidationReset || codexLeaseRecordAllowsPortableReset(old))
	unadmittedBindingReset := bindingReset && !old.EverAdmitted
	if result.SessionHash == "" || result.ThreadHash == "" || result.NamespaceHash == "" || result.TurnHash == "" || result.ModeEpoch == 0 || result.ProtocolSchema != CurrentCodexLeaseSchema {
		return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: incomplete record identity/protocol", ErrCodexLeaseInvalidMutation)
	}
	if !exists {
		if beginRequest || result.State != LeaseReserving || result.AccountHash != "" || !codexCurrentRequestIsZero(result.CodexCurrentRequest) || result.AdoptedPrewarm || result.PrewarmAdoptionJournalGeneration != 0 {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: new record must begin as a clean reserving lease", ErrCodexLeaseInvalidMutation)
		}
		result.RecordGeneration = 1
		result.LeaseGeneration = 1
		result.CreatedAt = now
	} else {
		if old.AdoptedPrewarm != input.AdoptedPrewarm || old.PrewarmAdoptionJournalGeneration != input.PrewarmAdoptionJournalGeneration {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: prewarm adoption marker changed", ErrCodexLeaseInvalidMutation)
		}
		if old.RecordGeneration == math.MaxUint64 {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: record generation overflow", ErrCodexLeaseInvalidMutation)
		}
		provisionalUncertainty := !beginRequest && old.State == LeaseProvisional && input.State == LeaseOrphaned
		abandonedSuccessor := !beginRequest && input.State == LeaseSuperseded && codexLeaseAbandonedUnadmittedHead(old)
		if !provisionalUncertainty && !abandonedSuccessor && !beginRequest && old.State != input.State && !validLeaseTransition(old.State, input.State) {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: forbidden lease transition %s -> %s", ErrCodexLeaseInvalidMutation, old.State, input.State)
		}
		if old.PredecessorTurnHash != input.PredecessorTurnHash || old.PredecessorModeEpoch != input.PredecessorModeEpoch || old.PredecessorAuthoritative != input.PredecessorAuthoritative {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: frozen predecessor changed", ErrCodexLeaseInvalidMutation)
		}
		if old.NonMigratable && !input.NonMigratable && !bindingReset {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: non-migratable authority was cleared", ErrCodexLeaseInvalidMutation)
		}
		if old.HasEncryptedState && !input.HasEncryptedState {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: encrypted-state authority was cleared", ErrCodexLeaseInvalidMutation)
		}
		if old.HasTurnState && !input.HasTurnState {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: turn-state authority was cleared", ErrCodexLeaseInvalidMutation)
		}
		if !old.HasTurnState && input.HasTurnState && !input.TurnStateLatchCurrent {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: first turn-state authority is missing current latch marker", ErrCodexLeaseInvalidMutation)
		}
		validLatchMigration := migrateTurnStateLatch && beginRequest && old.HasTurnState && !old.TurnStateLatchCurrent && input.TurnStateLatchCurrent && old.EverAdmitted &&
			constantTimeCodexLeaseDigestEqual(old.AccountHash, input.AccountHash) && codexLeaseRuntimeCanBeginRequest(old)
		if old.TurnStateLatchCurrent != input.TurnStateLatchCurrent && !validLatchMigration && !(old.HasTurnState == false && input.HasTurnState && input.TurnStateLatchCurrent) {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: turn-state latch marker changed outside admission or migration", ErrCodexLeaseInvalidMutation)
		}
		if old.HasResponseAnchor && (!input.HasResponseAnchor || input.CorrelationHash == "") {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: response anchor was cleared", ErrCodexLeaseInvalidMutation)
		}
		result.RecordGeneration = old.RecordGeneration + 1
		result.CreatedAt = old.CreatedAt
		if sameCodexLeaseSemantics(old, input) {
			result.LeaseGeneration = old.LeaseGeneration
		} else {
			if old.LeaseGeneration == math.MaxUint64 {
				return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: lease generation overflow", ErrCodexLeaseInvalidMutation)
			}
			result.LeaseGeneration = old.LeaseGeneration + 1
		}
		if (old.EverAdmitted || old.NonMigratable) && !constantTimeCodexLeaseDigestEqual(old.AccountHash, input.AccountHash) && !bindingReassignment && !unadmittedBindingReset {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: admitted account changed", ErrCodexLeaseInvalidMutation)
		}
		result.EverAdmitted = old.EverAdmitted
		result.AdmissionJournalGeneration = old.AdmissionJournalGeneration
		result.AdmissionRequestGeneration = old.AdmissionRequestGeneration
		result.AdmissionRequestKind = old.AdmissionRequestKind
		result.AdmissionCompactionPhase = old.AdmissionCompactionPhase
		result.AdmittedAt = old.AdmittedAt
	}
	result.LastObservedAt = now
	var appended uint64
	if beginRequest {
		request, err := store.buildCodexBeginRequestAfterImage(old, result, affinityInvalidationReset, fence, now)
		if err != nil {
			return CodexJournalRecordV2{}, 0, false, err
		}
		result.CodexCurrentRequest = request
		appended = 1
	} else {
		if exists && !sameCodexCurrentRequestPlan(old.CodexCurrentRequest, input.CodexCurrentRequest) {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: current request plan changed without BeginRequest", ErrCodexLeaseInvalidMutation)
		}
		attempts, current, nextAttempt, err := store.buildCodexLeaseAttemptAfterImages(old.Attempts, old.CurrentAttemptGeneration, input.Attempts, input.CurrentAttemptGeneration, fence.TouchedAttempts, old.Generation, result.AttemptEnvelope, now)
		if err != nil {
			return CodexJournalRecordV2{}, 0, false, err
		}
		if nextAttempt != 0 {
			previousCurrent, found := codexLeaseAttemptByGeneration(attempts, old.CurrentAttemptGeneration)
			appendedAttempt, appendedFound := codexLeaseAttemptByGeneration(attempts, nextAttempt)
			pinnedAccountChanged := old.NonMigratable && !constantTimeCodexLeaseDigestEqual(old.AccountHash, result.AccountHash)
			unadmittedReplacement := !old.EverAdmitted && old.State == LeaseProvisional && result.State == LeaseProvisional
			admittedReplacement := old.EverAdmitted && old.Generation > old.AdmissionRequestGeneration &&
				old.State == LeaseBoundActive && result.State == LeaseBoundActive &&
				constantTimeCodexLeaseDigestEqual(old.AccountHash, result.AccountHash) && appendedFound &&
				appendedAttempt.Slot > 0 && int(appendedAttempt.Slot) <= len(result.AttemptEnvelope.Slots) &&
				old.RoutingRefs == result.RoutingRefs && old.AttemptRefs == result.AttemptRefs &&
				old.ResponseObserverRefs == result.ResponseObserverRefs && old.SocketLineageExtinct == result.SocketLineageExtinct
			sameAccountReplacement := admittedReplacement && found && previousCurrent.State == CodexAttemptProviderFailed &&
				previousCurrent.Slot > 0 && int(previousCurrent.Slot) <= len(result.AttemptEnvelope.Slots) &&
				constantTimeCodexLeaseDigestEqual(result.AttemptEnvelope.Slots[appendedAttempt.Slot-1].AccountHash, result.AttemptEnvelope.Slots[previousCurrent.Slot-1].AccountHash)
			unavailableReplacement := admittedReplacement && !old.NonMigratable && found && previousCurrent.State == CodexAttemptAccountUnavailable
			unadmittedTerminal := unadmittedReplacement && found && (previousCurrent.State == CodexAttemptProviderFailed || previousCurrent.State == CodexAttemptAccountUnavailable)
			if pinnedAccountChanged || (!unadmittedTerminal && !sameAccountReplacement && !unavailableReplacement) {
				return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: terminal request replacement requires BeginRequest", ErrCodexLeaseInvalidMutation)
			}
		}
		result.Attempts = attempts
		result.CurrentAttemptGeneration = current
		appended = nextAttempt
	}
	turnStateChanged := input.HasTurnState && (!old.HasTurnState || !constantTimeCodexLeaseDigestEqual(old.TurnStateHash, input.TurnStateHash))
	if exists && turnStateChanged {
		oldAttempt, oldFound := codexLeaseAttemptByGeneration(old.Attempts, old.CurrentAttemptGeneration)
		resultAttempt, resultFound := codexLeaseAttemptByGeneration(result.Attempts, result.CurrentAttemptGeneration)
		oldAdmitted := oldAttempt.State == CodexAttemptDispatched || oldAttempt.State == CodexAttemptStreaming
		validRelatch := migrateTurnStateLatch && beginRequest && old.HasTurnState && !old.TurnStateLatchCurrent && result.TurnStateLatchCurrent && old.EverAdmitted &&
			constantTimeCodexLeaseDigestEqual(old.AccountHash, result.AccountHash) && codexLeaseRuntimeCanBeginRequest(old)
		if !validRelatch && (beginRequest || !oldFound || !resultFound || !oldAdmitted || resultAttempt.State != CodexAttemptStreaming || result.CurrentAttemptGeneration != old.CurrentAttemptGeneration || !codexLeaseExactCurrentAttemptFence(fence, old)) {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: turn-state authority changed outside provider admission", ErrCodexLeaseInvalidMutation)
		}
	}
	if codexLeaseCurrentAttemptState(result) == CodexAttemptIndeterminate && !result.NonMigratable {
		result.NonMigratable = true
		if exists && result.LeaseGeneration == old.LeaseGeneration {
			if old.LeaseGeneration == math.MaxUint64 {
				return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: lease generation overflow", ErrCodexLeaseInvalidMutation)
			}
			result.LeaseGeneration++
		}
	}
	accountChanged := old.AccountHash != "" || result.AccountHash != ""
	accountChanged = accountChanged && !constantTimeCodexLeaseDigestEqual(old.AccountHash, result.AccountHash)
	if exists && accountChanged && !beginRequest && !bindingReassignment {
		appendedAttempt, found := codexLeaseAttemptByGeneration(result.Attempts, appended)
		if old.EverAdmitted || old.NonMigratable || appended == 0 || !found || result.CurrentAttemptGeneration != appended || appendedAttempt.State != CodexAttemptPrepared || appendedAttempt.Slot == 0 || int(appendedAttempt.Slot) > len(result.AttemptEnvelope.Slots) || !constantTimeCodexLeaseDigestEqual(result.AttemptEnvelope.Slots[appendedAttempt.Slot-1].AccountHash, result.AccountHash) {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: account change is not coupled to a prepared replacement attempt", ErrCodexLeaseInvalidMutation)
		}
	}
	if exists && old.State == LeaseProvisional && result.State == LeaseOrphaned {
		oldAttempt, oldFound := codexLeaseAttemptByGeneration(old.Attempts, old.CurrentAttemptGeneration)
		resultAttempt, resultFound := codexLeaseAttemptByGeneration(result.Attempts, old.CurrentAttemptGeneration)
		expected := cloneCodexJournalRecordV2(old)
		expected.State = LeaseOrphaned
		expected.RoutingRefs = 0
		expected.AttemptRefs = 0
		expected.ResponseObserverRefs = 1
		expected.NonMigratable = true
		for index := range expected.Attempts {
			if expected.Attempts[index].Generation == expected.CurrentAttemptGeneration {
				expected.Attempts[index].State = CodexAttemptIndeterminate
				break
			}
		}
		if beginRequest || !oldFound || !resultFound || !codexLeaseExactCurrentAttemptFence(fence, old) || old.CurrentAttemptGeneration == 0 || result.CurrentAttemptGeneration != old.CurrentAttemptGeneration || oldAttempt.State != CodexAttemptDispatched || resultAttempt.State != CodexAttemptIndeterminate || oldAttempt.Slot != resultAttempt.Slot || !constantTimeCodexLeaseDigestEqual(old.AccountHash, result.AccountHash) || old.Generation != result.Generation || old.RoutingRefs != 1 || old.AttemptRefs != 0 || old.ResponseObserverRefs != 0 || old.SocketLineageExtinct || result.RoutingRefs != 0 || result.AttemptRefs != 0 || result.ResponseObserverRefs != 1 || result.SocketLineageExtinct || !result.NonMigratable || !sameCodexLeaseSemantics(expected, result) {
			return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: provisional uncertainty lacks an exact dispatched after-image", ErrCodexLeaseInvalidMutation)
		}
	}
	if exists {
		oldAttempt, oldFound := codexLeaseAttemptByGeneration(old.Attempts, old.CurrentAttemptGeneration)
		resultAttempt, resultFound := codexLeaseAttemptByGeneration(result.Attempts, old.CurrentAttemptGeneration)
		if oldFound && resultFound && oldAttempt.State == CodexAttemptPrepared && resultAttempt.State == CodexAttemptAbandonedBeforeDispatch {
			expected := cloneCodexJournalRecordV2(old)
			expected.State = LeaseProvisional
			if old.EverAdmitted {
				expected.State = LeaseOrphaned
			}
			expected.RoutingRefs = 0
			expected.AttemptRefs = 0
			expected.ResponseObserverRefs = 0
			expected.SocketLineageExtinct = true
			for index := range expected.Attempts {
				if expected.Attempts[index].Generation == expected.CurrentAttemptGeneration {
					expected.Attempts[index].State = CodexAttemptAbandonedBeforeDispatch
					break
				}
			}
			validOldState := (!old.EverAdmitted && old.State == LeaseProvisional) || (old.EverAdmitted && old.State == LeaseBoundActive)
			if beginRequest || !validOldState || !codexLeaseExactCurrentAttemptFence(fence, old) || result.CurrentAttemptGeneration != old.CurrentAttemptGeneration || oldAttempt.Slot != resultAttempt.Slot || !sameCodexLeaseSemantics(expected, result) {
				return CodexJournalRecordV2{}, 0, false, fmt.Errorf("%w: prepared abandonment lacks an exact terminal after-image", ErrCodexLeaseInvalidMutation)
			}
		}
	}
	firstAdmission := !result.EverAdmitted && codexLeaseCurrentRequestCacheEligible(result) && result.State == LeaseBoundActive && codexLeaseCurrentAttemptState(result) == CodexAttemptStreaming
	if firstAdmission {
		result.EverAdmitted = true
		result.AdmissionJournalGeneration = store.v2.Generation + 1
		result.AdmissionRequestGeneration = result.Generation
		result.AdmissionRequestKind = result.RequestKind
		result.AdmissionCompactionPhase = result.CompactionPhase
		result.AdmittedAt = now
	}
	return result, appended, firstAdmission || bindingReassignment, nil
}

func codexLeaseAccountUnavailableAdmission(old, desired CodexJournalRecordV2) bool {
	validState := (!old.EverAdmitted && old.State == LeaseProvisional) || (old.EverAdmitted && old.State == LeaseBoundActive)
	if !validState || old.NonMigratable || desired.State != LeaseBoundActive || old.Generation != desired.Generation || old.CurrentAttemptGeneration == 0 || old.CurrentAttemptGeneration != desired.CurrentAttemptGeneration || constantTimeCodexLeaseDigestEqual(old.AccountHash, desired.AccountHash) {
		return false
	}
	current, currentFound := codexLeaseAttemptByGeneration(old.Attempts, old.CurrentAttemptGeneration)
	desiredCurrent, found := codexLeaseAttemptByGeneration(desired.Attempts, desired.CurrentAttemptGeneration)
	if !currentFound || current.State != CodexAttemptDispatched || !found || desiredCurrent.State != CodexAttemptStreaming || desiredCurrent.Slot != current.Slot || current.Slot == 0 || int(current.Slot) > len(old.AttemptEnvelope.Slots) || constantTimeCodexLeaseDigestEqual(old.AttemptEnvelope.Slots[current.Slot-1].AccountHash, old.AccountHash) {
		return false
	}
	return constantTimeCodexLeaseDigestEqual(old.AttemptEnvelope.Slots[current.Slot-1].AccountHash, desired.AccountHash)
}

func codexLeaseAccountUnavailableBeginRequest(old, desired CodexJournalRecordV2) bool {
	if !codexLeaseAccountUnavailableCanBeginRequest(old) || desired.NonMigratable {
		return false
	}
	if old.EverAdmitted {
		return desired.State == LeaseBoundActive
	}
	return desired.State == LeaseProvisional
}

func codexLeaseAffinityInvalidationBeginRequest(old, desired CodexJournalRecordV2, lane CodexJournalLane, invalidationGeneration uint64) bool {
	if invalidationGeneration == 0 || lane.LastAdmissionJournalGeneration == 0 || lane.LastAdmissionJournalGeneration > invalidationGeneration ||
		!old.EverAdmitted || codexLeaseRecordRequiresAccount(old) || desired.NonMigratable ||
		len(desired.AttemptEnvelope.Slots) == 0 || len(desired.Attempts) != 1 {
		return false
	}
	attempt := desired.Attempts[0]
	return attempt.Slot > 0 && int(attempt.Slot) <= len(desired.AttemptEnvelope.Slots) &&
		!constantTimeCodexLeaseDigestEqual(old.AccountHash, desired.AttemptEnvelope.Slots[attempt.Slot-1].AccountHash)
}

func codexLeaseAccountUnavailableCanBeginRequest(old CodexJournalRecordV2) bool {
	currentState := codexLeaseCurrentAttemptState(old)
	accountUnavailable := currentState == CodexAttemptAccountUnavailable
	pendingPreparedRebind := old.EverAdmitted && old.State == LeaseOrphaned && currentState == CodexAttemptAbandonedBeforeDispatch && !old.NonMigratable && codexLeaseCurrentAttemptAccountDiffersFromBinding(old)
	if (!accountUnavailable && !pendingPreparedRebind) || old.RoutingRefs != 0 || old.AttemptRefs != 0 || old.ResponseObserverRefs != 0 || !old.SocketLineageExtinct {
		return false
	}
	if old.EverAdmitted {
		return old.State == LeaseBoundQuiescent || old.State == LeaseOrphaned
	}
	return old.State == LeaseFailedUnadmitted && codexLeaseRestartableFailedHead(old)
}

func codexLeaseCurrentAttemptAccountDiffersFromBinding(record CodexJournalRecordV2) bool {
	current, found := codexLeaseAttemptByGeneration(record.Attempts, record.CurrentAttemptGeneration)
	return found && current.Slot > 0 && int(current.Slot) <= len(record.AttemptEnvelope.Slots) &&
		!constantTimeCodexLeaseDigestEqual(record.AttemptEnvelope.Slots[current.Slot-1].AccountHash, record.AccountHash)
}

func codexLeaseCacheAdmission(old, result CodexJournalRecordV2, exists bool) bool {
	if !exists || !codexLeaseCurrentRequestCacheRefreshEligible(old, result) || old.Generation != result.Generation || old.CurrentAttemptGeneration == 0 || old.CurrentAttemptGeneration != result.CurrentAttemptGeneration {
		return false
	}
	before, beforeFound := codexLeaseAttemptByGeneration(old.Attempts, old.CurrentAttemptGeneration)
	after, afterFound := codexLeaseAttemptByGeneration(result.Attempts, result.CurrentAttemptGeneration)
	return beforeFound && afterFound && before.State == CodexAttemptDispatched && after.State == CodexAttemptStreaming
}

func codexLeaseAccountUnavailableTransitionHash(old CodexJournalRecordV2, exists bool, result CodexJournalRecordV2) (string, error) {
	if !exists || old.Generation == 0 || old.Generation != result.Generation || old.CurrentAttemptGeneration == 0 {
		return "", fmt.Errorf("%w: account unavailability lacks current request authority", ErrCodexLeaseInvalidMutation)
	}
	before, beforeFound := codexLeaseAttemptByGeneration(old.Attempts, old.CurrentAttemptGeneration)
	after, afterFound := codexLeaseAttemptByGeneration(result.Attempts, old.CurrentAttemptGeneration)
	if !beforeFound || !afterFound || before.State == CodexAttemptAccountUnavailable || after.State != CodexAttemptAccountUnavailable || before.Slot == 0 || int(before.Slot) > len(old.AttemptEnvelope.Slots) {
		return "", fmt.Errorf("%w: account unavailability is not an exact current-attempt transition", ErrCodexLeaseInvalidMutation)
	}
	return old.AttemptEnvelope.Slots[before.Slot-1].AccountHash, nil
}

func codexLeaseRequestCompleted(old CodexJournalRecordV2, exists bool, result CodexJournalRecordV2) bool {
	if !exists || old.Generation == 0 || old.Generation != result.Generation || old.CurrentAttemptGeneration == 0 || old.CurrentAttemptGeneration != result.CurrentAttemptGeneration {
		return false
	}
	before, beforeFound := codexLeaseAttemptByGeneration(old.Attempts, old.CurrentAttemptGeneration)
	after, afterFound := codexLeaseAttemptByGeneration(result.Attempts, result.CurrentAttemptGeneration)
	return beforeFound && afterFound && before.State == CodexAttemptStreaming && after.State == CodexAttemptProviderCompleted
}

func sameCodexLaneRequestScope(left, right CodexJournalLane) bool {
	return codexLaneRequestScopeIdentity(left) == codexLaneRequestScopeIdentity(right)
}

func codexLaneRequestScopeIdentity(lane CodexJournalLane) CodexJournalRecordIdentity {
	if current := codexLaneTupleIdentity(lane, true); !current.IsZero() {
		return current
	}
	return codexLaneTupleIdentity(lane, false)
}

func codexLeaseQuotaProbeRecoveryHash(old CodexJournalRecordV2, exists bool, result CodexJournalRecordV2) (string, bool) {
	if !exists || !old.QuotaExhaustionProbe || old.Generation == 0 || old.Generation != result.Generation || old.CurrentAttemptGeneration == 0 || old.CurrentAttemptGeneration != result.CurrentAttemptGeneration {
		return "", false
	}
	before, beforeFound := codexLeaseAttemptByGeneration(old.Attempts, old.CurrentAttemptGeneration)
	after, afterFound := codexLeaseAttemptByGeneration(result.Attempts, result.CurrentAttemptGeneration)
	if !beforeFound || !afterFound || before.State != CodexAttemptStreaming || after.State != CodexAttemptProviderCompleted || before.Slot == 0 || int(before.Slot) > len(old.AttemptEnvelope.Slots) {
		return "", false
	}
	return old.AttemptEnvelope.Slots[before.Slot-1].AccountHash, true
}

func validateCodexLeaseBeginRequestAvailability(lane CodexJournalLane, result CodexJournalRecordV2) error {
	current, found := codexLeaseAttemptByGeneration(result.Attempts, result.CurrentAttemptGeneration)
	if !found || current.Slot == 0 || int(current.Slot) > len(result.AttemptEnvelope.Slots) {
		return fmt.Errorf("%w: BeginRequest current attempt is unavailable", ErrCodexLeaseInvalidMutation)
	}
	containsHash := func(hashes []string, accountHash string) bool {
		for _, unavailable := range hashes {
			if constantTimeCodexLeaseDigestEqual(unavailable, accountHash) {
				return true
			}
		}
		return false
	}
	if result.QuotaExhaustionProbe {
		selectedHash := result.AttemptEnvelope.Slots[current.Slot-1].AccountHash
		if !containsHash(lane.QuotaExhaustedAccountHashes, selectedHash) {
			return fmt.Errorf("%w: quota probe does not name one exhausted account", ErrCodexLeaseInvalidMutation)
		}
		for _, slot := range result.AttemptEnvelope.Slots {
			if !constantTimeCodexLeaseDigestEqual(slot.AccountHash, selectedHash) {
				return fmt.Errorf("%w: quota probe spans multiple accounts", ErrCodexLeaseInvalidMutation)
			}
		}
		return nil
	}
	requestUnavailable := []string(nil)
	if result.Identity() == codexLaneRequestScopeIdentity(lane) {
		requestUnavailable = lane.RequestUnavailableAccountHashes
	}
	for _, slot := range result.AttemptEnvelope.Slots {
		if containsHash(lane.QuotaExhaustedAccountHashes, slot.AccountHash) || containsHash(requestUnavailable, slot.AccountHash) {
			return fmt.Errorf("%w: BeginRequest reuses an unavailable account", ErrCodexLeaseInvalidMutation)
		}
	}
	return nil
}

func codexLeaseCurrentRequestCacheRefreshEligible(old, result CodexJournalRecordV2) bool {
	if codexLeaseCurrentRequestCacheEligible(result) {
		return true
	}
	return old.EverAdmitted && result.EverAdmitted && result.RequestKind == CodexRequestCompaction && result.CompactionPhase == CodexCompactionMidTurn && constantTimeCodexLeaseDigestEqual(old.AccountHash, result.AccountHash)
}

func (store *CodexLeaseStore) buildCodexBeginRequestAfterImage(old, desired CodexJournalRecordV2, affinityInvalidationReset bool, fence CodexLeaseRecordFence, now time.Time) (CodexCurrentRequest, error) {
	request := cloneCodexCurrentRequest(desired.CodexCurrentRequest)
	bindingReset := codexLeaseAccountUnavailableBeginRequest(old, desired) || affinityInvalidationReset || codexLeaseRecordAllowsPortableReset(old)
	unadmittedBindingReset := bindingReset && !old.EverAdmitted
	if request.Generation != 0 || request.CurrentAttemptGeneration != 0 || len(request.Attempts) != 1 {
		return CodexCurrentRequest{}, fmt.Errorf("%w: BeginRequest requires a store-owned generation and one prepared attempt", ErrCodexLeaseInvalidMutation)
	}
	if len(fence.TouchedAttempts) != 1 || fence.TouchedAttempts[0].RequestGeneration != 0 || fence.TouchedAttempts[0].Generation != 0 || fence.TouchedAttempts[0].Revision != 0 {
		return CodexCurrentRequest{}, fmt.Errorf("%w: BeginRequest requires an absent request-attempt fence", ErrCodexLeaseInvalidMutation)
	}
	if old.Generation == math.MaxUint64 {
		return CodexCurrentRequest{}, fmt.Errorf("%w: request generation overflow", ErrCodexLeaseInvalidMutation)
	}
	if old.Generation == 0 {
		if old.State != LeaseReserving || !codexCurrentRequestIsZero(old.CodexCurrentRequest) {
			return CodexCurrentRequest{}, fmt.Errorf("%w: initial BeginRequest requires a clean reservation", ErrCodexLeaseInvalidMutation)
		}
	} else {
		if old.RoutingRefs != 0 || old.AttemptRefs != 0 || old.ResponseObserverRefs != 0 || !codexLeaseAttemptTerminalForRequest(codexLeaseCurrentAttemptState(old)) {
			return CodexCurrentRequest{}, fmt.Errorf("%w: prior request is not terminal and drained", ErrCodexLeaseInvalidMutation)
		}
		allowed := old.State == LeaseContinuationPending || old.State == LeaseBoundQuiescent || old.State == LeaseOrphaned || (old.State == LeaseProvisional && codexLeaseCurrentAttemptState(old) == CodexAttemptAbandonedBeforeDispatch) || codexLeaseRestartableFailedHead(old)
		if !allowed {
			return CodexCurrentRequest{}, fmt.Errorf("%w: lease state cannot begin another request", ErrCodexLeaseInvalidMutation)
		}
	}
	wantState := LeaseProvisional
	if old.EverAdmitted {
		wantState = LeaseBoundActive
	}
	if desired.State != wantState {
		return CodexCurrentRequest{}, fmt.Errorf("%w: BeginRequest has invalid lease state", ErrCodexLeaseInvalidMutation)
	}
	if (old.EverAdmitted || old.NonMigratable) && !constantTimeCodexLeaseDigestEqual(old.AccountHash, desired.AccountHash) && !unadmittedBindingReset {
		return CodexCurrentRequest{}, fmt.Errorf("%w: bound request changed account", ErrCodexLeaseInvalidMutation)
	}
	if old.NonMigratable && !bindingReset {
		for _, slot := range request.AttemptEnvelope.Slots {
			if !constantTimeCodexLeaseDigestEqual(slot.AccountHash, desired.AccountHash) {
				return CodexCurrentRequest{}, fmt.Errorf("%w: bound request plan contains another account", ErrCodexLeaseInvalidMutation)
			}
		}
	}
	attempt := request.Attempts[0]
	if attempt.Generation != 0 || attempt.Revision != 0 || attempt.State != CodexAttemptPrepared || attempt.Slot == 0 || !attempt.CreatedAt.IsZero() || !attempt.LastObservedAt.IsZero() {
		return CodexCurrentRequest{}, fmt.Errorf("%w: BeginRequest attempt is not a clean prepared append", ErrCodexLeaseInvalidMutation)
	}
	if int(attempt.Slot) > len(request.AttemptEnvelope.Slots) {
		return CodexCurrentRequest{}, fmt.Errorf("%w: BeginRequest attempt slot is outside the frozen envelope", ErrCodexLeaseInvalidMutation)
	}
	if (old.EverAdmitted || old.NonMigratable) && !constantTimeCodexLeaseDigestEqual(request.AttemptEnvelope.Slots[attempt.Slot-1].AccountHash, desired.AccountHash) && !bindingReset {
		return CodexCurrentRequest{}, fmt.Errorf("%w: bound request changed account without account unavailability", ErrCodexLeaseInvalidMutation)
	}
	request.Generation = old.Generation + 1
	attempt.Generation = 1
	attempt.Revision = 1
	attempt.CreatedAt = now
	attempt.LastObservedAt = now
	request.Attempts = []CodexJournalAttempt{attempt}
	request.CurrentAttemptGeneration = 1
	return request, nil
}

func (store *CodexLeaseStore) buildCodexLeaseAttemptAfterImages(old []CodexJournalAttempt, oldCurrent uint64, desired []CodexJournalAttempt, desiredCurrent uint64, fences []CodexAttemptFence, requestGeneration uint64, envelope CodexAttemptEnvelope, now time.Time) ([]CodexJournalAttempt, uint64, uint64, error) {
	oldByGeneration := make(map[uint64]CodexJournalAttempt, len(old))
	var maxGeneration uint64
	for _, attempt := range old {
		if attempt.Generation == 0 {
			return nil, 0, 0, fmt.Errorf("%w: stored zero attempt generation", ErrCodexLeaseTrustLost)
		}
		oldByGeneration[attempt.Generation] = attempt
		maxGeneration = max(maxGeneration, attempt.Generation)
	}
	fenceByGeneration := make(map[uint64]CodexAttemptFence, len(fences))
	for _, fence := range fences {
		if fence.RequestGeneration != requestGeneration {
			return nil, 0, 0, staleCodexLeaseMutation("attempt_request_generation", fmt.Sprintf("have %d, expected %d", requestGeneration, fence.RequestGeneration))
		}
		if _, duplicate := fenceByGeneration[fence.Generation]; duplicate {
			return nil, 0, 0, fmt.Errorf("%w: duplicate attempt fence", ErrCodexLeaseInvalidMutation)
		}
		fenceByGeneration[fence.Generation] = fence
	}
	desiredByGeneration := make(map[uint64]struct{}, len(desired))
	result := make([]CodexJournalAttempt, 0, len(desired))
	appendIndex := -1
	for _, input := range desired {
		if input.Generation == 0 {
			if appendIndex >= 0 {
				return nil, 0, 0, fmt.Errorf("%w: more than one attempt append", ErrCodexLeaseInvalidMutation)
			}
			appendIndex = len(result)
			result = append(result, input)
			continue
		}
		if _, duplicate := desiredByGeneration[input.Generation]; duplicate {
			return nil, 0, 0, fmt.Errorf("%w: duplicate attempt generation", ErrCodexLeaseInvalidMutation)
		}
		desiredByGeneration[input.Generation] = struct{}{}
		previous, exists := oldByGeneration[input.Generation]
		if !exists {
			return nil, 0, 0, fmt.Errorf("%w: explicit attempt generation is store-owned", ErrCodexLeaseInvalidMutation)
		}
		if input.Slot != previous.Slot {
			return nil, 0, 0, fmt.Errorf("%w: attempt slot changed", ErrCodexLeaseInvalidMutation)
		}
		fence, touched := fenceByGeneration[input.Generation]
		if input.State != previous.State {
			if !touched {
				return nil, 0, 0, fmt.Errorf("%w: changed attempt lacks fence", ErrCodexLeaseInvalidMutation)
			}
			abandonedBeforeDispatch := previous.State == CodexAttemptPrepared && input.State == CodexAttemptAbandonedBeforeDispatch
			if !abandonedBeforeDispatch && !validCodexAttemptTransition(previous.State, input.State) {
				return nil, 0, 0, fmt.Errorf("%w: forbidden attempt transition", ErrCodexLeaseInvalidMutation)
			}
		}
		if !touched {
			result = append(result, previous)
			continue
		}
		if fence.Revision != previous.Revision {
			return nil, 0, 0, staleCodexLeaseMutation("attempt_revision", fmt.Sprintf("generation %d has %d, expected %d", input.Generation, previous.Revision, fence.Revision))
		}
		if previous.Revision == math.MaxUint64 {
			return nil, 0, 0, fmt.Errorf("%w: attempt revision overflow", ErrCodexLeaseInvalidMutation)
		}
		updated := previous
		updated.State = input.State
		updated.Revision++
		updated.LastObservedAt = now
		result = append(result, updated)
	}
	for generation := range oldByGeneration {
		if _, retained := desiredByGeneration[generation]; !retained {
			return nil, 0, 0, fmt.Errorf("%w: attempt deletion by omission", ErrCodexLeaseInvalidMutation)
		}
	}
	var appended uint64
	if appendIndex >= 0 {
		fence, ok := fenceByGeneration[0]
		if !ok || fence.Revision != 0 {
			return nil, 0, 0, fmt.Errorf("%w: attempt append lacks absent fence", ErrCodexLeaseInvalidMutation)
		}
		if result[appendIndex].State != CodexAttemptPrepared {
			return nil, 0, 0, fmt.Errorf("%w: appended attempt must be prepared", ErrCodexLeaseInvalidMutation)
		}
		if maxGeneration == math.MaxUint64 {
			return nil, 0, 0, fmt.Errorf("%w: attempt generation overflow", ErrCodexLeaseInvalidMutation)
		}
		if envelope.AttemptLimit == 0 || len(old) >= int(envelope.AttemptLimit) {
			return nil, 0, 0, ErrCodexLeaseAttemptLimit
		}
		if len(old) != 0 {
			previousCurrent, found := codexLeaseAttemptByGeneration(result, oldCurrent)
			if !found || !codexLeaseAttemptTerminalForRollover(previousCurrent.State) {
				return nil, 0, 0, fmt.Errorf("%w: previous current attempt is not terminal", ErrCodexLeaseInvalidMutation)
			}
		}
		appended = maxGeneration + 1
		result[appendIndex].Generation = appended
		result[appendIndex].Revision = 1
		result[appendIndex].CreatedAt = now
		result[appendIndex].LastObservedAt = now
		if desiredCurrent != 0 {
			return nil, 0, 0, fmt.Errorf("%w: appended attempt requires store-owned current generation", ErrCodexLeaseInvalidMutation)
		}
		desiredCurrent = appended
	}
	for generation, fence := range fenceByGeneration {
		if generation == 0 {
			continue
		}
		previous, exists := oldByGeneration[generation]
		if fence.Revision == 0 {
			if exists {
				return nil, 0, 0, staleCodexLeaseMutation("attempt_absence", fmt.Sprintf("generation %d exists", generation))
			}
			continue
		}
		if !exists || previous.Revision != fence.Revision {
			return nil, 0, 0, staleCodexLeaseMutation("attempt_revision", fmt.Sprintf("generation %d changed", generation))
		}
	}
	if desiredCurrent != 0 {
		found := false
		for _, attempt := range result {
			if attempt.Generation == desiredCurrent {
				found = true
				break
			}
		}
		if !found {
			return nil, 0, 0, fmt.Errorf("%w: current attempt is absent", ErrCodexLeaseInvalidMutation)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Generation < result[j].Generation })
	return result, desiredCurrent, appended, nil
}

func codexLeaseAttemptByGeneration(attempts []CodexJournalAttempt, generation uint64) (CodexJournalAttempt, bool) {
	for _, attempt := range attempts {
		if attempt.Generation == generation {
			return attempt, true
		}
	}
	return CodexJournalAttempt{}, false
}

func codexLeaseAttemptTerminalForRollover(state CodexAttemptState) bool {
	switch state {
	case CodexAttemptProviderCompleted, CodexAttemptProviderFailed, CodexAttemptIndeterminate, CodexAttemptAbandonedBeforeDispatch, CodexAttemptAccountUnavailable:
		return true
	default:
		return false
	}
}

func codexLeaseAttemptTerminalForRequest(state CodexAttemptState) bool {
	return codexLeaseAttemptTerminalForRollover(state) || state == CodexAttemptAbandonedBeforeDispatch
}

func codexLeaseExactCurrentAttemptFence(fence CodexLeaseRecordFence, record CodexJournalRecordV2) bool {
	if len(fence.TouchedAttempts) != 1 || record.CurrentAttemptGeneration == 0 {
		return false
	}
	attempt, found := codexLeaseAttemptByGeneration(record.Attempts, record.CurrentAttemptGeneration)
	if !found {
		return false
	}
	touched := fence.TouchedAttempts[0]
	return touched.RequestGeneration == record.Generation && touched.Generation == record.CurrentAttemptGeneration && touched.Revision == attempt.Revision
}

func validateCodexLaneMutationOwnedFields(mutation CodexLaneMutation) error {
	if mutation.Lane != nil && (mutation.Lane.Generation != 0 || !mutation.Lane.LastObservedAt.IsZero() || !codexLaneAffinityIsZero(*mutation.Lane) || len(mutation.Lane.RequestUnavailableAccountHashes) != 0 || len(mutation.Lane.QuotaExhaustedAccountHashes) != 0) {
		return fmt.Errorf("%w: caller supplied lane generation/timestamp", ErrCodexLeaseInvalidMutation)
	}
	for _, record := range mutation.UpsertRecords {
		if record.RecordGeneration != 0 || record.LaneGeneration != 0 || record.PredecessorGeneration != 0 || record.LeaseGeneration != 0 || !record.CreatedAt.IsZero() || !record.LastObservedAt.IsZero() || record.EverAdmitted || record.AdmissionJournalGeneration != 0 || record.AdmissionRequestGeneration != 0 || record.AdmissionRequestKind != "" || record.AdmissionCompactionPhase != "" || !record.AdmittedAt.IsZero() {
			return fmt.Errorf("%w: caller supplied record generation/timestamp", ErrCodexLeaseInvalidMutation)
		}
		for _, attempt := range record.Attempts {
			if attempt.Revision != 0 || !attempt.CreatedAt.IsZero() || !attempt.LastObservedAt.IsZero() {
				return fmt.Errorf("%w: caller supplied attempt revision/timestamp", ErrCodexLeaseInvalidMutation)
			}
		}
	}
	return nil
}

func codexLaneMutationTarget(mutation CodexLaneMutation) (string, error) {
	target := ""
	accept := func(digest string) error {
		if digest == "" {
			return fmt.Errorf("%w: empty target lane", ErrCodexLeaseInvalidMutation)
		}
		if target == "" {
			target = digest
			return nil
		}
		if target != digest {
			return fmt.Errorf("%w: one mutation spans multiple lanes", ErrCodexLeaseInvalidMutation)
		}
		return nil
	}
	if mutation.Lane != nil {
		if mutation.Lane.SessionHash == "" || mutation.Lane.ThreadHash == "" || mutation.Lane.NamespaceHash == "" {
			return "", fmt.Errorf("%w: incomplete lane identity", ErrCodexLeaseInvalidMutation)
		}
		if err := accept(codexJournalLaneDigest(mutation.Lane.SessionHash, mutation.Lane.ThreadHash, mutation.Lane.NamespaceHash)); err != nil {
			return "", err
		}
	}
	if mutation.CompleteUnavailableCycle != nil {
		if err := accept(mutation.CompleteUnavailableCycle.LaneDigest); err != nil {
			return "", err
		}
	}
	for _, record := range mutation.UpsertRecords {
		if err := accept(record.Identity().LaneDigest); err != nil {
			return "", err
		}
	}
	for _, identity := range mutation.DeleteRecords {
		if err := accept(identity.LaneDigest); err != nil {
			return "", err
		}
	}
	if target == "" {
		return "", fmt.Errorf("%w: mutation has no target lane", ErrCodexLeaseInvalidMutation)
	}
	return target, nil
}

func validateCodexLeaseRecordFences(fences []CodexLeaseRecordFence, records map[CodexJournalRecordIdentity]int, source []CodexJournalRecordV2) (map[CodexJournalRecordIdentity]CodexLeaseRecordFence, error) {
	result := make(map[CodexJournalRecordIdentity]CodexLeaseRecordFence, len(fences))
	for _, fence := range fences {
		if fence.Record.IsZero() {
			return nil, fmt.Errorf("%w: zero record fence identity", ErrCodexLeaseInvalidMutation)
		}
		if _, duplicate := result[fence.Record]; duplicate {
			return nil, fmt.Errorf("%w: duplicate record fence", ErrCodexLeaseInvalidMutation)
		}
		index, exists := records[fence.Record]
		if !exists {
			if fence.Revision != 0 || fence.Lease != 0 || fence.RequestGeneration != 0 || fence.CurrentAttempt != 0 {
				return nil, staleCodexLeaseMutation("record_absence", "record is absent")
			}
			for _, attempt := range fence.TouchedAttempts {
				if attempt.Generation != 0 || attempt.Revision != 0 {
					return nil, staleCodexLeaseMutation("attempt_absence", "record is absent")
				}
			}
			result[fence.Record] = cloneCodexLeaseRecordFence(fence)
			continue
		}
		record := source[index]
		if record.RecordGeneration != fence.Revision {
			return nil, staleCodexLeaseMutation("record_revision", fmt.Sprintf("have %d, expected %d", record.RecordGeneration, fence.Revision))
		}
		if record.LeaseGeneration != fence.Lease {
			return nil, staleCodexLeaseMutation("lease_generation", fmt.Sprintf("have %d, expected %d", record.LeaseGeneration, fence.Lease))
		}
		if record.Generation != fence.RequestGeneration {
			return nil, staleCodexLeaseMutation("request_generation", fmt.Sprintf("have %d, expected %d", record.Generation, fence.RequestGeneration))
		}
		if record.CurrentAttemptGeneration != fence.CurrentAttempt {
			return nil, staleCodexLeaseMutation("current_attempt", fmt.Sprintf("have %d, expected %d", record.CurrentAttemptGeneration, fence.CurrentAttempt))
		}
		result[fence.Record] = cloneCodexLeaseRecordFence(fence)
	}
	return result, nil
}

func (store *CodexLeaseStore) validateCodexLeaseV2State(envelope codexLeaseJournalEnvelopeV2) error {
	return store.validateCodexLeaseV2StateForSchema(envelope, CurrentCodexLeaseSchema)
}

func (store *CodexLeaseStore) validateCodexLeaseV2StateForSchema(envelope codexLeaseJournalEnvelopeV2, protocolSchema int) error {
	lanes := make(map[string]CodexJournalLane, len(envelope.Lanes))
	for _, lane := range envelope.Lanes {
		if lane.SessionHash == "" || lane.ThreadHash == "" || lane.NamespaceHash == "" || lane.Generation == 0 || lane.LastObservedAt.IsZero() {
			return fmt.Errorf("%w: invalid lane", ErrCodexLeaseInvalidMutation)
		}
		digest := codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
		if _, duplicate := lanes[digest]; duplicate {
			return fmt.Errorf("%w: duplicate lane", ErrCodexLeaseInvalidMutation)
		}
		lanes[digest] = lane
	}
	records := make(map[CodexJournalRecordIdentity]CodexJournalRecordV2, len(envelope.Records))
	for _, record := range envelope.Records {
		identity := record.Identity()
		if identity.IsZero() || record.SessionHash == "" || record.ThreadHash == "" || record.NamespaceHash == "" || record.TurnHash == "" || record.ModeEpoch == 0 || record.RecordGeneration == 0 || record.LaneGeneration == 0 || record.LeaseGeneration == 0 || record.CreatedAt.IsZero() || record.LastObservedAt.Before(record.CreatedAt) {
			return fmt.Errorf("%w: invalid record", ErrCodexLeaseInvalidMutation)
		}
		if _, duplicate := records[identity]; duplicate {
			return fmt.Errorf("%w: duplicate record", ErrCodexLeaseInvalidMutation)
		}
		if _, ok := lanes[identity.LaneDigest]; !ok {
			return fmt.Errorf("%w: record has no lane", ErrCodexLeaseInvalidMutation)
		}
		if (record.Generation != 0 && !validCodexLeaseRequestKind(record.RequestKind, record.CompactionPhase)) || record.ProtocolSchema != protocolSchema {
			return fmt.Errorf("%w: unsupported request kind/phase", ErrCodexLeaseInvalidMutation)
		}
		if !canonicalCodexCapacityBuckets(record.RequiredBuckets) {
			return fmt.Errorf("%w: non-canonical capacity buckets", ErrCodexLeaseInvalidMutation)
		}
		if err := store.validateCodexAttemptEnvelope(record); err != nil {
			return err
		}
		records[identity] = record
	}
	for digest, lane := range lanes {
		current := codexLaneTupleIdentity(lane, true)
		last := codexLaneTupleIdentity(lane, false)
		if !current.IsZero() {
			if current.LaneDigest != digest {
				return fmt.Errorf("%w: current lane digest mismatch", ErrCodexLeaseInvalidMutation)
			}
			if _, ok := records[current]; !ok {
				return fmt.Errorf("%w: current tuple has no record", ErrCodexLeaseInvalidMutation)
			}
			if current != last {
				return fmt.Errorf("%w: current and last disagree", ErrCodexLeaseInvalidMutation)
			}
		}
		if !last.IsZero() {
			if last.LaneDigest != digest {
				return fmt.Errorf("%w: last lane digest mismatch", ErrCodexLeaseInvalidMutation)
			}
			if _, ok := records[last]; !ok {
				return fmt.Errorf("%w: last tuple has no record", ErrCodexLeaseInvalidMutation)
			}
		}
	}
	for identity, record := range records {
		if record.PredecessorTurnHash == "" {
			if record.PredecessorModeEpoch != 0 || record.PredecessorAuthoritative || record.PredecessorGeneration != 0 {
				return fmt.Errorf("%w: partial predecessor tuple", ErrCodexLeaseInvalidMutation)
			}
			continue
		}
		predecessor := CodexJournalRecordIdentity{LaneDigest: identity.LaneDigest, TurnDigest: record.PredecessorTurnHash, ModeEpoch: record.PredecessorModeEpoch, Authoritative: record.PredecessorAuthoritative}
		stored, ok := records[predecessor]
		if ok && stored.RecordGeneration != record.PredecessorGeneration {
			return fmt.Errorf("%w: invalid predecessor generation", ErrCodexLeaseInvalidMutation)
		}
	}
	if err := store.validateCodexLeaseAdmissionEvidence(envelope); err != nil {
		return fmt.Errorf("%w: %v", ErrCodexLeaseInvalidMutation, err)
	}
	return nil
}

func (store *CodexLeaseStore) validateCodexLeaseAdmissionEvidence(envelope codexLeaseJournalEnvelopeV2) error {
	records := make(map[CodexJournalRecordIdentity]CodexJournalRecordV2, len(envelope.Records))
	eligibleByLane := make(map[string][]CodexJournalRecordV2)
	for _, record := range envelope.Records {
		identity := record.Identity()
		records[identity] = record
		if !record.EverAdmitted {
			if record.AdmissionJournalGeneration != 0 || record.AdmissionRequestGeneration != 0 || record.AdmissionRequestKind != "" || record.AdmissionCompactionPhase != "" || !record.AdmittedAt.IsZero() {
				return errors.New("partial record admission evidence")
			}
			if record.Authoritative && (record.State == LeaseBoundActive || record.State == LeaseContinuationPending || record.State == LeaseBoundQuiescent) {
				return errors.New("admitted lifecycle state lacks admission evidence")
			}
			continue
		}
		if record.AdmissionRequestKind == CodexRequestPrewarm || !validCodexLeaseRequestKind(record.AdmissionRequestKind, record.AdmissionCompactionPhase) || !validCodexLeaseDigest(record.AccountHash) {
			return errors.New("ineligible record has admission evidence")
		}
		if record.AdmissionJournalGeneration <= envelope.Cutover.CompletionGeneration || record.AdmissionJournalGeneration > envelope.Generation || record.AdmissionRequestGeneration == 0 || record.AdmissionRequestGeneration > record.Generation {
			return errors.New("invalid record admission generation")
		}
		if record.AdmissionRequestGeneration == record.Generation && (record.AdmissionRequestKind != record.RequestKind || record.AdmissionCompactionPhase != record.CompactionPhase) {
			return errors.New("first admission evidence does not match its request")
		}
		if !codexLeaseUTCTime(record.AdmittedAt) || record.AdmittedAt.Before(record.CreatedAt) || record.AdmittedAt.After(record.LastObservedAt) {
			return errors.New("invalid record admission timestamp")
		}
		switch record.State {
		case LeaseReserving, LeaseProvisional, LeaseFailedUnadmitted:
			return errors.New("unadmitted lifecycle state has admission evidence")
		}
		if codexLeaseAffinityEligible(record) {
			eligibleByLane[identity.LaneDigest] = append(eligibleByLane[identity.LaneDigest], record)
		}
	}

	for _, lane := range envelope.Lanes {
		laneDigest := codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
		present := lane.LastAdmittedAccountHash != "" || lane.LastAdmittedTurnHash != "" || lane.LastAdmittedModeEpoch != 0 || lane.LastAdmittedAuthoritative || lane.LastAdmissionJournalGeneration != 0 || lane.AffinityRefreshJournalGeneration != 0 || !lane.LastAdmittedAt.IsZero() || !lane.LastCacheAdmittedAt.IsZero() || lane.LastCacheEffectiveModel != ""
		if !present {
			if len(eligibleByLane[laneDigest]) != 0 {
				return errors.New("eligible admitted record has no lane affinity")
			}
			continue
		}
		if !validCodexLeaseDigest(lane.LastAdmittedAccountHash) || !validCodexLeaseDigest(lane.LastAdmittedTurnHash) || lane.LastAdmittedModeEpoch == 0 || !lane.LastAdmittedAuthoritative || lane.LastAdmissionJournalGeneration <= envelope.Cutover.CompletionGeneration || lane.LastAdmissionJournalGeneration > envelope.Generation || !codexLeaseUTCTime(lane.LastAdmittedAt) || lane.LastAdmittedAt.Before(envelope.Cutover.CompletedAt) || lane.LastAdmittedAt.After(lane.LastObservedAt) {
			return errors.New("invalid lane admission affinity")
		}
		if lane.AffinityRefreshJournalGeneration != 0 && (lane.AffinityRefreshJournalGeneration <= lane.LastAdmissionJournalGeneration || lane.AffinityRefreshJournalGeneration > envelope.Generation) {
			return errors.New("invalid lane affinity refresh")
		}
		if lane.LastCacheAdmittedAt.IsZero() {
			if lane.LastCacheEffectiveModel != "" {
				return errors.New("invalid lane cache affinity")
			}
		} else if lane.LastCacheEffectiveModel == "" || !codexLeaseUTCTime(lane.LastCacheAdmittedAt) || lane.LastCacheAdmittedAt.Before(lane.LastAdmittedAt) || lane.LastCacheAdmittedAt.After(lane.LastObservedAt) || strings.TrimSpace(lane.LastCacheEffectiveModel) != lane.LastCacheEffectiveModel {
			return errors.New("invalid lane cache affinity")
		}
		source := CodexJournalRecordIdentity{
			LaneDigest:    laneDigest,
			TurnDigest:    lane.LastAdmittedTurnHash,
			ModeEpoch:     lane.LastAdmittedModeEpoch,
			Authoritative: true,
		}
		if record, retained := records[source]; retained {
			if !codexLeaseAffinityEligible(record) || !constantTimeCodexLeaseDigestEqual(record.AccountHash, lane.LastAdmittedAccountHash) || record.AdmissionJournalGeneration != lane.LastAdmissionJournalGeneration || record.AdmittedAt != lane.LastAdmittedAt {
				return errors.New("lane affinity source mismatch")
			}
		}
		for _, record := range eligibleByLane[laneDigest] {
			if record.AdmissionJournalGeneration > lane.LastAdmissionJournalGeneration || (record.AdmissionJournalGeneration == lane.LastAdmissionJournalGeneration && record.Identity() != source) {
				return errors.New("lane affinity is not the latest admitted record")
			}
		}
	}
	return nil
}

func (store *CodexLeaseStore) validateCodexAttemptEnvelope(record CodexJournalRecordV2) error {
	if record.Generation == 0 {
		if record.AccountHash != "" || !codexCurrentRequestIsZero(record.CodexCurrentRequest) || (record.State != LeaseReserving && record.State != LeaseFailedUnadmitted) {
			return fmt.Errorf("%w: zero request generation carries request authority", ErrCodexLeaseInvalidMutation)
		}
		return nil
	}
	empty := codexAttemptEnvelopeIsZero(record.AttemptEnvelope)
	if empty {
		return fmt.Errorf("%w: nonzero request generation lacks attempt envelope", ErrCodexLeaseInvalidMutation)
	}
	envelope := record.AttemptEnvelope
	if envelope.PolicyVersion != CodexLeaseAttemptPolicyVersion || envelope.AttemptLimit == 0 || int(envelope.AttemptLimit) != len(envelope.Slots) || envelope.PlanDigest == "" || !hmac.Equal([]byte(envelope.PlanDigest), []byte(codexLeaseAttemptPlanDigest(store.key, envelope.Slots))) {
		return fmt.Errorf("%w: invalid attempt envelope", ErrCodexLeaseInvalidMutation)
	}
	for index, slot := range envelope.Slots {
		if slot.Index != uint32(index+1) || slot.AccountHash == "" || slot.CandidateHash == "" || (slot.Kind != CodexAttemptSlotDirect && slot.Kind != CodexAttemptSlotEligibleManagedRefresh) {
			return fmt.Errorf("%w: invalid attempt slot", ErrCodexLeaseInvalidMutation)
		}
	}
	if record.AccountHash == "" || record.RequestedModelHash == "" || record.EffectiveModel == "" || len(record.RequiredBuckets) == 0 {
		return fmt.Errorf("%w: incomplete route choice", ErrCodexLeaseInvalidMutation)
	}
	if len(record.Attempts) > int(envelope.AttemptLimit) {
		return ErrCodexLeaseAttemptLimit
	}
	seenGeneration := make(map[uint64]struct{}, len(record.Attempts))
	consumedSlot := make(map[uint32]struct{})
	currentFound := record.CurrentAttemptGeneration == 0
	var previousGeneration uint64
	for _, attempt := range record.Attempts {
		if attempt.Generation == 0 || attempt.Revision == 0 || attempt.Slot == 0 || attempt.Slot > envelope.AttemptLimit || attempt.CreatedAt.Before(record.CreatedAt) || attempt.LastObservedAt.Before(attempt.CreatedAt) || attempt.LastObservedAt.After(record.LastObservedAt) {
			return fmt.Errorf("%w: invalid attempt row", ErrCodexLeaseInvalidMutation)
		}
		if attempt.Generation <= previousGeneration {
			return fmt.Errorf("%w: non-monotonic attempt generation", ErrCodexLeaseInvalidMutation)
		}
		previousGeneration = attempt.Generation
		if _, duplicate := seenGeneration[attempt.Generation]; duplicate {
			return fmt.Errorf("%w: duplicate attempt generation", ErrCodexLeaseInvalidMutation)
		}
		seenGeneration[attempt.Generation] = struct{}{}
		if attempt.Generation == record.CurrentAttemptGeneration {
			currentFound = true
		}
		if attempt.State != CodexAttemptPrepared {
			if _, duplicate := consumedSlot[attempt.Slot]; duplicate {
				return fmt.Errorf("%w: attempt slot consumed twice", ErrCodexLeaseInvalidMutation)
			}
			consumedSlot[attempt.Slot] = struct{}{}
		}
	}
	if !currentFound {
		return fmt.Errorf("%w: missing current attempt", ErrCodexLeaseInvalidMutation)
	}
	return nil
}

func buildCodexLeasePostFence(envelope codexLeaseJournalEnvelopeV2, laneDigest string, previous []CodexLeaseRecordFence, appended map[CodexJournalRecordIdentity]uint64) (CodexLeaseGenerationFence, error) {
	var lane CodexJournalLane
	foundLane := false
	for _, candidate := range envelope.Lanes {
		if codexJournalLaneDigest(candidate.SessionHash, candidate.ThreadHash, candidate.NamespaceHash) == laneDigest {
			lane = candidate
			foundLane = true
			break
		}
	}
	if !foundLane {
		return CodexLeaseGenerationFence{}, fmt.Errorf("%w: committed lane missing", ErrCodexLeaseTrustLost)
	}
	records := make(map[CodexJournalRecordIdentity]CodexJournalRecordV2, len(envelope.Records))
	for _, record := range envelope.Records {
		records[record.Identity()] = record
	}
	post := CodexLeaseGenerationFence{
		Journal: envelope.Generation,
		Lane:    lane.Generation,
		Current: codexLaneTupleIdentity(lane, true),
		Last:    codexLaneTupleIdentity(lane, false),
	}
	for _, oldFence := range previous {
		record, exists := records[oldFence.Record]
		if !exists {
			post.TouchedRecords = append(post.TouchedRecords, CodexLeaseRecordFence{Record: oldFence.Record})
			continue
		}
		updated := CodexLeaseRecordFence{Record: oldFence.Record, Revision: record.RecordGeneration, Lease: record.LeaseGeneration, RequestGeneration: record.Generation, CurrentAttempt: record.CurrentAttemptGeneration}
		attempts := make(map[uint64]CodexJournalAttempt, len(record.Attempts))
		for _, attempt := range record.Attempts {
			attempts[attempt.Generation] = attempt
		}
		for _, oldAttempt := range oldFence.TouchedAttempts {
			generation := oldAttempt.Generation
			if generation == 0 {
				generation = appended[oldFence.Record]
			}
			if attempt, ok := attempts[generation]; ok {
				updated.TouchedAttempts = append(updated.TouchedAttempts, CodexAttemptFence{RequestGeneration: record.Generation, Generation: generation, Revision: attempt.Revision})
			} else {
				updated.TouchedAttempts = append(updated.TouchedAttempts, CodexAttemptFence{RequestGeneration: record.Generation, Generation: generation})
			}
		}
		post.TouchedRecords = append(post.TouchedRecords, updated)
	}
	sort.Slice(post.TouchedRecords, func(i, j int) bool {
		return codexRecordIdentityLess(post.TouchedRecords[i].Record, post.TouchedRecords[j].Record)
	})
	return post, nil
}

func codexLaneCurrentIdentity(lane CodexJournalLane, records map[CodexJournalRecordIdentity]int, source []CodexJournalRecordV2) (CodexJournalRecordIdentity, error) {
	identity := codexLaneTupleIdentity(lane, true)
	if identity.IsZero() {
		return identity, nil
	}
	if _, ok := records[identity]; !ok {
		return CodexJournalRecordIdentity{}, fmt.Errorf("%w: lane current record missing", ErrCodexLeaseTrustLost)
	}
	return identity, nil
}

func codexLaneLastIdentity(lane CodexJournalLane, records map[CodexJournalRecordIdentity]int, source []CodexJournalRecordV2) (CodexJournalRecordIdentity, error) {
	identity := codexLaneTupleIdentity(lane, false)
	if identity.IsZero() {
		return identity, nil
	}
	if _, ok := records[identity]; !ok {
		return CodexJournalRecordIdentity{}, fmt.Errorf("%w: lane last record missing", ErrCodexLeaseTrustLost)
	}
	return identity, nil
}

func codexLaneTupleIdentity(lane CodexJournalLane, current bool) CodexJournalRecordIdentity {
	turn := lane.LastTurnHash
	epoch := lane.LastModeEpoch
	authoritative := lane.LastAuthoritative
	if current {
		turn = lane.CurrentTurnHash
		epoch = lane.CurrentModeEpoch
		authoritative = lane.CurrentAuthoritative
	}
	if turn == "" {
		return CodexJournalRecordIdentity{}
	}
	return CodexJournalRecordIdentity{LaneDigest: codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash), TurnDigest: turn, ModeEpoch: epoch, Authoritative: authoritative}
}

func sameCodexLanePointers(left, right CodexJournalLane) bool {
	return left.SessionHash == right.SessionHash && left.ThreadHash == right.ThreadHash && left.NamespaceHash == right.NamespaceHash && left.CurrentTurnHash == right.CurrentTurnHash && left.CurrentModeEpoch == right.CurrentModeEpoch && left.CurrentAuthoritative == right.CurrentAuthoritative && left.LastTurnHash == right.LastTurnHash && left.LastModeEpoch == right.LastModeEpoch && left.LastAuthoritative == right.LastAuthoritative && slices.Equal(left.RequestUnavailableAccountHashes, right.RequestUnavailableAccountHashes) && slices.Equal(left.QuotaExhaustedAccountHashes, right.QuotaExhaustedAccountHashes)
}

func codexLeaseCurrentAttemptState(record CodexJournalRecordV2) CodexAttemptState {
	for _, attempt := range record.Attempts {
		if attempt.Generation == record.CurrentAttemptGeneration {
			return attempt.State
		}
	}
	return 0
}

func codexLaneAffinityIsZero(lane CodexJournalLane) bool {
	return lane.LastAdmittedAccountHash == "" && lane.LastAdmittedTurnHash == "" && lane.LastAdmittedModeEpoch == 0 && !lane.LastAdmittedAuthoritative && lane.LastAdmissionJournalGeneration == 0 && lane.AffinityRefreshJournalGeneration == 0 && lane.LastAdmittedAt.IsZero() && lane.LastCacheAdmittedAt.IsZero() && lane.LastCacheEffectiveModel == ""
}

func codexLaneAffinityJournalGeneration(lane CodexJournalLane) uint64 {
	return max(lane.LastAdmissionJournalGeneration, lane.AffinityRefreshJournalGeneration)
}

func copyCodexLaneAffinity(destination *CodexJournalLane, source CodexJournalLane) {
	destination.LastAdmittedAccountHash = source.LastAdmittedAccountHash
	destination.LastAdmittedTurnHash = source.LastAdmittedTurnHash
	destination.LastAdmittedModeEpoch = source.LastAdmittedModeEpoch
	destination.LastAdmittedAuthoritative = source.LastAdmittedAuthoritative
	destination.LastAdmissionJournalGeneration = source.LastAdmissionJournalGeneration
	destination.AffinityRefreshJournalGeneration = source.AffinityRefreshJournalGeneration
	destination.LastAdmittedAt = source.LastAdmittedAt
	destination.LastCacheAdmittedAt = source.LastCacheAdmittedAt
	destination.LastCacheEffectiveModel = source.LastCacheEffectiveModel
}

func codexLeaseAffinityEligible(record CodexJournalRecordV2) bool {
	if !record.Authoritative || !record.EverAdmitted {
		return false
	}
	switch record.AdmissionRequestKind {
	case CodexRequestTurn:
		return record.AdmissionCompactionPhase == ""
	case CodexRequestCompaction:
		return record.AdmissionCompactionPhase == CodexCompactionStandalone || record.AdmissionCompactionPhase == CodexCompactionPreTurn
	default:
		return false
	}
}

func codexLeaseCurrentRequestCacheEligible(record CodexJournalRecordV2) bool {
	switch record.RequestKind {
	case CodexRequestTurn:
		return record.CompactionPhase == ""
	case CodexRequestCompaction:
		return record.CompactionPhase == CodexCompactionStandalone || record.CompactionPhase == CodexCompactionPreTurn
	default:
		return false
	}
}

func sameCodexLeaseSemantics(left, right CodexJournalRecordV2) bool {
	return left.AccountHash == right.AccountHash &&
		left.CorrelationHash == right.CorrelationHash &&
		left.TurnStateHash == right.TurnStateHash &&
		left.State == right.State &&
		left.RequestKind == right.RequestKind &&
		left.CompactionPhase == right.CompactionPhase &&
		left.ProtocolSchema == right.ProtocolSchema &&
		left.Authoritative == right.Authoritative &&
		left.DownstreamSocketGeneration == right.DownstreamSocketGeneration &&
		left.UpstreamSocketGeneration == right.UpstreamSocketGeneration &&
		left.SocketLineageExtinct == right.SocketLineageExtinct &&
		left.Generation == right.Generation &&
		left.RoutingRefs == right.RoutingRefs &&
		left.AttemptRefs == right.AttemptRefs &&
		left.ResponseObserverRefs == right.ResponseObserverRefs &&
		left.RequestedModelHash == right.RequestedModelHash &&
		left.DispatchPermitDigest == right.DispatchPermitDigest &&
		left.QuotaExhaustionProbe == right.QuotaExhaustionProbe &&
		left.EffectiveModel == right.EffectiveModel &&
		slices.Equal(left.RequiredBuckets, right.RequiredBuckets) &&
		left.HasEncryptedState == right.HasEncryptedState &&
		left.HasResponseAnchor == right.HasResponseAnchor &&
		left.HasTurnState == right.HasTurnState &&
		left.TurnStateLatchCurrent == right.TurnStateLatchCurrent &&
		left.NonMigratable == right.NonMigratable &&
		left.AdoptedPrewarm == right.AdoptedPrewarm &&
		left.PrewarmAdoptionJournalGeneration == right.PrewarmAdoptionJournalGeneration &&
		sameCodexAttemptEnvelope(left.AttemptEnvelope, right.AttemptEnvelope)
}

func codexAttemptEnvelopeIsZero(envelope CodexAttemptEnvelope) bool {
	return envelope.PolicyVersion == 0 && envelope.PlanDigest == "" && envelope.AttemptLimit == 0 && len(envelope.Slots) == 0
}

func validCodexLeaseRequestKind(kind CodexRequestKind, phase CodexCompactionPhase) bool {
	switch kind {
	case CodexRequestTurn, CodexRequestPrewarm, CodexRequestMemory:
		return phase == ""
	case CodexRequestCompaction:
		return phase == CodexCompactionStandalone || phase == CodexCompactionPreTurn || phase == CodexCompactionMidTurn
	default:
		return false
	}
}

func canonicalCodexCapacityBuckets(buckets []CapacityBucket) bool {
	for index, bucket := range buckets {
		value := string(bucket)
		if value == "" || (bucket != CapacityBucketBase && (!strings.HasPrefix(value, "model:") || len(value) == len("model:") || value != strings.ToLower(value))) {
			return false
		}
		if index != 0 && buckets[index-1] >= bucket {
			return false
		}
	}
	return true
}

func cloneCodexJournalRecordV2(record CodexJournalRecordV2) CodexJournalRecordV2 {
	clone := record
	clone.CodexCurrentRequest = cloneCodexCurrentRequest(record.CodexCurrentRequest)
	return clone
}

func cloneCodexCurrentRequest(request CodexCurrentRequest) CodexCurrentRequest {
	clone := request
	clone.AttemptEnvelope.Slots = cloneCodexLeaseSlice(request.AttemptEnvelope.Slots)
	clone.RequiredBuckets = cloneCodexLeaseSlice(request.RequiredBuckets)
	clone.Attempts = cloneCodexLeaseSlice(request.Attempts)
	return clone
}

func codexCurrentRequestIsZero(request CodexCurrentRequest) bool {
	return request.Generation == 0 && request.RequestKind == "" && request.CompactionPhase == "" && request.RequestedModelHash == "" && request.DispatchPermitDigest == "" && !request.QuotaExhaustionProbe && request.EffectiveModel == "" && len(request.RequiredBuckets) == 0 && codexAttemptEnvelopeIsZero(request.AttemptEnvelope) && request.CurrentAttemptGeneration == 0 && request.RoutingRefs == 0 && request.AttemptRefs == 0 && request.ResponseObserverRefs == 0 && len(request.Attempts) == 0
}

func sameCodexCurrentRequestPlan(left, right CodexCurrentRequest) bool {
	return left.Generation == right.Generation && left.RequestKind == right.RequestKind && left.CompactionPhase == right.CompactionPhase && sameCodexLeaseOptionalDigest(left.RequestedModelHash, right.RequestedModelHash) && sameCodexLeaseOptionalDigest(left.DispatchPermitDigest, right.DispatchPermitDigest) && left.QuotaExhaustionProbe == right.QuotaExhaustionProbe && left.EffectiveModel == right.EffectiveModel && slices.Equal(left.RequiredBuckets, right.RequiredBuckets) && sameCodexAttemptEnvelope(left.AttemptEnvelope, right.AttemptEnvelope)
}

func sameCodexAttemptEnvelope(left, right CodexAttemptEnvelope) bool {
	return left.PolicyVersion == right.PolicyVersion && left.PlanDigest == right.PlanDigest && left.AttemptLimit == right.AttemptLimit && slices.Equal(left.Slots, right.Slots)
}

func sameCodexLeaseOptionalDigest(left, right string) bool {
	if left == "" || right == "" {
		return left == right
	}
	return constantTimeCodexLeaseDigestEqual(left, right)
}

func cloneCodexLeaseRecordFence(fence CodexLeaseRecordFence) CodexLeaseRecordFence {
	clone := fence
	clone.TouchedAttempts = append([]CodexAttemptFence(nil), fence.TouchedAttempts...)
	return clone
}

func cloneCodexLeaseGenerationFence(fence CodexLeaseGenerationFence) CodexLeaseGenerationFence {
	clone := fence
	clone.TouchedRecords = make([]CodexLeaseRecordFence, len(fence.TouchedRecords))
	for index, record := range fence.TouchedRecords {
		clone.TouchedRecords[index] = cloneCodexLeaseRecordFence(record)
	}
	return clone
}

func staleCodexLeaseMutation(component, detail string) error {
	return &CodexLeaseStaleMutationError{Component: component, Detail: detail}
}

func codexRecordIdentityLess(left, right CodexJournalRecordIdentity) bool {
	if left.LaneDigest != right.LaneDigest {
		return left.LaneDigest < right.LaneDigest
	}
	if left.TurnDigest != right.TurnDigest {
		return left.TurnDigest < right.TurnDigest
	}
	if left.ModeEpoch != right.ModeEpoch {
		return left.ModeEpoch < right.ModeEpoch
	}
	return !left.Authoritative && right.Authoritative
}
