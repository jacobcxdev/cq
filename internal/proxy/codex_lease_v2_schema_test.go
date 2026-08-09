package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCodexLeaseV2SchemaAcceptsCanonicalSignedEnvelope(t *testing.T) {
	store, envelope := codexLeaseV2SchemaFixture(t)
	codexLeaseV2SignSchemaFixture(t, store, &envelope)

	if err := store.validateV2Envelope(envelope); err != nil {
		t.Fatalf("validate canonical signed envelope: %v", err)
	}
	if err := (&CodexLeaseStore{key: append([]byte(nil), store.key...)}).loadV2Locked(codexLeaseV2SchemaJSON(t, envelope)); err != nil {
		t.Fatalf("decode canonical signed envelope: %v", err)
	}
}

func TestCodexLeaseV2SchemaAcceptsCrashRecoverableStructuralStates(t *testing.T) {
	store, base := codexLeaseV2SchemaFixture(t)
	tests := []struct {
		name   string
		mutate func(*codexLeaseJournalEnvelopeV2)
	}{
		{name: "reserving without choice", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			record := &value.Records[0]
			value.Generation = 2
			record.State = LeaseReserving
			record.RecordGeneration = 1
			record.LeaseGeneration = 1
			record.AccountHash = ""
			record.RequestedModelHash = ""
			record.EffectiveModel = ""
			record.RequiredBuckets = nil
			record.CurrentAttemptGeneration = 0
			record.AttemptEnvelope = CodexAttemptEnvelope{}
			record.Attempts = nil
			record.AttemptRefs = 0
		}},
		{name: "provisional prepared", mutate: func(*codexLeaseJournalEnvelopeV2) {}},
		{name: "provisional dispatched before restart normalisation", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Generation = 4
			value.Records[0].RecordGeneration = 3
			value.Records[0].Attempts[0].State = CodexAttemptDispatched
			value.Records[0].Attempts[0].Revision++
		}},
		{name: "bound active streaming", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Generation = 5
			value.Records[0].State = LeaseBoundActive
			value.Records[0].RecordGeneration = 4
			value.Records[0].LeaseGeneration = 3
			value.Records[0].Attempts[0].State = CodexAttemptStreaming
			value.Records[0].Attempts[0].Revision += 2
			codexLeaseV2SetSchemaAdmissionEvidence(value, 4)
		}},
		{name: "continuation pending completed", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Generation = 6
			value.Records[0].State = LeaseContinuationPending
			value.Records[0].RecordGeneration = 5
			value.Records[0].LeaseGeneration = 4
			value.Records[0].Attempts[0].State = CodexAttemptProviderCompleted
			value.Records[0].Attempts[0].Revision += 3
			value.Records[0].RoutingRefs = 0
			value.Records[0].AttemptRefs = 0
			codexLeaseV2SetSchemaAdmissionEvidence(value, 4)
		}},
		{name: "bound quiescent completed", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Generation = 6
			value.Records[0].State = LeaseBoundQuiescent
			value.Records[0].RecordGeneration = 5
			value.Records[0].LeaseGeneration = 4
			value.Records[0].Attempts[0].State = CodexAttemptProviderCompleted
			value.Records[0].Attempts[0].Revision += 3
			value.Records[0].RoutingRefs = 0
			value.Records[0].AttemptRefs = 0
			codexLeaseV2SetSchemaAdmissionEvidence(value, 4)
		}},
		{name: "orphaned indeterminate with extinct lineage", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Generation = 5
			value.Records[0].State = LeaseOrphaned
			value.Records[0].RecordGeneration = 4
			value.Records[0].LeaseGeneration = 3
			value.Records[0].Attempts[0].State = CodexAttemptIndeterminate
			value.Records[0].Attempts[0].Revision += 2
			value.Records[0].RoutingRefs = 0
			value.Records[0].AttemptRefs = 0
			value.Records[0].SocketLineageExtinct = true
			value.Records[0].NonMigratable = true
		}},
		{name: "shadow terminal orphan without admission authority", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			record := &value.Records[0]
			record.Authoritative = false
			record.State = LeaseOrphaned
			record.Attempts[0].State = CodexAttemptProviderCompleted
			record.RoutingRefs = 0
			record.AttemptRefs = 0
			record.ResponseObserverRefs = 0
			record.SocketLineageExtinct = true
			value.Lanes[0].CurrentAuthoritative = false
			value.Lanes[0].LastAuthoritative = false
		}},
		{name: "failed unadmitted terminal route tombstone", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			record := &value.Records[0]
			value.Generation = 5
			record.State = LeaseFailedUnadmitted
			record.RecordGeneration = 4
			record.LeaseGeneration = 3
			record.Attempts[0].State = CodexAttemptProviderFailed
			record.Attempts[0].Revision += 2
			record.RoutingRefs = 0
			record.AttemptRefs = 0
			value.Lanes[0].CurrentTurnHash = ""
			value.Lanes[0].CurrentModeEpoch = 0
			value.Lanes[0].CurrentAuthoritative = false
		}},
		{name: "failed unadmitted selector tombstone without choice", mutate: func(value *codexLeaseJournalEnvelopeV2) {
			record := &value.Records[0]
			record.State = LeaseFailedUnadmitted
			record.AccountHash = ""
			record.RequestedModelHash = ""
			record.EffectiveModel = ""
			record.RequiredBuckets = nil
			record.CurrentAttemptGeneration = 0
			record.AttemptEnvelope = CodexAttemptEnvelope{}
			record.Attempts = nil
			record.RoutingRefs = 0
			record.AttemptRefs = 0
			value.Lanes[0].CurrentTurnHash = ""
			value.Lanes[0].CurrentModeEpoch = 0
			value.Lanes[0].CurrentAuthoritative = false
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := codexLeaseV2CloneSchemaFixture(t, base)
			test.mutate(&value)
			codexLeaseV2SignSchemaFixture(t, store, &value)
			if err := store.validateV2Envelope(value); err != nil {
				t.Fatalf("validate structurally recoverable state: %v", err)
			}
		})
	}
}

func codexLeaseV2SetSchemaAdmissionEvidence(value *codexLeaseJournalEnvelopeV2, generation uint64) {
	record := &value.Records[0]
	record.EverAdmitted = true
	record.AdmissionJournalGeneration = generation
	record.AdmissionRequestGeneration = record.Generation
	record.AdmissionRequestKind = record.RequestKind
	record.AdmissionCompactionPhase = record.CompactionPhase
	record.AdmittedAt = record.Attempts[0].LastObservedAt
	lane := &value.Lanes[0]
	lane.LastAdmittedAccountHash = record.AccountHash
	lane.LastAdmittedTurnHash = record.TurnHash
	lane.LastAdmittedModeEpoch = record.ModeEpoch
	lane.LastAdmittedAuthoritative = true
	lane.LastAdmissionJournalGeneration = generation
	lane.LastAdmittedAt = record.AdmittedAt
}

func TestCodexLeaseV2SchemaRejectsUnknownJSONKeys(t *testing.T) {
	store, envelope := codexLeaseV2SchemaFixture(t)
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	data := string(codexLeaseV2SchemaJSON(t, envelope))

	tests := []struct {
		name string
		old  string
		new  string
	}{
		{name: "envelope", old: `{"version":2`, new: `{"unknown_envelope":true,"version":2`},
		{name: "cutover", old: `"cutover":{"source_version":0`, new: `"cutover":{"unknown_cutover":true,"source_version":0`},
		{name: "lane", old: `"lanes":[{`, new: `"lanes":[{"unknown_lane":true,`},
		{name: "record", old: `"records":[{`, new: `"records":[{"unknown_record":true,`},
		{name: "current request", old: `"current_request":{`, new: `"current_request":{"unknown_current_request":true,`},
		{name: "attempt envelope", old: `"attempt_envelope":{"policy_version":1`, new: `"attempt_envelope":{"unknown_attempt_envelope":true,"policy_version":1`},
		{name: "attempt slot", old: `"slots":[{`, new: `"slots":[{"unknown_slot":true,`},
		{name: "attempt", old: `"attempts":[{`, new: `"attempts":[{"unknown_attempt":true,`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := codexLeaseV2SchemaReplaceOnce(t, data, test.old, test.new)
			codexLeaseV2RequireStrictDecodeFailure(t, store.key, []byte(mutated))
		})
	}
}

func TestCodexLeaseV2SchemaRejectsDuplicateJSONKeys(t *testing.T) {
	store, envelope := codexLeaseV2SchemaFixture(t)
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	data := string(codexLeaseV2SchemaJSON(t, envelope))
	laneSession := envelope.Lanes[0].SessionHash
	recordSession := envelope.Records[0].SessionHash

	tests := []struct {
		name string
		old  string
		new  string
	}{
		{name: "envelope", old: `{"version":2`, new: `{"version":2,"version":2`},
		{name: "cutover", old: `"cutover":{"source_version":0`, new: `"cutover":{"source_version":0,"source_version":0`},
		{
			name: "lane",
			old:  `"lanes":[{"session_hash":"` + laneSession + `"`,
			new:  `"lanes":[{"session_hash":"` + laneSession + `","session_hash":"` + laneSession + `"`,
		},
		{
			name: "record",
			old:  `"records":[{"session_hash":"` + recordSession + `"`,
			new:  `"records":[{"session_hash":"` + recordSession + `","session_hash":"` + recordSession + `"`,
		},
		{name: "attempt envelope", old: `"attempt_envelope":{"policy_version":1`, new: `"attempt_envelope":{"policy_version":1,"policy_version":1`},
		{name: "attempt slot", old: `"slots":[{"index":1`, new: `"slots":[{"index":1,"index":1`},
		{name: "attempt", old: `"attempts":[{"generation":1`, new: `"attempts":[{"generation":1,"generation":1`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := codexLeaseV2SchemaReplaceOnce(t, data, test.old, test.new)
			codexLeaseV2RequireStrictDecodeFailure(t, store.key, []byte(mutated))
		})
	}
}

func TestCodexLeaseV2SchemaRejectsDuplicateBoundedRequestKeys(t *testing.T) {
	store, envelope := codexLeaseV2AdmittedSchemaFixture(t)
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	data := string(codexLeaseV2SchemaJSON(t, envelope))
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{name: "request generation", old: `"current_request":{"generation":1`, new: `"current_request":{"generation":1,"generation":1`},
		{name: "response observer refs", old: `"current_request":{`, new: `"current_request":{"response_observer_refs":0,"response_observer_refs":0,`},
		{name: "admission request generation", old: `"admission_request_generation":1`, new: `"admission_request_generation":1,"admission_request_generation":1`},
		{name: "admission request kind", old: `"admission_request_kind":"turn"`, new: `"admission_request_kind":"turn","admission_request_kind":"turn"`},
		{name: "admission compaction phase", old: `"ever_admitted":true`, new: `"admission_compaction_phase":"","admission_compaction_phase":"","ever_admitted":true`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := codexLeaseV2SchemaReplaceOnce(t, data, test.old, test.new)
			codexLeaseV2RequireStrictDecodeFailure(t, store.key, []byte(mutated))
		})
	}
}

func TestCodexLeaseV2SchemaRejectsReorderedObjectMembersWithoutWriting(t *testing.T) {
	store, envelope := codexLeaseV2SchemaFixture(t)
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	data := string(codexLeaseV2SchemaJSON(t, envelope))
	lane := envelope.Lanes[0]
	record := envelope.Records[0]
	request := record.CodexCurrentRequest
	slot := request.AttemptEnvelope.Slots[0]

	tests := []struct {
		name        string
		old         string
		replacement string
	}{
		{name: "envelope", old: `{"version":2,"hash_version":1`, replacement: `{"hash_version":1,"version":2`},
		{name: "cutover", old: `"cutover":{"source_version":0,"compatibility_epoch":3`, replacement: `"cutover":{"compatibility_epoch":3,"source_version":0`},
		{name: "lane", old: `"lanes":[{"session_hash":"` + lane.SessionHash + `","thread_hash":"` + lane.ThreadHash + `"`, replacement: `"lanes":[{"thread_hash":"` + lane.ThreadHash + `","session_hash":"` + lane.SessionHash + `"`},
		{name: "record", old: `"records":[{"session_hash":"` + record.SessionHash + `","thread_hash":"` + record.ThreadHash + `"`, replacement: `"records":[{"thread_hash":"` + record.ThreadHash + `","session_hash":"` + record.SessionHash + `"`},
		{name: "current request", old: `"current_request":{"generation":1,"request_kind":"turn"`, replacement: `"current_request":{"request_kind":"turn","generation":1`},
		{name: "attempt envelope", old: `"attempt_envelope":{"policy_version":1,"plan_digest":"` + request.AttemptEnvelope.PlanDigest + `"`, replacement: `"attempt_envelope":{"plan_digest":"` + request.AttemptEnvelope.PlanDigest + `","policy_version":1`},
		{name: "slot", old: `"slots":[{"index":1,"account_hash":"` + slot.AccountHash + `"`, replacement: `"slots":[{"account_hash":"` + slot.AccountHash + `","index":1`},
		{name: "attempt", old: `"attempts":[{"generation":1,"revision":1`, replacement: `"attempts":[{"revision":1,"generation":1`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := codexLeaseV2SchemaReplaceOnce(t, data, test.old, test.replacement)
			codexLeaseV2RequireStrictOpenFailureWithoutWrite(t, store.key, []byte(mutated))
		})
	}
}

func TestCodexLeaseV2SchemaRejectsDuplicateAdmissionEvidenceKeys(t *testing.T) {
	store, envelope := codexLeaseV2AdmittedSchemaFixture(t)
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	data := string(codexLeaseV2SchemaJSON(t, envelope))
	record := envelope.Records[0]
	lane := envelope.Lanes[0]

	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "record admitted flag", field: "ever_admitted", value: record.EverAdmitted},
		{name: "record admission generation", field: "admission_journal_generation", value: record.AdmissionJournalGeneration},
		{name: "record admission time", field: "admitted_at", value: record.AdmittedAt},
		{name: "lane admitted account", field: "last_admitted_account_hash", value: lane.LastAdmittedAccountHash},
		{name: "lane admitted turn", field: "last_admitted_turn_hash", value: lane.LastAdmittedTurnHash},
		{name: "lane admitted epoch", field: "last_admitted_mode_epoch", value: lane.LastAdmittedModeEpoch},
		{name: "lane admitted authority", field: "last_admitted_authoritative", value: lane.LastAdmittedAuthoritative},
		{name: "lane admission generation", field: "last_admission_journal_generation", value: lane.LastAdmissionJournalGeneration},
		{name: "lane admission time", field: "last_admitted_at", value: lane.LastAdmittedAt},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			key := `"` + test.field + `":`
			mutated := codexLeaseV2SchemaReplaceOnce(t, data, key, key+string(encoded)+","+key)
			codexLeaseV2RequireStrictDecodeFailure(t, store.key, []byte(mutated))
		})
	}
}

func TestCodexLeaseV2SchemaRejectsTrailingJSONValue(t *testing.T) {
	store, envelope := codexLeaseV2SchemaFixture(t)
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	data := append(codexLeaseV2SchemaJSON(t, envelope), []byte(` {}`)...)
	codexLeaseV2RequireStrictDecodeFailure(t, store.key, data)
}

func TestCodexLeaseV2SchemaRejectsMissingRequiredMembers(t *testing.T) {
	store, envelope := codexLeaseV2SchemaFixture(t)
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "envelope lanes", mutate: func(root map[string]any) { delete(root, "lanes") }},
		{name: "cutover source", mutate: func(root map[string]any) { delete(codexLeaseV2SchemaObject(t, root, "cutover"), "source_version") }},
		{name: "lane generation", mutate: func(root map[string]any) { delete(codexLeaseV2SchemaArrayObject(t, root, "lanes", 0), "generation") }},
		{name: "record protocol", mutate: func(root map[string]any) {
			delete(codexLeaseV2SchemaArrayObject(t, root, "records", 0), "protocol_schema")
		}},
		{name: "current request", mutate: func(root map[string]any) {
			delete(codexLeaseV2SchemaArrayObject(t, root, "records", 0), "current_request")
		}},
		{name: "attempt envelope", mutate: func(root map[string]any) { delete(codexLeaseV2SchemaCurrentRequest(t, root), "attempt_envelope") }},
		{name: "attempt slots", mutate: func(root map[string]any) {
			delete(codexLeaseV2SchemaObject(t, codexLeaseV2SchemaCurrentRequest(t, root), "attempt_envelope"), "slots")
		}},
		{name: "slot kind", mutate: func(root map[string]any) {
			delete(codexLeaseV2SchemaArrayObject(t, codexLeaseV2SchemaObject(t, codexLeaseV2SchemaCurrentRequest(t, root), "attempt_envelope"), "slots", 0), "kind")
		}},
		{name: "attempt revision", mutate: func(root map[string]any) {
			delete(codexLeaseV2SchemaArrayObject(t, codexLeaseV2SchemaCurrentRequest(t, root), "attempts", 0), "revision")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := codexLeaseV2SchemaRawObject(t, codexLeaseV2SchemaJSON(t, envelope))
			test.mutate(root)
			codexLeaseV2RequireStrictDecodeFailure(t, store.key, codexLeaseV2SchemaRawJSON(t, root))
		})
	}
}

func TestCodexLeaseV2SchemaRejectsNullAndExplicitOmittedValues(t *testing.T) {
	store, envelope := codexLeaseV2SchemaFixture(t)
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "null lanes", mutate: func(root map[string]any) { root["lanes"] = nil }},
		{name: "null records", mutate: func(root map[string]any) { root["records"] = nil }},
		{name: "null current request", mutate: func(root map[string]any) {
			codexLeaseV2SchemaArrayObject(t, root, "records", 0)["current_request"] = nil
		}},
		{name: "null attempt envelope", mutate: func(root map[string]any) { codexLeaseV2SchemaCurrentRequest(t, root)["attempt_envelope"] = nil }},
		{name: "null slots", mutate: func(root map[string]any) {
			codexLeaseV2SchemaObject(t, codexLeaseV2SchemaCurrentRequest(t, root), "attempt_envelope")["slots"] = nil
		}},
		{name: "null buckets", mutate: func(root map[string]any) { codexLeaseV2SchemaCurrentRequest(t, root)["required_buckets"] = nil }},
		{name: "null attempts", mutate: func(root map[string]any) { codexLeaseV2SchemaCurrentRequest(t, root)["attempts"] = nil }},
		{name: "explicit empty fresh epochs", mutate: func(root map[string]any) {
			codexLeaseV2SchemaObject(t, root, "cutover")["authoritative_mode_epochs"] = []any{}
		}},
		{name: "explicit zero observer refs", mutate: func(root map[string]any) {
			codexLeaseV2SchemaCurrentRequest(t, root)["response_observer_refs"] = float64(0)
		}},
		{name: "explicit empty predecessor", mutate: func(root map[string]any) {
			codexLeaseV2SchemaArrayObject(t, root, "records", 0)["predecessor_turn_hash"] = ""
		}},
		{name: "explicit zero admission request", mutate: func(root map[string]any) {
			codexLeaseV2SchemaArrayObject(t, root, "records", 0)["admission_request_generation"] = float64(0)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := codexLeaseV2SchemaRawObject(t, codexLeaseV2SchemaJSON(t, envelope))
			test.mutate(root)
			codexLeaseV2RequireStrictDecodeFailure(t, store.key, codexLeaseV2SchemaRawJSON(t, root))
		})
	}
}

func TestCodexLeaseV2SchemaRejectsInvalidCutoverTuples(t *testing.T) {
	store, fresh := codexLeaseV2SchemaFixture(t)
	legacy := codexLeaseV2LegacyQuarantineSchemaFixture(t, store, fresh)
	legacyComplete := codexLeaseV2CloneSchemaFixture(t, legacy)
	legacyComplete.Generation++
	legacyComplete.Cutover.State = CodexLeaseCutoverComplete
	legacyComplete.Cutover.CompletedAt = legacyComplete.Cutover.LegacyQuarantineUntil
	legacyComplete.Cutover.CompletionGeneration = legacyComplete.Generation
	legacyComplete.Cutover.NoLegacyAuthority = true

	tests := []struct {
		name   string
		base   codexLeaseJournalEnvelopeV2
		mutate func(*codexLeaseJournalEnvelopeV2)
	}{
		{name: "zero envelope generation", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Generation = 0 }},
		{name: "unsupported compatibility epoch", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.CompatibilityEpoch = 2 }},
		{name: "zero cutover time", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.At = time.Time{} }},
		{name: "non UTC cutover time", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Cutover.At = value.Cutover.At.In(time.FixedZone("offset", 3600))
			value.Cutover.CompletedAt = value.Cutover.At
		}},
		{name: "zero journal generation", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.JournalGeneration = 0 }},
		{name: "journal generation after envelope", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.JournalGeneration = value.Generation + 1 }},
		{name: "unsupported source", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.SourceVersion = 2 }},
		{name: "fresh state not complete", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.State = CodexLeaseCutoverLegacyQuarantine }},
		{name: "fresh completion time differs", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Cutover.CompletedAt = value.Cutover.At.Add(time.Second)
		}},
		{name: "fresh completion generation differs", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.CompletionGeneration++ }},
		{name: "fresh legacy proof false", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.NoLegacyAuthority = false }},
		{name: "fresh archive digest present", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.LegacyV1SHA256 = strings.Repeat("a", 64) }},
		{name: "fresh horizon present", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Cutover.LegacyQuarantineUntil = value.Cutover.At.Add(time.Hour)
		}},
		{name: "fresh authoritative epochs present", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.AuthoritativeModeEpochs = []uint64{1} }},
		{name: "fresh shadow epochs present", base: fresh, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.ShadowModeEpochs = []uint64{1} }},
		{name: "legacy digest malformed", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.LegacyV1SHA256 = "not-a-sha256" }},
		{name: "legacy horizon equals cutover", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.LegacyQuarantineUntil = value.Cutover.At }},
		{name: "legacy horizon before cutover", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Cutover.LegacyQuarantineUntil = value.Cutover.At.Add(-time.Second)
		}},
		{name: "legacy authoritative epochs unsorted", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.AuthoritativeModeEpochs = []uint64{4, 2} }},
		{name: "legacy authoritative epochs duplicate", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.AuthoritativeModeEpochs = []uint64{2, 2} }},
		{name: "legacy authoritative epoch zero", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.AuthoritativeModeEpochs = []uint64{0, 2} }},
		{name: "legacy shadow epochs unsorted", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.ShadowModeEpochs = []uint64{5, 3} }},
		{name: "legacy shadow epochs duplicate", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.ShadowModeEpochs = []uint64{3, 3} }},
		{name: "legacy quarantine has completion time", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Cutover.CompletedAt = value.Cutover.LegacyQuarantineUntil
		}},
		{name: "legacy quarantine has completion generation", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.CompletionGeneration = value.Generation }},
		{name: "legacy quarantine claims no authority", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.NoLegacyAuthority = true }},
		{name: "legacy quarantine generation advanced", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Generation++ }},
		{name: "legacy quarantine contains lane", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes = append(value.Lanes, fresh.Lanes[0]) }},
		{name: "legacy quarantine contains record", base: legacy, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records = append(value.Records, fresh.Records[0]) }},
		{name: "legacy completion before horizon", base: legacyComplete, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Cutover.CompletedAt = value.Cutover.LegacyQuarantineUntil.Add(-time.Nanosecond)
		}},
		{name: "legacy completion generation differs from commit", base: legacyComplete, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.CompletionGeneration-- }},
		{name: "legacy completion after envelope generation", base: legacyComplete, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Cutover.CompletionGeneration = value.Generation + 1 }},
	}

	codexLeaseV2RunSemanticRejectionTable(t, store, tests)
}

func TestCodexLeaseV2SchemaRejectsInvalidHashesAndDuplicateIdentities(t *testing.T) {
	store, base := codexLeaseV2SchemaFixture(t)
	validOtherTurn := store.hash("turn", "other-turn")
	validOtherSession := store.hash("session", "other-session")

	tests := []struct {
		name   string
		base   codexLeaseJournalEnvelopeV2
		mutate func(*codexLeaseJournalEnvelopeV2)
	}{
		{name: "empty lane session hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].SessionHash = "" }},
		{name: "malformed lane thread hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].ThreadHash = "raw-thread" }},
		{name: "malformed lane namespace hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].NamespaceHash = "!" }},
		{name: "lane namespace is valid HMAC for wrong namespace", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].NamespaceHash = store.hash("namespace", "other")
			value.Records[0].NamespaceHash = value.Lanes[0].NamespaceHash
		}},
		{name: "empty record session hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].SessionHash = "" }},
		{name: "malformed record thread hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].ThreadHash = "raw-thread" }},
		{name: "empty turn hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].TurnHash = "" }},
		{name: "malformed account hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AccountHash = "raw-account" }},
		{name: "empty requested model hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequestedModelHash = "" }},
		{name: "malformed requested model hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequestedModelHash = "gpt-raw" }},
		{name: "response anchor flag without hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].HasResponseAnchor = true }},
		{name: "response anchor hash without flag", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].CorrelationHash = store.hash("correlation", "response")
			value.Records[0].HasResponseAnchor = false
		}},
		{name: "turn state flag without hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].HasTurnState = true }},
		{name: "turn state hash without flag", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].TurnStateHash = store.hash("turn-state", "state")
			value.Records[0].HasTurnState = false
		}},
		{name: "malformed predecessor hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].PredecessorTurnHash = "raw-turn"
			value.Records[0].PredecessorModeEpoch = 1
			value.Records[0].PredecessorAuthoritative = true
			value.Records[0].PredecessorGeneration = 1
		}},
		{name: "slot account differs from route account", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[0].AccountHash = store.hash("account", "other-account")
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "empty slot candidate hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[0].CandidateHash = ""
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "malformed slot candidate hash", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[0].CandidateHash = "candidate-a"
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "duplicate lane identity", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			duplicate := value.Lanes[0]
			duplicate.Generation++
			value.Lanes = append(value.Lanes, duplicate)
		}},
		{name: "duplicate record identity", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			duplicate := value.Records[0]
			duplicate.RecordGeneration++
			value.Records = append(value.Records, duplicate)
		}},
		{name: "record references absent lane", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].SessionHash = validOtherSession }},
		{name: "lane current references absent turn", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].CurrentTurnHash = validOtherTurn
			value.Lanes[0].LastTurnHash = validOtherTurn
		}},
	}

	codexLeaseV2RunSemanticRejectionTable(t, store, tests)
}

func TestCodexLeaseV2SchemaRejectsInvalidLaneHeadsAndGenerations(t *testing.T) {
	store, base := codexLeaseV2SchemaFixture(t)
	otherTurn := store.hash("turn", "other-turn")

	tests := []struct {
		name   string
		base   codexLeaseJournalEnvelopeV2
		mutate func(*codexLeaseJournalEnvelopeV2)
	}{
		{name: "current empty with mode", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].CurrentTurnHash = "" }},
		{name: "current and last turn differ", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].LastTurnHash = otherTurn }},
		{name: "current and last mode differ", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].LastModeEpoch++ }},
		{name: "current and last authority differ", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].LastAuthoritative = false }},
		{name: "last empty with current present", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].LastTurnHash = "" }},
		{name: "record mode does not match head", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].ModeEpoch++ }},
		{name: "record authority does not match head", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].Authoritative = false }},
		{name: "zero lane generation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].Generation = 0 }},
		{name: "lane generation after envelope", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].Generation = value.Generation + 1 }},
		{name: "zero record generation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RecordGeneration = 0 }},
		{name: "record generation after envelope", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RecordGeneration = value.Generation + 1 }},
		{name: "zero record lane generation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].LaneGeneration = 0 }},
		{name: "record lane generation after lane", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].LaneGeneration = value.Lanes[0].Generation + 1
		}},
		{name: "zero lease generation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].LeaseGeneration = 0 }},
		{name: "lease generation after record", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].LeaseGeneration = value.Records[0].RecordGeneration + 1
		}},
		{name: "zero mode epoch", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].ModeEpoch = 0
			value.Lanes[0].CurrentModeEpoch = 0
			value.Lanes[0].LastModeEpoch = 0
		}},
		{name: "predecessor generation without predecessor", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].PredecessorGeneration = 1 }},
		{name: "predecessor mode without predecessor", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].PredecessorModeEpoch = 1 }},
		{name: "predecessor authority without predecessor", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].PredecessorAuthoritative = true }},
		{name: "predecessor hash without generation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].PredecessorTurnHash = otherTurn
			value.Records[0].PredecessorModeEpoch = 1
			value.Records[0].PredecessorAuthoritative = true
		}},
		{name: "zero current attempt with attempts", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].CurrentAttemptGeneration = 0 }},
		{name: "current attempt absent", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].CurrentAttemptGeneration = 2 }},
		{name: "negative routing refs", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RoutingRefs = -1 }},
		{name: "negative attempt refs", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AttemptRefs = -1 }},
		{name: "extinct socket has live refs", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].SocketLineageExtinct = true }},
		{name: "zero protocol schema", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].ProtocolSchema = 0 }},
		{name: "schema one inhibited", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].ProtocolSchema = 1 }},
		{name: "unsupported protocol schema", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].ProtocolSchema = CurrentCodexLeaseSchema + 1
		}},
	}

	codexLeaseV2RunSemanticRejectionTable(t, store, tests)
}

func TestCodexLeaseV2SchemaRejectsInvalidAttemptEnvelopesAndAttempts(t *testing.T) {
	store, base := codexLeaseV2SchemaFixture(t)

	tests := []struct {
		name   string
		base   codexLeaseJournalEnvelopeV2
		mutate func(*codexLeaseJournalEnvelopeV2)
	}{
		{name: "missing policy version", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AttemptEnvelope.PolicyVersion = 0 }},
		{name: "unsupported policy version", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AttemptEnvelope.PolicyVersion = 2 }},
		{name: "empty plan digest", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AttemptEnvelope.PlanDigest = "" }},
		{name: "malformed plan digest", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AttemptEnvelope.PlanDigest = "not-a-digest" }},
		{name: "plan digest mismatch", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[0].CandidateHash = store.hash("candidate", "changed")
		}},
		{name: "zero attempt limit", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AttemptEnvelope.AttemptLimit = 0 }},
		{name: "attempt limit differs from slots", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].AttemptEnvelope.AttemptLimit++ }},
		{name: "missing slots", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots = nil
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "zero slot index", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[0].Index = 0
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "duplicate slot index", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[1].Index = 1
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "non contiguous slot index", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[1].Index = 3
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "slots out of order", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[0], value.Records[0].AttemptEnvelope.Slots[1] = value.Records[0].AttemptEnvelope.Slots[1], value.Records[0].AttemptEnvelope.Slots[0]
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "empty slot account", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[0].AccountHash = ""
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "unsupported slot kind", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].AttemptEnvelope.Slots[0].Kind = "other"
			codexLeaseV2RefreshPlanDigest(t, store, &value.Records[0])
		}},
		{name: "zero attempt generation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].Attempts[0].Generation = 0 }},
		{name: "zero attempt revision", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].Attempts[0].Revision = 0 }},
		{name: "zero attempt slot", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].Attempts[0].Slot = 0 }},
		{name: "attempt slot beyond envelope", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].Attempts[0].Slot = 3 }},
		{name: "unsupported attempt state", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].Attempts[0].State = 99 }},
		{name: "duplicate attempt generation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			second := value.Records[0].Attempts[0]
			second.Slot = 2
			value.Records[0].Attempts = append(value.Records[0].Attempts, second)
		}},
		{name: "attempt generations out of order", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			first := value.Records[0].Attempts[0]
			second := first
			first.Generation = 2
			first.Slot = 2
			second.Generation = 1
			value.Records[0].Attempts = []CodexJournalAttempt{first, second}
			value.Records[0].CurrentAttemptGeneration = 2
		}},
		{name: "current attempt is not latest", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			second := value.Records[0].Attempts[0]
			second.Generation = 2
			second.Slot = 2
			value.Records[0].Attempts = append(value.Records[0].Attempts, second)
		}},
		{name: "two dispatched attempts consume one slot", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].Attempts[0].State = CodexAttemptDispatched
			second := value.Records[0].Attempts[0]
			second.Generation = 2
			value.Records[0].Attempts = append(value.Records[0].Attempts, second)
			value.Records[0].CurrentAttemptGeneration = 2
		}},
		{name: "prepared replacement reuses consumed slot", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].Attempts[0].State = CodexAttemptProviderFailed
			value.Records[0].Attempts[0].Revision++
			second := value.Records[0].Attempts[0]
			second.Generation = 2
			second.Revision = 1
			second.State = CodexAttemptPrepared
			value.Records[0].Attempts = append(value.Records[0].Attempts, second)
			value.Records[0].CurrentAttemptGeneration = 2
		}},
		{name: "historical attempt remains nonterminal", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			second := value.Records[0].Attempts[0]
			second.Generation = 2
			second.Slot = 2
			value.Records[0].Attempts = append(value.Records[0].Attempts, second)
			value.Records[0].CurrentAttemptGeneration = 2
		}},
		{name: "attempts beyond immutable limit", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			second := value.Records[0].Attempts[0]
			second.Generation = 2
			second.Slot = 2
			third := second
			third.Generation = 3
			value.Records[0].Attempts = append(value.Records[0].Attempts, second, third)
			value.Records[0].CurrentAttemptGeneration = 3
		}},
		{name: "reserving lease has attempt envelope", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].State = LeaseReserving }},
		{name: "provisional lease has streaming attempt", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].Attempts[0].State = CodexAttemptStreaming }},
		{name: "bound lease has prepared attempt", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].State = LeaseBoundActive }},
		{name: "failed tombstone retains prepared attempt", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].State = LeaseFailedUnadmitted }},
	}

	codexLeaseV2RunSemanticRejectionTable(t, store, tests)
}

func TestCodexLeaseV2SchemaRejectsImpossibleHistoricalAttemptStatesOnReopen(t *testing.T) {
	store, base := codexLeaseV2SchemaFixture(t)
	tests := []struct {
		name  string
		state CodexAttemptState
	}{
		{name: "provider completed", state: CodexAttemptProviderCompleted},
		{name: "indeterminate", state: CodexAttemptIndeterminate},
		{name: "abandoned before dispatch", state: CodexAttemptAbandonedBeforeDispatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := codexLeaseV2CloneSchemaFixture(t, base)
			value.Records[0].Attempts[0].State = test.state
			if test.state == CodexAttemptIndeterminate {
				value.Records[0].NonMigratable = true
			}
			current := value.Records[0].Attempts[0]
			current.Generation = 2
			current.Slot = 2
			current.State = CodexAttemptPrepared
			value.Records[0].Attempts = append(value.Records[0].Attempts, current)
			value.Records[0].CurrentAttemptGeneration = 2
			codexLeaseV2SignSchemaFixture(t, store, &value)

			reopened := &CodexLeaseStore{key: append([]byte(nil), store.key...)}
			err := reopened.loadV2Locked(codexLeaseV2SchemaJSON(t, value))
			if !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("reopen error = %T %v, want trust lost", err, err)
			}
			if reopened.v2 != nil || len(reopened.journalBytes) != 0 {
				t.Fatal("rejected historical attempt was published in memory")
			}
		})
	}
}

func TestCodexLeaseV2SchemaRejectsUnauthorisedTerminalAuthoritativeOrphanOnReopen(t *testing.T) {
	store, base := codexLeaseV2SchemaFixture(t)
	tests := []struct {
		name  string
		state CodexAttemptState
	}{
		{name: "provider completed", state: CodexAttemptProviderCompleted},
		{name: "provider failed", state: CodexAttemptProviderFailed},
		{name: "abandoned before dispatch", state: CodexAttemptAbandonedBeforeDispatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := codexLeaseV2CloneSchemaFixture(t, base)
			record := &value.Records[0]
			record.State = LeaseOrphaned
			record.Attempts[0].State = test.state
			record.RoutingRefs = 0
			record.AttemptRefs = 0
			record.ResponseObserverRefs = 0
			record.SocketLineageExtinct = true
			codexLeaseV2SignSchemaFixture(t, store, &value)

			reopened := &CodexLeaseStore{key: append([]byte(nil), store.key...)}
			err := reopened.loadV2Locked(codexLeaseV2SchemaJSON(t, value))
			if !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("reopen error = %T %v, want trust lost", err, err)
			}
			if reopened.v2 != nil || len(reopened.journalBytes) != 0 {
				t.Fatal("rejected unauthorised orphan was published in memory")
			}
		})
	}
}

func TestCodexLeaseV2SchemaRejectsInvalidKindsRoutesAndBuckets(t *testing.T) {
	store, base := codexLeaseV2SchemaFixture(t)

	tests := []struct {
		name   string
		base   codexLeaseJournalEnvelopeV2
		mutate func(*codexLeaseJournalEnvelopeV2)
	}{
		{name: "missing request kind", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequestKind = "" }},
		{name: "unsupported request kind", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequestKind = "other" }},
		{name: "memory cannot own lease", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequestKind = CodexRequestMemory }},
		{name: "turn has compaction phase", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].CompactionPhase = CodexCompactionMidTurn }},
		{name: "compaction missing phase", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequestKind = CodexRequestCompaction }},
		{name: "compaction has unsupported phase", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].RequestKind = CodexRequestCompaction
			value.Records[0].CompactionPhase = "other"
		}},
		{name: "missing effective model", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].EffectiveModel = "" }},
		{name: "missing required buckets", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequiredBuckets = nil }},
		{name: "empty bucket", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequiredBuckets = []CapacityBucket{""} }},
		{name: "duplicate buckets", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].RequiredBuckets = []CapacityBucket{CapacityBucketBase, CapacityBucketBase}
		}},
		{name: "unsorted buckets", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].RequestKind = CodexRequestCompaction
			value.Records[0].CompactionPhase = CodexCompactionPreTurn
			value.Records[0].RequiredBuckets = []CapacityBucket{CapacityBucket("model:gpt-5.3-codex-spark"), CapacityBucketBase}
		}},
		{name: "non canonical uppercase bucket", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequiredBuckets = []CapacityBucket{"BASE"} }},
		{name: "invalid model bucket", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].RequiredBuckets = []CapacityBucket{"model:"}
		}},
		{name: "unknown bucket form", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].RequiredBuckets = []CapacityBucket{"spark"} }},
		{name: "effective model bucket absent", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].RequiredBuckets = []CapacityBucket{"model:gpt-5.3-codex-spark"}
		}},
	}

	codexLeaseV2RunSemanticRejectionTable(t, store, tests)
}

func TestCodexLeaseV2SchemaRejectsInvalidLifecycleTimestampsAndOrdering(t *testing.T) {
	store, base := codexLeaseV2SchemaFixture(t)
	cutoverAt := base.Cutover.At

	tests := []struct {
		name   string
		base   codexLeaseJournalEnvelopeV2
		mutate func(*codexLeaseJournalEnvelopeV2)
	}{
		{name: "zero lane observation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Lanes[0].LastObservedAt = time.Time{} }},
		{name: "zero record creation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].CreatedAt = time.Time{} }},
		{name: "zero record observation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].LastObservedAt = time.Time{} }},
		{name: "record predates cutover", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].CreatedAt = cutoverAt.Add(-time.Nanosecond) }},
		{name: "record observation predates creation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].LastObservedAt = value.Records[0].CreatedAt.Add(-time.Nanosecond)
		}},
		{name: "lane observation predates current record", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].LastObservedAt = value.Records[0].LastObservedAt.Add(-time.Nanosecond)
		}},
		{name: "zero attempt creation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].Attempts[0].CreatedAt = time.Time{} }},
		{name: "zero attempt observation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) { value.Records[0].Attempts[0].LastObservedAt = time.Time{} }},
		{name: "attempt predates record", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].Attempts[0].CreatedAt = value.Records[0].CreatedAt.Add(-time.Nanosecond)
		}},
		{name: "attempt observation predates creation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].Attempts[0].LastObservedAt = value.Records[0].Attempts[0].CreatedAt.Add(-time.Nanosecond)
		}},
		{name: "attempt observation follows record", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].Attempts[0].LastObservedAt = value.Records[0].LastObservedAt.Add(time.Nanosecond)
		}},
		{name: "non UTC lane observation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Lanes[0].LastObservedAt = value.Lanes[0].LastObservedAt.In(time.FixedZone("offset", 3600))
		}},
		{name: "non UTC record creation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].CreatedAt = value.Records[0].CreatedAt.In(time.FixedZone("offset", 3600))
		}},
		{name: "non UTC attempt observation", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			value.Records[0].Attempts[0].LastObservedAt = value.Records[0].Attempts[0].LastObservedAt.In(time.FixedZone("offset", 3600))
		}},
		{name: "lanes not canonical", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			codexLeaseV2AppendSecondSchemaLane(t, store, value)
			sort.Slice(value.Lanes, func(i, j int) bool { return codexLeaseV2SchemaLaneLess(value.Lanes[j], value.Lanes[i]) })
		}},
		{name: "records not canonical", base: base, mutate: func(value *codexLeaseJournalEnvelopeV2) {
			codexLeaseV2AppendSecondSchemaLane(t, store, value)
			sort.Slice(value.Records, func(i, j int) bool { return codexLeaseV2SchemaRecordLess(value.Records[j], value.Records[i]) })
		}},
	}

	codexLeaseV2RunSemanticRejectionTable(t, store, tests)
}

func codexLeaseV2SchemaFixture(t *testing.T) (*CodexLeaseStore, codexLeaseJournalEnvelopeV2) {
	t.Helper()
	store := &CodexLeaseStore{key: bytes.Repeat([]byte{0x5a}, codexLeaseHMACKeyBytes)}
	cutoverAt := time.Date(2026, time.August, 9, 1, 0, 0, 0, time.UTC)
	createdAt := cutoverAt.Add(time.Minute)
	attemptAt := createdAt.Add(time.Minute)
	observedAt := attemptAt.Add(time.Minute)

	sessionHash := store.hash("session", "session-a")
	threadHash := store.hash("thread", "thread-a")
	namespaceHash := store.hash("namespace", CodexResponsesNamespace)
	turnHash := store.hash("turn", "turn-a")
	accountHash := store.hash("account", "account-a")
	slots := []CodexAttemptSlot{
		{Index: 1, AccountHash: accountHash, CandidateHash: store.hash("candidate", "candidate-a"), Kind: CodexAttemptSlotDirect},
		{Index: 2, AccountHash: accountHash, CandidateHash: store.hash("candidate", "candidate-refresh"), Kind: CodexAttemptSlotEligibleManagedRefresh},
	}
	envelope := codexLeaseJournalEnvelopeV2{
		Version:     codexLeaseJournalVersionV2,
		HashVersion: codexLeaseHashVersion,
		Generation:  3,
		Cutover: CodexLeaseCutover{
			SourceVersion:        0,
			CompatibilityEpoch:   3,
			State:                CodexLeaseCutoverComplete,
			At:                   cutoverAt,
			JournalGeneration:    1,
			CompletedAt:          cutoverAt,
			CompletionGeneration: 1,
			NoLegacyAuthority:    true,
		},
		Lanes: []CodexJournalLane{{
			SessionHash:          sessionHash,
			ThreadHash:           threadHash,
			NamespaceHash:        namespaceHash,
			Generation:           1,
			CurrentTurnHash:      turnHash,
			CurrentModeEpoch:     1,
			CurrentAuthoritative: true,
			LastTurnHash:         turnHash,
			LastModeEpoch:        1,
			LastAuthoritative:    true,
			LastObservedAt:       observedAt,
		}},
		Records: []CodexJournalRecordV2{{
			SessionHash:      sessionHash,
			ThreadHash:       threadHash,
			NamespaceHash:    namespaceHash,
			TurnHash:         turnHash,
			AccountHash:      accountHash,
			RecordGeneration: 2,
			LaneGeneration:   1,
			LeaseGeneration:  2,
			ModeEpoch:        1,
			State:            LeaseProvisional,
			ProtocolSchema:   CurrentCodexLeaseSchema,
			Authoritative:    true,
			CodexCurrentRequest: CodexCurrentRequest{
				Generation:               1,
				RequestKind:              CodexRequestTurn,
				CurrentAttemptGeneration: 1,
				AttemptEnvelope: CodexAttemptEnvelope{
					PolicyVersion: 1,
					AttemptLimit:  uint32(len(slots)),
					Slots:         slots,
				},
				RoutingRefs:        1,
				AttemptRefs:        1,
				RequestedModelHash: store.hash("requested-model", "gpt-5.4"),
				EffectiveModel:     "gpt-5.4",
				RequiredBuckets:    []CapacityBucket{CapacityBucketBase},
				Attempts: []CodexJournalAttempt{{
					Generation:     1,
					Revision:       1,
					Slot:           1,
					State:          CodexAttemptPrepared,
					CreatedAt:      attemptAt,
					LastObservedAt: attemptAt,
				}},
			},
			CreatedAt:      createdAt,
			LastObservedAt: observedAt,
		}},
	}
	codexLeaseV2RefreshPlanDigest(t, store, &envelope.Records[0])
	return store, envelope
}

func codexLeaseV2LegacyQuarantineSchemaFixture(t *testing.T, store *CodexLeaseStore, source codexLeaseJournalEnvelopeV2) codexLeaseJournalEnvelopeV2 {
	t.Helper()
	value := codexLeaseV2CloneSchemaFixture(t, source)
	value.Generation = 8
	value.Cutover = CodexLeaseCutover{
		SourceVersion:           1,
		CompatibilityEpoch:      3,
		State:                   CodexLeaseCutoverLegacyQuarantine,
		At:                      source.Cutover.At,
		JournalGeneration:       8,
		AuthoritativeModeEpochs: []uint64{2, 4},
		ShadowModeEpochs:        []uint64{3, 5},
		LegacyQuarantineUntil:   source.Cutover.At.Add(7 * 24 * time.Hour),
		LegacyV1SHA256:          strings.Repeat("a", 64),
	}
	value.Lanes = []CodexJournalLane{}
	value.Records = []CodexJournalRecordV2{}
	value.MAC = ""
	return value
}

func codexLeaseV2RunSemanticRejectionTable(t *testing.T, store *CodexLeaseStore, tests []struct {
	name   string
	base   codexLeaseJournalEnvelopeV2
	mutate func(*codexLeaseJournalEnvelopeV2)
}) {
	t.Helper()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := codexLeaseV2CloneSchemaFixture(t, test.base)
			test.mutate(&value)
			codexLeaseV2SignSchemaFixture(t, store, &value)
			err := store.validateV2Envelope(value)
			if err == nil {
				t.Fatal("validate signed semantically invalid envelope succeeded")
			}
			if !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("validation error = %v, want ErrCodexLeaseTrustLost", err)
			}
		})
	}
}

func codexLeaseV2RequireStrictDecodeFailure(t *testing.T, key, data []byte) {
	t.Helper()
	store := &CodexLeaseStore{key: append([]byte(nil), key...)}
	err := store.loadV2Locked(data)
	if err == nil {
		t.Fatal("strict decode accepted non-canonical JSON")
	}
	if !errors.Is(err, ErrCodexLeaseTrustLost) {
		t.Fatalf("strict decode error = %v, want ErrCodexLeaseTrustLost", err)
	}
}

func codexLeaseV2RequireStrictOpenFailureWithoutWrite(t *testing.T, key, data []byte) {
	t.Helper()
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return time.Date(2026, time.August, 9, 2, 0, 0, 0, time.UTC) },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}},
	}, codexLeaseV2CASTestOwner{})
	if coordinator != nil {
		_ = coordinator.Close()
	}
	if !errors.Is(err, ErrCodexLeaseTrustLost) {
		t.Fatalf("strict reopen error = %T %v, want trust lost", err, err)
	}
	after, readErr := fsys.ReadFile("/state/leases.json")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, data) {
		t.Fatal("strict reopen failure rewrote the installed journal")
	}
}

func codexLeaseV2SignSchemaFixture(t *testing.T, store *CodexLeaseStore, envelope *codexLeaseJournalEnvelopeV2) {
	t.Helper()
	envelope.MAC = ""
	mac, err := store.v2EnvelopeMAC(*envelope)
	if err != nil {
		t.Fatalf("sign v2 schema fixture: %v", err)
	}
	envelope.MAC = mac
	if !store.validV2EnvelopeMAC(*envelope) {
		t.Fatal("signed v2 schema fixture has invalid MAC")
	}
}

func codexLeaseV2RefreshPlanDigest(t *testing.T, store *CodexLeaseStore, record *CodexJournalRecordV2) {
	t.Helper()
	slots, err := json.Marshal(record.AttemptEnvelope.Slots)
	if err != nil {
		t.Fatalf("encode canonical attempt slots: %v", err)
	}
	record.AttemptEnvelope.PlanDigest = store.hash("plan", string(slots))
}

func codexLeaseV2SchemaJSON(t *testing.T, envelope codexLeaseJournalEnvelopeV2) []byte {
	t.Helper()
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode v2 schema fixture: %v", err)
	}
	return data
}

func codexLeaseV2SchemaRawObject(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode raw v2 schema fixture: %v", err)
	}
	return value
}

func codexLeaseV2SchemaRawJSON(t *testing.T, value map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode raw v2 schema fixture: %v", err)
	}
	return data
}

func codexLeaseV2SchemaObject(t *testing.T, value map[string]any, key string) map[string]any {
	t.Helper()
	object, ok := value[key].(map[string]any)
	if !ok {
		t.Fatalf("raw v2 schema member %q = %T, want object", key, value[key])
	}
	return object
}

func codexLeaseV2SchemaArrayObject(t *testing.T, value map[string]any, key string, index int) map[string]any {
	t.Helper()
	array, ok := value[key].([]any)
	if !ok || index < 0 || index >= len(array) {
		t.Fatalf("raw v2 schema member %q = %T/%d, want object at %d", key, value[key], len(array), index)
	}
	object, ok := array[index].(map[string]any)
	if !ok {
		t.Fatalf("raw v2 schema member %q[%d] = %T, want object", key, index, array[index])
	}
	return object
}

func codexLeaseV2SchemaCurrentRequest(t *testing.T, root map[string]any) map[string]any {
	t.Helper()
	return codexLeaseV2SchemaObject(t, codexLeaseV2SchemaArrayObject(t, root, "records", 0), "current_request")
}

func codexLeaseV2CloneSchemaFixture(t *testing.T, envelope codexLeaseJournalEnvelopeV2) codexLeaseJournalEnvelopeV2 {
	t.Helper()
	data := codexLeaseV2SchemaJSON(t, envelope)
	var clone codexLeaseJournalEnvelopeV2
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatalf("clone v2 schema fixture: %v", err)
	}
	return clone
}

func codexLeaseV2SchemaReplaceOnce(t *testing.T, value, old, replacement string) string {
	t.Helper()
	if strings.Count(value, old) != 1 {
		t.Fatalf("fixture contains %d copies of %q, want 1", strings.Count(value, old), old)
	}
	return strings.Replace(value, old, replacement, 1)
}

func codexLeaseV2AppendSecondSchemaLane(t *testing.T, store *CodexLeaseStore, envelope *codexLeaseJournalEnvelopeV2) {
	t.Helper()
	lane := envelope.Lanes[0]
	record := envelope.Records[0]
	lane.SessionHash = store.hash("session", "session-b")
	lane.ThreadHash = store.hash("thread", "thread-b")
	lane.CurrentTurnHash = store.hash("turn", "turn-b")
	lane.LastTurnHash = lane.CurrentTurnHash
	record.SessionHash = lane.SessionHash
	record.ThreadHash = lane.ThreadHash
	record.TurnHash = lane.CurrentTurnHash
	envelope.Lanes = append(envelope.Lanes, lane)
	envelope.Records = append(envelope.Records, record)
	sort.Slice(envelope.Lanes, func(i, j int) bool { return codexLeaseV2SchemaLaneLess(envelope.Lanes[i], envelope.Lanes[j]) })
	sort.Slice(envelope.Records, func(i, j int) bool { return codexLeaseV2SchemaRecordLess(envelope.Records[i], envelope.Records[j]) })
}

func codexLeaseV2SchemaLaneLess(left, right CodexJournalLane) bool {
	if left.SessionHash != right.SessionHash {
		return left.SessionHash < right.SessionHash
	}
	if left.ThreadHash != right.ThreadHash {
		return left.ThreadHash < right.ThreadHash
	}
	return left.NamespaceHash < right.NamespaceHash
}

func codexLeaseV2SchemaRecordLess(left, right CodexJournalRecordV2) bool {
	if left.SessionHash != right.SessionHash {
		return left.SessionHash < right.SessionHash
	}
	if left.ThreadHash != right.ThreadHash {
		return left.ThreadHash < right.ThreadHash
	}
	if left.NamespaceHash != right.NamespaceHash {
		return left.NamespaceHash < right.NamespaceHash
	}
	if left.TurnHash != right.TurnHash {
		return left.TurnHash < right.TurnHash
	}
	if left.ModeEpoch != right.ModeEpoch {
		return left.ModeEpoch < right.ModeEpoch
	}
	return !left.Authoritative && right.Authoritative
}
