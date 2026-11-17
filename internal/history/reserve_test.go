package history

import (
	"bytes"
	"testing"
)

func TestReadEstimatesKeepsHistoryUnchanged(t *testing.T) {
	store, fs := newTestStore(t)
	state := &BurnState{Version: schemaVersion, Accounts: map[string]*AccountState{
		"codex\x00a": {ProviderID: "codex", AccountKey: "a", Windows: map[string]*WindowState{
			"7d":      {EWMARatePctPerS: .02, Samples: 3, LastSeenUnix: 1800000000},
			"5h":      {EWMARatePctPerS: .03, Samples: 1, LastSeenUnix: 1800000000},
			"invalid": {EWMARatePctPerS: -1, Samples: 3, LastSeenUnix: 1800000000},
			"missing": nil,
		}},
		"missing": nil,
	}}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	before, _ := fs.ReadFile(store.path)
	estimates, err := store.ReadEstimates()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := estimates.Get(BurnRateKey{ProviderID: "codex", AccountKey: "a", Window: "7d"})
	if !ok || len(estimates) != 1 || got.RatePctPerS != .02 || got.Samples != 3 || got.LastSeenUnix != 1800000000 {
		t.Fatalf("estimates = %+v", estimates)
	}
	after, _ := fs.ReadFile(store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("read updated history")
	}
}

func TestReadEstimatesMissingHistoryStaysMissing(t *testing.T) {
	store, fs := newTestStore(t)
	got, err := store.ReadEstimates()
	if err != nil || len(got) != 0 {
		t.Fatalf("missing history = %+v, %v", got, err)
	}
	if _, err := fs.ReadFile(store.path); err == nil {
		t.Fatal("read created history")
	}
}
