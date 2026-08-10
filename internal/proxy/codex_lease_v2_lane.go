package proxy

import (
	"fmt"
	"sort"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// CodexRestoredRecord pairs authenticated journal state with the transient
// account key that is currently resolvable for it. Record and Choice are
// detached copies; RequestedModel remains empty until the caller proves the
// request-supplied model against Record.RequestedModelHash.
type CodexRestoredRecord struct {
	Identity   CodexJournalRecordIdentity
	Record     CodexJournalRecordV2
	AccountKey codex.AccountKey
	Choice     RouteChoice
	Fence      CodexLeaseRecordFence
}

func (store *CodexLeaseStore) LoadLane(key LeaseKey, accounts []codex.AccountKey, policy CodexLeaseAuthorityPolicy) (CodexRestoredLane, error) {
	blocked := CodexRestoredLane{Classification: CodexRestoredLaneRecoveryBlocked}
	if store == nil {
		return blocked, ErrCodexLeaseWriterUnavailable
	}
	if err := key.validate(); err != nil {
		return blocked, fmt.Errorf("%w: %v", ErrCodexContinuity, err)
	}
	policy = cloneCodexLeaseAuthorityPolicy(policy)
	if err := validateCodexLeaseAuthorityPolicy(policy); err != nil {
		return blocked, err
	}
	operation, err := store.beginOperation()
	if err != nil {
		return blocked, err
	}
	defer operation.Release()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return blocked, ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return blocked, fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if err := store.revalidateV2InstalledLocked(); err != nil {
		store.poisoned = err
		return blocked, err
	}
	if store.v2 == nil {
		return blocked, fmt.Errorf("%w: schema-v2 journal unavailable", ErrCodexLeaseTrustLost)
	}
	if store.v2.Cutover.State == CodexLeaseCutoverLegacyQuarantine || !store.v2.Cutover.NoLegacyAuthority {
		return blocked, ErrCodexLegacyQuarantine
	}
	if err := validateCodexLeaseRepresentedModes(representedCodexLeaseAuthoritativeEpochs(store.v2.Records), store.modes); err != nil {
		return blocked, err
	}
	if policy.Authoritative && !containsCodexLeaseEpoch(store.modes.RecognisedAuthoritativeEpochs, policy.ModeEpoch) {
		return blocked, fmt.Errorf("%w: mode epoch %d is not retained", ErrCodexLeaseAuthorityMismatch, policy.ModeEpoch)
	}
	for _, epoch := range policy.RetainedAuthoritativeEpochs {
		if !containsCodexLeaseEpoch(store.modes.RecognisedAuthoritativeEpochs, epoch) {
			return blocked, fmt.Errorf("%w: mode epoch %d is not retained", ErrCodexLeaseAuthorityMismatch, epoch)
		}
	}

	sessionHash := store.hash("session", key.Lane.Session)
	threadHash := store.hash("thread", key.Lane.Thread)
	namespaceHash := store.hash("namespace", key.Lane.Namespace)
	turnHash := store.hash("turn", key.Turn)
	requestedIdentity := CodexJournalRecordIdentity{
		LaneDigest:    codexJournalLaneDigest(sessionHash, threadHash, namespaceHash),
		TurnDigest:    turnHash,
		ModeEpoch:     policy.ModeEpoch,
		Authoritative: policy.Authoritative,
	}
	restored := CodexRestoredLane{
		Classification:    CodexRestoredLaneUnseen,
		RequestedIdentity: requestedIdentity,
		RequestedRecord: CodexJournalRecordV2{
			SessionHash:    sessionHash,
			ThreadHash:     threadHash,
			NamespaceHash:  namespaceHash,
			TurnHash:       turnHash,
			ModeEpoch:      policy.ModeEpoch,
			Authoritative:  policy.Authoritative,
			ProtocolSchema: CurrentCodexLeaseSchema,
		},
		Fence: CodexLeaseGenerationFence{Journal: store.v2.Generation},
	}

	laneFound := false
	for _, lane := range store.v2.Lanes {
		if !constantTimeCodexLeaseDigestEqual(lane.SessionHash, sessionHash) || !constantTimeCodexLeaseDigestEqual(lane.ThreadHash, threadHash) || !constantTimeCodexLeaseDigestEqual(lane.NamespaceHash, namespaceHash) {
			continue
		}
		if laneFound {
			return blocked, fmt.Errorf("%w: duplicate matching lane", ErrCodexLeaseTrustLost)
		}
		laneFound = true
		restored.Lane = lane
		restored.Fence.Lane = lane.Generation
		restored.Fence.Current = codexLaneTupleIdentity(lane, true)
		restored.Fence.Last = codexLaneTupleIdentity(lane, false)
	}
	if !laneFound {
		restored.Fence.TouchedRecords = []CodexLeaseRecordFence{{Record: requestedIdentity}}
		return cloneCodexRestoredLane(restored), nil
	}
	if !codexLaneAffinityIsZero(restored.Lane) {
		resolvedAccount, resolved := store.resolveCodexLeaseAccount(restored.Lane.LastAdmittedAccountHash, accounts)
		restored.Affinity = &CodexLeaseAffinityHint{
			Resolved: resolved,
			Source: CodexJournalRecordIdentity{
				LaneDigest:    requestedIdentity.LaneDigest,
				TurnDigest:    restored.Lane.LastAdmittedTurnHash,
				ModeEpoch:     restored.Lane.LastAdmittedModeEpoch,
				Authoritative: restored.Lane.LastAdmittedAuthoritative,
			},
			AdmissionJournalGeneration: restored.Lane.LastAdmissionJournalGeneration,
			AdmittedAt:                 restored.Lane.LastAdmittedAt,
		}
		if resolved {
			restored.Affinity.AccountKey = resolvedAccount
		}
	}

	exactHistorical := false
	shadowOnly := false
	unrecognisedAuthority := false
	for _, record := range store.v2.Records {
		if !constantTimeCodexLeaseDigestEqual(record.SessionHash, sessionHash) || !constantTimeCodexLeaseDigestEqual(record.ThreadHash, threadHash) || !constantTimeCodexLeaseDigestEqual(record.NamespaceHash, namespaceHash) {
			continue
		}
		clone := cloneCodexJournalRecordV2(record)
		identity := clone.Identity()
		fence := codexLeaseFenceForRestoredRecord(clone)
		resolvedAccount, resolved := store.resolveCodexLeaseAccount(clone.AccountHash, accounts)
		exactRequestedTurn := constantTimeCodexLeaseDigestEqual(clone.TurnHash, turnHash) && codexLeaseRecordAllowedByPolicy(clone, policy)
		if clone.AccountHash != "" && !resolved && exactRequestedTurn && codexLeaseRecordRequiresResolvedAccount(clone) {
			return blocked, fmt.Errorf("%w: persisted route account is unavailable", ErrCodexLeaseAuthorityMismatch)
		}
		choice := RouteChoice{}
		if resolved {
			choice = RouteChoice{
				AccountKey:      resolvedAccount,
				EffectiveModel:  clone.EffectiveModel,
				RequiredBuckets: append([]CapacityBucket(nil), clone.RequiredBuckets...),
			}
		}
		restored.Records = append(restored.Records, cloneCodexJournalRecordV2(clone))
		restored.ResolvedRecords = append(restored.ResolvedRecords, CodexRestoredRecord{
			Identity:   identity,
			Record:     clone,
			AccountKey: resolvedAccount,
			Choice:     choice,
			Fence:      cloneCodexLeaseRecordFence(fence),
		})
		restored.Fence.TouchedRecords = append(restored.Fence.TouchedRecords, cloneCodexLeaseRecordFence(fence))

		if !constantTimeCodexLeaseDigestEqual(clone.TurnHash, turnHash) {
			continue
		}
		if codexLeaseRecordAllowedByPolicy(clone, policy) {
			if identity == restored.Fence.Current {
				restored.Classification = CodexRestoredLaneCurrent
			} else {
				exactHistorical = true
			}
			continue
		}
		if policy.Authoritative && !clone.Authoritative && clone.ModeEpoch == policy.ModeEpoch {
			shadowOnly = true
		} else if clone.Authoritative {
			unrecognisedAuthority = true
		}
	}
	for _, record := range restored.Records {
		if record.PredecessorTurnHash == "" {
			continue
		}
		predecessor := CodexJournalRecordIdentity{
			LaneDigest:    record.Identity().LaneDigest,
			TurnDigest:    record.PredecessorTurnHash,
			ModeEpoch:     record.PredecessorModeEpoch,
			Authoritative: record.PredecessorAuthoritative,
		}
		if !codexLeaseFenceContainsRecord(restored.Fence.TouchedRecords, predecessor) {
			restored.Fence.TouchedRecords = append(restored.Fence.TouchedRecords, CodexLeaseRecordFence{Record: predecessor})
		}
	}
	if restored.Classification != CodexRestoredLaneCurrent {
		switch {
		case exactHistorical:
			restored.Classification = CodexRestoredLaneHistorical
		case unrecognisedAuthority:
			return blocked, fmt.Errorf("%w: turn exists only at an unrecognised authoritative epoch", ErrCodexLeaseAuthorityMismatch)
		case shadowOnly:
			restored.Classification = CodexRestoredLaneShadowOnly
		}
	}
	if !codexLeaseFenceContainsRecord(restored.Fence.TouchedRecords, requestedIdentity) {
		restored.Fence.TouchedRecords = append(restored.Fence.TouchedRecords, CodexLeaseRecordFence{Record: requestedIdentity})
	}
	sort.Slice(restored.Fence.TouchedRecords, func(i, j int) bool {
		return codexRecordIdentityLess(restored.Fence.TouchedRecords[i].Record, restored.Fence.TouchedRecords[j].Record)
	})
	return cloneCodexRestoredLane(restored), nil
}

func validateCodexLeaseAuthorityPolicy(policy CodexLeaseAuthorityPolicy) error {
	if policy.ModeEpoch == 0 {
		return fmt.Errorf("%w: invalid lease authority policy", ErrCodexLeaseAuthorityMismatch)
	}
	previous := uint64(0)
	for _, epoch := range policy.RetainedAuthoritativeEpochs {
		if epoch == 0 || epoch >= policy.ModeEpoch || epoch <= previous {
			return fmt.Errorf("%w: non-canonical retained authority epochs", ErrCodexLeaseAuthorityMismatch)
		}
		previous = epoch
	}
	return nil
}

func codexLeaseRecordAllowedByPolicy(record CodexJournalRecordV2, policy CodexLeaseAuthorityPolicy) bool {
	if record.Authoritative {
		return (policy.Authoritative && record.ModeEpoch == policy.ModeEpoch) || containsCodexLeaseEpoch(policy.RetainedAuthoritativeEpochs, record.ModeEpoch)
	}
	return !policy.Authoritative && record.ModeEpoch == policy.ModeEpoch
}

// MutationFence narrows the hydrated snapshot to exactly the records a
// mutation depends on. CommitLane intentionally rejects unrelated fences.
func (restored CodexRestoredLane) MutationFence(identities ...CodexJournalRecordIdentity) (CodexLeaseGenerationFence, error) {
	result := CodexLeaseGenerationFence{
		Journal: restored.Fence.Journal,
		Lane:    restored.Fence.Lane,
		Current: restored.Fence.Current,
		Last:    restored.Fence.Last,
	}
	seen := make(map[CodexJournalRecordIdentity]struct{}, len(identities))
	for _, identity := range identities {
		if identity.IsZero() {
			return CodexLeaseGenerationFence{}, fmt.Errorf("%w: zero requested mutation fence", ErrCodexLeaseInvalidMutation)
		}
		if _, duplicate := seen[identity]; duplicate {
			return CodexLeaseGenerationFence{}, fmt.Errorf("%w: duplicate requested mutation fence", ErrCodexLeaseInvalidMutation)
		}
		seen[identity] = struct{}{}
		found := false
		for _, fence := range restored.Fence.TouchedRecords {
			if fence.Record != identity {
				continue
			}
			result.TouchedRecords = append(result.TouchedRecords, cloneCodexLeaseRecordFence(fence))
			found = true
			break
		}
		if !found {
			return CodexLeaseGenerationFence{}, fmt.Errorf("%w: requested record is outside hydrated lane", ErrCodexLeaseInvalidMutation)
		}
	}
	sort.Slice(result.TouchedRecords, func(i, j int) bool {
		return codexRecordIdentityLess(result.TouchedRecords[i].Record, result.TouchedRecords[j].Record)
	})
	return result, nil
}

func (store *CodexLeaseStore) resolveCodexLeaseAccount(accountHash string, accounts []codex.AccountKey) (codex.AccountKey, bool) {
	if accountHash == "" {
		return "", false
	}
	var resolved codex.AccountKey
	for _, account := range accounts {
		if account == "" || !constantTimeCodexLeaseDigestEqual(accountHash, store.hash("account", string(account))) {
			continue
		}
		if resolved != "" && resolved != account {
			return "", false
		}
		resolved = account
	}
	return resolved, resolved != ""
}

func codexLeaseRecordRequiresResolvedAccount(record CodexJournalRecordV2) bool {
	switch record.State {
	case LeaseProvisional:
		return record.EverAdmitted || record.NonMigratable || codexLeaseCurrentAttemptState(record) != CodexAttemptAbandonedBeforeDispatch
	case LeaseBoundActive, LeaseContinuationPending, LeaseBoundQuiescent, LeaseOrphaned:
		return true
	default:
		return false
	}
}

func codexLeaseFenceForRestoredRecord(record CodexJournalRecordV2) CodexLeaseRecordFence {
	requestGeneration := record.CodexCurrentRequest.Generation
	fence := CodexLeaseRecordFence{
		Record:            record.Identity(),
		Revision:          record.RecordGeneration,
		Lease:             record.LeaseGeneration,
		RequestGeneration: requestGeneration,
		CurrentAttempt:    record.CurrentAttemptGeneration,
	}
	for _, attempt := range record.Attempts {
		fence.TouchedAttempts = append(fence.TouchedAttempts, CodexAttemptFence{RequestGeneration: requestGeneration, Generation: attempt.Generation, Revision: attempt.Revision})
	}
	return fence
}

func codexLeaseFenceContainsRecord(fences []CodexLeaseRecordFence, identity CodexJournalRecordIdentity) bool {
	for _, fence := range fences {
		if fence.Record == identity {
			return true
		}
	}
	return false
}

func cloneCodexRestoredLane(restored CodexRestoredLane) CodexRestoredLane {
	clone := restored
	clone.RequestedRecord = cloneCodexJournalRecordV2(restored.RequestedRecord)
	if restored.Affinity != nil {
		affinity := *restored.Affinity
		clone.Affinity = &affinity
	}
	clone.Records = make([]CodexJournalRecordV2, len(restored.Records))
	for index, record := range restored.Records {
		clone.Records[index] = cloneCodexJournalRecordV2(record)
	}
	clone.ResolvedRecords = make([]CodexRestoredRecord, len(restored.ResolvedRecords))
	for index, record := range restored.ResolvedRecords {
		clone.ResolvedRecords[index] = record
		clone.ResolvedRecords[index].Record = cloneCodexJournalRecordV2(record.Record)
		clone.ResolvedRecords[index].Choice.RequiredBuckets = append([]CapacityBucket(nil), record.Choice.RequiredBuckets...)
		clone.ResolvedRecords[index].Fence = cloneCodexLeaseRecordFence(record.Fence)
	}
	clone.Fence = cloneCodexLeaseGenerationFence(restored.Fence)
	return clone
}
