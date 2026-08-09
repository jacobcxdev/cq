package proxy

import (
	"fmt"
	"math"
	"time"
)

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

	next, changed, err := store.compactCodexLeaseV2Locked(store.policy.Now().UTC(), store.policy.Retention)
	if err != nil || !changed {
		return err
	}
	if err := store.validateCodexLeaseV2CandidateLocked(next); err != nil {
		return err
	}
	return store.commitV2Locked(store.v2.Generation, next)
}

func (store *CodexLeaseStore) compactCodexLeaseV2Locked(now time.Time, retention time.Duration) (codexLeaseJournalEnvelopeV2, bool, error) {
	if store.v2.Generation == math.MaxUint64 {
		return codexLeaseJournalEnvelopeV2{}, false, fmt.Errorf("%w: journal generation overflow", ErrCodexLeaseInvalidMutation)
	}
	next := cloneCodexLeaseV2Envelope(*store.v2)
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
	for _, lane := range next.Lanes {
		laneDigest := codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
		survivors := 0
		for identity, action := range actions {
			if identity.LaneDigest == laneDigest && action != codexLeaseRetentionDelete {
				survivors++
			}
		}
		if survivors == 0 {
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

	keptLanes := make([]CodexJournalLane, 0, len(next.Lanes))
	for _, lane := range next.Lanes {
		laneDigest := codexJournalLaneDigest(lane.SessionHash, lane.ThreadHash, lane.NamespaceHash)
		hasRecord := false
		laneTransitioned := false
		for _, record := range next.Records {
			if record.Identity().LaneDigest == laneDigest {
				hasRecord = true
				if _, expired := transitioned[record.Identity()]; expired {
					laneTransitioned = true
				}
			}
		}
		if !hasRecord {
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
			changed = true
		} else if laneTransitioned {
			lane.LastObservedAt = monotonicCodexLeaseTime(now, lane.LastObservedAt)
		}
		for index := range next.Records {
			if next.Records[index].Identity().LaneDigest == laneDigest {
				if _, expired := transitioned[next.Records[index].Identity()]; expired {
					next.Records[index].LaneGeneration = lane.Generation
				}
			}
		}
		keptLanes = append(keptLanes, lane)
	}
	next.Lanes = keptLanes
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
	data, err := store.marshalV2Envelope(candidate)
	if err != nil {
		return err
	}
	var signed codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseV2StrictJSON(data, &signed); err != nil {
		return err
	}
	if err := store.validateV2Envelope(signed); err != nil {
		return err
	}
	if err := store.validateCodexLeaseV2State(signed); err != nil {
		return err
	}
	return nil
}
