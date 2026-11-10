package proxy

import "slices"

// Delta snapshots have already passed canonical validation, including UTC
// timestamps without monotonic clock readings. Keep slice nilness significant,
// matching reflect.DeepEqual rather than treating nil and empty as equivalent.
func equalCodexDeltaLane(a, b *CodexJournalLane) bool {
	return a.RedistributionRequestedGeneration == b.RedistributionRequestedGeneration &&
		a.RedistributionCompletedGeneration == b.RedistributionCompletedGeneration &&
		a.SessionHash == b.SessionHash && a.ThreadHash == b.ThreadHash && a.NamespaceHash == b.NamespaceHash &&
		a.Generation == b.Generation && a.CurrentTurnHash == b.CurrentTurnHash &&
		a.CurrentModeEpoch == b.CurrentModeEpoch && a.CurrentAuthoritative == b.CurrentAuthoritative &&
		a.LastTurnHash == b.LastTurnHash && a.LastModeEpoch == b.LastModeEpoch && a.LastAuthoritative == b.LastAuthoritative &&
		a.LastAdmittedAccountHash == b.LastAdmittedAccountHash && a.LastAdmittedTurnHash == b.LastAdmittedTurnHash &&
		a.LastAdmittedModeEpoch == b.LastAdmittedModeEpoch && a.LastAdmittedAuthoritative == b.LastAdmittedAuthoritative &&
		a.LastAdmissionJournalGeneration == b.LastAdmissionJournalGeneration &&
		a.AffinityRefreshJournalGeneration == b.AffinityRefreshJournalGeneration &&
		a.LastAdmittedAt == b.LastAdmittedAt && a.LastCacheAdmittedAt == b.LastCacheAdmittedAt &&
		a.LastCacheEffectiveModel == b.LastCacheEffectiveModel && a.LastObservedAt == b.LastObservedAt &&
		(a.RequestUnavailableAccountHashes == nil) == (b.RequestUnavailableAccountHashes == nil) &&
		slices.Equal(a.RequestUnavailableAccountHashes, b.RequestUnavailableAccountHashes) &&
		(a.QuotaExhaustedAccountHashes == nil) == (b.QuotaExhaustedAccountHashes == nil) &&
		slices.Equal(a.QuotaExhaustedAccountHashes, b.QuotaExhaustedAccountHashes)
}

func equalCodexDeltaRecord(a, b *CodexJournalRecordV2) bool {
	return a.SessionHash == b.SessionHash && a.ThreadHash == b.ThreadHash && a.NamespaceHash == b.NamespaceHash &&
		a.TurnHash == b.TurnHash && a.AccountHash == b.AccountHash && a.PredecessorTurnHash == b.PredecessorTurnHash &&
		a.PredecessorModeEpoch == b.PredecessorModeEpoch && a.PredecessorAuthoritative == b.PredecessorAuthoritative &&
		a.CorrelationHash == b.CorrelationHash && a.TurnStateHash == b.TurnStateHash && a.PreviousTurnStateHash == b.PreviousTurnStateHash &&
		a.RecordGeneration == b.RecordGeneration && a.LaneGeneration == b.LaneGeneration &&
		a.PredecessorGeneration == b.PredecessorGeneration && a.LeaseGeneration == b.LeaseGeneration && a.ModeEpoch == b.ModeEpoch &&
		a.DownstreamSocketGeneration == b.DownstreamSocketGeneration && a.UpstreamSocketGeneration == b.UpstreamSocketGeneration &&
		a.State == b.State && a.ProtocolSchema == b.ProtocolSchema && a.Authoritative == b.Authoritative &&
		a.SocketLineageExtinct == b.SocketLineageExtinct && equalCodexDeltaRequest(&a.CodexCurrentRequest, &b.CodexCurrentRequest) &&
		a.HasEncryptedState == b.HasEncryptedState && a.HasResponseAnchor == b.HasResponseAnchor &&
		a.HasTurnState == b.HasTurnState && a.TurnStateLatchCurrent == b.TurnStateLatchCurrent &&
		a.NonMigratable == b.NonMigratable && a.AdoptedPrewarm == b.AdoptedPrewarm &&
		a.PrewarmAdoptionJournalGeneration == b.PrewarmAdoptionJournalGeneration && a.EverAdmitted == b.EverAdmitted &&
		a.AdmissionJournalGeneration == b.AdmissionJournalGeneration && a.AdmissionRequestGeneration == b.AdmissionRequestGeneration &&
		a.AdmissionRequestKind == b.AdmissionRequestKind && a.AdmissionCompactionPhase == b.AdmissionCompactionPhase &&
		a.AdmittedAt == b.AdmittedAt && a.CreatedAt == b.CreatedAt && a.LastObservedAt == b.LastObservedAt
}

func equalCodexDeltaRequest(a, b *CodexCurrentRequest) bool {
	return a.RedistributionGeneration == b.RedistributionGeneration && a.Generation == b.Generation &&
		a.RequestKind == b.RequestKind && a.CompactionPhase == b.CompactionPhase &&
		a.RequestedModelHash == b.RequestedModelHash && a.DispatchPermitDigest == b.DispatchPermitDigest &&
		a.QuotaExhaustionProbe == b.QuotaExhaustionProbe && a.EffectiveModel == b.EffectiveModel &&
		a.CurrentAttemptGeneration == b.CurrentAttemptGeneration && a.RoutingRefs == b.RoutingRefs &&
		a.AttemptRefs == b.AttemptRefs && a.ResponseObserverRefs == b.ResponseObserverRefs &&
		(a.RequiredBuckets == nil) == (b.RequiredBuckets == nil) && slices.Equal(a.RequiredBuckets, b.RequiredBuckets) &&
		a.AttemptEnvelope.PolicyVersion == b.AttemptEnvelope.PolicyVersion &&
		a.AttemptEnvelope.PlanDigest == b.AttemptEnvelope.PlanDigest &&
		a.AttemptEnvelope.AttemptLimit == b.AttemptEnvelope.AttemptLimit &&
		(a.AttemptEnvelope.Slots == nil) == (b.AttemptEnvelope.Slots == nil) &&
		slices.Equal(a.AttemptEnvelope.Slots, b.AttemptEnvelope.Slots) &&
		(a.Attempts == nil) == (b.Attempts == nil) && slices.Equal(a.Attempts, b.Attempts)
}
