package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDecodeCodexLeaseV1StrictJSONAcceptsCanonicalSignedFixture(t *testing.T) {
	store, envelope := codexLeaseV1StrictFixture(t)
	data := codexLeaseV1StrictJSON(t, envelope)

	var decoded codexLeaseJournalEnvelope
	if err := decodeCodexLeaseV1StrictJSON(data, &decoded); err != nil {
		t.Fatalf("decode canonical signed v1 fixture: %v", err)
	}
	if !store.validEnvelopeMAC(decoded) {
		t.Fatal("decoded canonical v1 fixture has invalid MAC")
	}
	if len(decoded.Records) != 1 || decoded.Records[0].Authoritative {
		t.Fatalf("decoded records = %#v, want one non-authoritative record", decoded.Records)
	}
}

func TestDecodeCodexLeaseV1StrictJSONDoesNotValidateMAC(t *testing.T) {
	_, envelope := codexLeaseV1StrictFixture(t)
	envelope.MAC = "MAC-validation-belongs-to-the-caller"

	var decoded codexLeaseJournalEnvelope
	if err := decodeCodexLeaseV1StrictJSON(codexLeaseV1StrictJSON(t, envelope), &decoded); err != nil {
		t.Fatalf("decode canonical v1 shape with unvalidated MAC: %v", err)
	}
	if decoded.MAC != envelope.MAC {
		t.Fatalf("decoded MAC = %q, want %q", decoded.MAC, envelope.MAC)
	}
}

func TestDecodeCodexLeaseV1StrictJSONRejectsMissingAndNullRequiredFalse(t *testing.T) {
	_, envelope := codexLeaseV1StrictFixture(t)
	canonical := string(codexLeaseV1StrictJSON(t, envelope))
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{name: "missing", old: `,"authoritative":false`, new: ""},
		{name: "null", old: `"authoritative":false`, new: `"authoritative":null`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := codexLeaseV1StrictReplaceOnce(t, canonical, test.old, test.new)
			codexLeaseV1StrictRequireFailure(t, []byte(mutated))
		})
	}
}

func TestDecodeCodexLeaseV1StrictJSONRejectsKnownExplicitZeroAliases(t *testing.T) {
	_, envelope := codexLeaseV1StrictFixture(t)
	canonical := string(codexLeaseV1StrictJSON(t, envelope))
	tests := []struct {
		name  string
		field string
		value string
	}{
		{name: "false boolean", field: "non_migratable", value: "false"},
		{name: "zero integer", field: "active_refs", value: "0"},
		{name: "null optional", field: "has_turn_state", value: "null"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := codexLeaseV1StrictReplaceOnce(
				t,
				canonical,
				`"last_seen":`,
				`"`+test.field+`":`+test.value+`,"last_seen":`,
			)
			codexLeaseV1StrictRequireFailure(t, []byte(mutated))
		})
	}
}

func TestDecodeCodexLeaseV1StrictJSONRejectsReorderedObjectMembers(t *testing.T) {
	_, envelope := codexLeaseV1StrictFixture(t)
	canonical := string(codexLeaseV1StrictJSON(t, envelope))
	record := envelope.Records[0]
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{name: "envelope", old: `{"version":1,"generation":3`, new: `{"generation":3,"version":1`},
		{
			name: "record",
			old:  `"records":[{"session_hash":"` + record.SessionHash + `","thread_hash":"` + record.ThreadHash + `"`,
			new:  `"records":[{"thread_hash":"` + record.ThreadHash + `","session_hash":"` + record.SessionHash + `"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := codexLeaseV1StrictReplaceOnce(t, canonical, test.old, test.new)
			codexLeaseV1StrictRequireFailure(t, []byte(mutated))
		})
	}
}

func TestDecodeCodexLeaseV1StrictJSONRequiresNonNullRecordsArray(t *testing.T) {
	_, envelope := codexLeaseV1StrictFixture(t)
	canonical := codexLeaseV1StrictJSON(t, envelope)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing", mutate: func(root map[string]any) { delete(root, "records") }},
		{name: "null", mutate: func(root map[string]any) { root["records"] = nil }},
		{name: "null record", mutate: func(root map[string]any) { root["records"] = []any{nil} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := codexLeaseV1StrictRawObject(t, canonical)
			test.mutate(root)
			codexLeaseV1StrictRequireFailure(t, codexLeaseV1StrictRawJSON(t, root))
		})
	}
}

func TestDecodeCodexLeaseV1StrictJSONAcceptsEmptyRecordsArray(t *testing.T) {
	store, envelope := codexLeaseV1StrictFixture(t)
	envelope.Records = []CodexJournalRecord{}
	envelope.MAC = ""
	mac, err := store.envelopeMAC(envelope)
	if err != nil {
		t.Fatalf("sign empty-records v1 fixture: %v", err)
	}
	envelope.MAC = mac

	var decoded codexLeaseJournalEnvelope
	if err := decodeCodexLeaseV1StrictJSON(codexLeaseV1StrictJSON(t, envelope), &decoded); err != nil {
		t.Fatalf("decode canonical empty records array: %v", err)
	}
	if decoded.Records == nil || len(decoded.Records) != 0 {
		t.Fatalf("decoded records = %#v, want non-nil empty array", decoded.Records)
	}
}

func TestDecodeCodexLeaseV1StrictJSONRejectsUnknownDuplicateNestedAndTrailingJSON(t *testing.T) {
	_, envelope := codexLeaseV1StrictFixture(t)
	canonical := string(codexLeaseV1StrictJSON(t, envelope))
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "unknown nested field",
			data: []byte(codexLeaseV1StrictReplaceOnce(
				t,
				canonical,
				`"session_hash":`,
				`"unknown_record":true,"session_hash":`,
			)),
		},
		{
			name: "duplicate nested field",
			data: []byte(codexLeaseV1StrictReplaceOnce(
				t,
				canonical,
				`"authoritative":false`,
				`"authoritative":false,"authoritative":false`,
			)),
		},
		{name: "trailing value", data: append([]byte(canonical), []byte(` {}`)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			codexLeaseV1StrictRequireFailure(t, test.data)
		})
	}
}

func TestDecodeCodexLeaseV1StrictJSONRejectsNilDestination(t *testing.T) {
	_, envelope := codexLeaseV1StrictFixture(t)
	if err := decodeCodexLeaseV1StrictJSON(codexLeaseV1StrictJSON(t, envelope), nil); err == nil {
		t.Fatal("strict v1 decoder accepted nil destination")
	}
}

func codexLeaseV1StrictFixture(t *testing.T) (*CodexLeaseStore, codexLeaseJournalEnvelope) {
	t.Helper()
	store := &CodexLeaseStore{key: bytes.Repeat([]byte{0x31}, codexLeaseHMACKeyBytes)}
	envelope := codexLeaseJournalEnvelope{
		Version:    codexLeaseJournalVersion,
		Generation: 3,
		Records: []CodexJournalRecord{{
			SessionHash:     store.hash("session", "strict-v1-session"),
			ThreadHash:      store.hash("thread", "strict-v1-thread"),
			TurnHash:        store.hash("turn", "strict-v1-turn"),
			NamespaceHash:   store.hash("namespace", CodexResponsesNamespace),
			AccountHash:     store.hash("account", "strict-v1-account"),
			State:           LeaseBoundQuiescent,
			LeaseGeneration: 2,
			ModeEpoch:       4,
			Authoritative:   false,
			LastSeen:        time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC),
		}},
	}
	mac, err := store.envelopeMAC(envelope)
	if err != nil {
		t.Fatalf("sign canonical v1 fixture: %v", err)
	}
	envelope.MAC = mac
	return store, envelope
}

func codexLeaseV1StrictJSON(t *testing.T, envelope codexLeaseJournalEnvelope) []byte {
	t.Helper()
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode v1 strict fixture: %v", err)
	}
	return data
}

func codexLeaseV1StrictRequireFailure(t *testing.T, data []byte) {
	t.Helper()
	var decoded codexLeaseJournalEnvelope
	if err := decodeCodexLeaseV1StrictJSON(data, &decoded); err == nil {
		t.Fatal("strict v1 decoder accepted non-canonical JSON")
	}
}

func codexLeaseV1StrictReplaceOnce(t *testing.T, value, old, replacement string) string {
	t.Helper()
	if strings.Count(value, old) != 1 {
		t.Fatalf("fixture contains %q %d times, want once", old, strings.Count(value, old))
	}
	return strings.Replace(value, old, replacement, 1)
}

func codexLeaseV1StrictRawObject(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode raw v1 fixture: %v", err)
	}
	return value
}

func codexLeaseV1StrictRawJSON(t *testing.T, value map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode raw v1 fixture: %v", err)
	}
	return data
}
