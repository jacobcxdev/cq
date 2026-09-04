package proxy

import (
	"fmt"
	"math"
	"time"
)

const codexLeaseRetentionSweepInterval = time.Hour

type codexLeaseRetentionAction uint8

const (
	codexLeaseRetentionKeep codexLeaseRetentionAction = iota
	codexLeaseRetentionDelete
	codexLeaseRetentionExpire
)

func (store *CodexLeaseStore) compactV2() error {
	operation, err := store.beginOperation()
	if err != nil {
		return err
	}
	defer operation.Release()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.v2 == nil {
		return ErrCodexLeaseWriterUnavailable
	}
	if store.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrCodexLeaseStorePoisoned, store.poisoned)
	}
	if store.policy.Retention <= 0 || store.policy.Now == nil {
		return fmt.Errorf("%w: invalid retention policy", ErrCodexLeaseWriterUnavailable)
	}
	if err := store.revalidateV2InstalledLocked(); err != nil {
		store.poisoned = err
		return err
	}
	if store.v2.Cutover.State != CodexLeaseCutoverComplete || !store.v2.Cutover.NoLegacyAuthority {
		return ErrCodexLegacyQuarantine
	}

	now := store.policy.Now().UTC()
	next, changed, err := store.compactCodexLeaseV2Locked(now, store.policy.Retention)
	if err != nil || !changed {
		if err == nil {
			store.lastRetentionSweep = now
		}
		return err
	}
	if err := store.validateCodexLeaseV2CandidateLocked(next); err != nil {
		return err
	}
	if err := store.commitV2Locked(store.v2.Generation, next); err != nil {
		return err
	}
	store.lastRetentionSweep = now
	return nil
}

func (store *CodexLeaseStore) compactCodexLeaseV2Locked(now time.Time, retention time.Duration) (codexLeaseJournalEnvelopeV2, bool, error) {
	if store.v2.Generation == math.MaxUint64 {
		return codexLeaseJournalEnvelopeV2{}, false, fmt.Errorf("%w: journal generation overflow", ErrCodexLeaseInvalidMutation)
	}
	next := cloneCodexLeaseV2Envelope(*store.v2)
	return compactCodexLeaseV2Envelope(next, now, retention)
}

func compactCodexLeaseV2Envelope(next codexLeaseJournalEnvelopeV2, now time.Time, retention time.Duration) (codexLeaseJournalEnvelopeV2, bool, error) {
	actions := make(map[CodexJournalRecordIdentity]codexLeaseRetentionAction, len(next.Records))
	for _, record := range next.Records {
		identity := record.Identity()
		actions[identity] = codexLeaseRetentionActionFor(record, now, retention)
	}

	// A retained successor carries an exact predecessor generation. If the
	// predecessor still exists and needs a lifecycle transition, retain it
	// unchanged until the successor is itself collectible. A deleted terminal
	// predecessor may be represented by the successor's signed generation fence.
	for _, record := range next.Records {
		if actions[record.Identity()] == codexLeaseRetentionDelete || record.PredecessorTurnHash == "" {
			continue
		}
		predecessor := CodexJournalRecordIdentity{
			LaneDigest:    record.Identity().LaneDigest,
			TurnDigest:    record.PredecessorTurnHash,
			ModeEpoch:     record.PredecessorModeEpoch,
			Authoritative: record.PredecessorAuthoritative,
		}
		if actions[predecessor] == codexLeaseRetentionExpire {
			actions[predecessor] = codexLeaseRetentionKeep
		}
	}

	// Every retained lane keeps its signed last-turn anchor. If all records in
	// the lane are collectible, the lane and its anchor are removed together.
	survivorsByLane := make(map[string]int, len(next.Lanes))
	for identity, action := range actions {
		if action != codexLeaseRetentionDelete {
			survivorsByLane[identity.LaneDigest]++
		}
	}
	for _, lane := range next.Lanes {
		laneDigest := codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
		if survivorsByLane[laneDigest] == 0 {
			continue
		}
		last := codexLaneTupleIdentity(lane, false)
		if actions[last] == codexLeaseRetentionDelete {
			actions[last] = codexLeaseRetentionKeep
		}
	}

	changed := false
	transitioned := make(map[CodexJournalRecordIdentity]struct{})
	keptRecords := make([]CodexJournalRecordV2, 0, len(next.Records))
	for _, record := range next.Records {
		identity := record.Identity()
		switch actions[identity] {
		case codexLeaseRetentionDelete:
			changed = true
			continue
		case codexLeaseRetentionExpire:
			if err := expireCodexLeaseRecord(&record, now); err != nil {
				return codexLeaseJournalEnvelopeV2{}, false, err
			}
			transitioned[identity] = struct{}{}
			changed = true
		}
		keptRecords = append(keptRecords, record)
	}
	next.Records = keptRecords
	hasRecordByLane := make(map[string]bool, len(next.Lanes))
	transitionedByLane := make(map[string]bool, len(next.Lanes))
	for _, record := range next.Records {
		identity := record.Identity()
		hasRecordByLane[identity.LaneDigest] = true
		if _, expired := transitioned[identity]; expired {
			transitionedByLane[identity.LaneDigest] = true
		}
	}

	keptLanes := make([]CodexJournalLane, 0, len(next.Lanes))
	updatedLaneGenerations := make(map[string]uint64)
	for _, lane := range next.Lanes {
		laneDigest := codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
		if !hasRecordByLane[laneDigest] {
			changed = true
			continue
		}
		current := codexLaneTupleIdentity(lane, true)
		if _, expired := transitioned[current]; expired {
			if lane.Generation == math.MaxUint64 {
				return codexLeaseJournalEnvelopeV2{}, false, fmt.Errorf("%w: lane generation overflow", ErrCodexLeaseInvalidMutation)
			}
			lane.Generation++
			lane.CurrentTurnHash = ""
			lane.CurrentModeEpoch = 0
			lane.CurrentAuthoritative = false
			lane.LastObservedAt = monotonicCodexLeaseTime(now, lane.LastObservedAt)
			updatedLaneGenerations[laneDigest] = lane.Generation
			changed = true
		} else if transitionedByLane[laneDigest] {
			lane.LastObservedAt = monotonicCodexLeaseTime(now, lane.LastObservedAt)
		}
		keptLanes = append(keptLanes, lane)
	}
	next.Lanes = keptLanes
	for index := range next.Records {
		identity := next.Records[index].Identity()
		if generation, updated := updatedLaneGenerations[identity.LaneDigest]; updated {
			if _, expired := transitioned[identity]; expired {
				next.Records[index].LaneGeneration = generation
			}
		}
	}
	return next, changed, nil
}

func codexLeaseRetentionActionFor(record CodexJournalRecordV2, now time.Time, retention time.Duration) codexLeaseRetentionAction {
	if record.RoutingRefs > 0 || record.AttemptRefs > 0 || record.ResponseObserverRefs > 0 || !record.SocketLineageExtinct || record.LastObservedAt.After(now) || now.Sub(record.LastObservedAt) <= retention {
		return codexLeaseRetentionKeep
	}
	switch record.State {
	case LeaseBoundQuiescent, LeaseOrphaned, LeaseSuperseded, LeaseExpired, LeaseFailedUnadmitted:
		return codexLeaseRetentionDelete
	default:
		return codexLeaseRetentionExpire
	}
}

func expireCodexLeaseRecord(record *CodexJournalRecordV2, now time.Time) error {
	if record.RecordGeneration == math.MaxUint64 || record.LeaseGeneration == math.MaxUint64 {
		return fmt.Errorf("%w: record generation overflow", ErrCodexLeaseInvalidMutation)
	}
	if record.State == LeaseReserving {
		record.State = LeaseFailedUnadmitted
	} else {
		record.State = LeaseExpired
	}
	becameIndeterminate := false
	for index := range record.Attempts {
		attempt := &record.Attempts[index]
		switch attempt.State {
		case CodexAttemptPrepared, CodexAttemptDispatched, CodexAttemptStreaming:
			if attempt.Revision == math.MaxUint64 {
				return fmt.Errorf("%w: attempt revision overflow", ErrCodexLeaseInvalidMutation)
			}
			attempt.State = CodexAttemptIndeterminate
			becameIndeterminate = true
			attempt.Revision++
			attempt.LastObservedAt = monotonicCodexLeaseTime(now, attempt.LastObservedAt)
		}
	}
	if becameIndeterminate {
		record.NonMigratable = true
	}
	record.RecordGeneration++
	record.LeaseGeneration++
	record.SocketLineageExtinct = true
	record.RoutingRefs = 0
	record.AttemptRefs = 0
	record.ResponseObserverRefs = 0
	record.LastObservedAt = monotonicCodexLeaseTime(now, record.LastObservedAt)
	return nil
}

func (store *CodexLeaseStore) validateCodexLeaseV2CandidateLocked(candidate codexLeaseJournalEnvelopeV2) error {
	candidate.Generation = store.v2.Generation + 1
	if err := validateCodexLeaseRepresentedModes(representedCodexLeaseAuthoritativeEpochs(candidate.Records), store.modes); err != nil {
		return err
	}
	_, _, err := store.prepareV2Envelope(candidate)
	return err
}
