package modelregistry

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func makeSnapshot() Snapshot {
	return Snapshot{
		Entries: []Entry{
			{Provider: ProviderAnthropic, ID: "claude-3-5-sonnet-20241022", Source: SourceNative},
			{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
		},
		CodexRawByID: map[string]json.RawMessage{
			"gpt-5.4": json.RawMessage(`{"id":"gpt-5.4"}`),
		},
		AnthropicRawByID: map[string]json.RawMessage{
			"claude-3-5-sonnet-20241022": json.RawMessage(`{"id":"claude-3-5-sonnet-20241022"}`),
		},
		FetchedAt: time.Now(),
	}
}

func TestCatalog_SnapshotImmutable(t *testing.T) {
	c := NewCatalog(makeSnapshot())
	snap := c.Snapshot()

	// Mutating the returned slice must not affect the internal state.
	snap.Entries[0].ID = "mutated"

	snap2 := c.Snapshot()
	if snap2.Entries[0].ID == "mutated" {
		t.Fatal("mutating returned Entries slice affected internal catalog state")
	}

	// Mutating a returned map value must not affect internal state.
	snap.CodexRawByID["gpt-5.4"] = json.RawMessage(`{"mutated":true}`)
	snap3 := c.Snapshot()
	if string(snap3.CodexRawByID["gpt-5.4"]) != `{"id":"gpt-5.4"}` {
		t.Fatalf("mutating returned CodexRawByID affected internal state: %s", snap3.CodexRawByID["gpt-5.4"])
	}

	// Mutating a raw message byte slice must not affect internal state.
	raw := snap.AnthropicRawByID["claude-3-5-sonnet-20241022"]
	raw[0] = 'X'
	snap4 := c.Snapshot()
	if snap4.AnthropicRawByID["claude-3-5-sonnet-20241022"][0] == 'X' {
		t.Fatal("mutating returned json.RawMessage bytes affected internal state")
	}
}

func TestCatalog_ReplaceIsVisible(t *testing.T) {
	c := NewCatalog(makeSnapshot())
	newSnap := Snapshot{
		Entries:   []Entry{{Provider: ProviderAnthropic, ID: "new-model", Source: SourceOverlay}},
		FetchedAt: time.Now(),
	}
	c.Replace(newSnap)

	got := c.Snapshot()
	if len(got.Entries) != 1 || got.Entries[0].ID != "new-model" {
		t.Fatalf("after Replace, Snapshot() = %+v, want single entry new-model", got.Entries)
	}
}

func TestCatalog_ConcurrentReplaceAndSnapshot(t *testing.T) {
	c := NewCatalog(makeSnapshot())

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			c.Replace(makeSnapshot())
		}()
		go func() {
			defer wg.Done()
			snap := c.Snapshot()
			// Access fields to exercise the race detector.
			_ = len(snap.Entries)
			_ = len(snap.CodexRawByID)
		}()
	}
	wg.Wait()
}

func TestCatalog_NilInitial(t *testing.T) {
	// NewCatalog with zero Snapshot must not panic.
	c := NewCatalog(Snapshot{})
	snap := c.Snapshot()
	if snap.Entries != nil {
		t.Fatalf("expected nil Entries on empty Snapshot, got %v", snap.Entries)
	}
}

func TestCatalog_NewCatalogIngressDeepCopied(t *testing.T) {
	entries := []Entry{
		{Provider: ProviderAnthropic, ID: "model-a", Source: SourceNative},
	}
	snap := Snapshot{Entries: entries, FetchedAt: time.Now()}
	c := NewCatalog(snap)

	entries[0].ID = "mutated-after-new-catalog"

	got := c.Snapshot()
	if got.Entries[0].ID == "mutated-after-new-catalog" {
		t.Fatal("mutating source slice after NewCatalog affected catalog state")
	}
}

func TestCatalog_ReplaceIngressDeepCopied(t *testing.T) {
	entries := []Entry{
		{Provider: ProviderCodex, ID: "gpt-5.4", Source: SourceNative},
	}
	snap := Snapshot{
		Entries: entries,
		CodexRawByID: map[string]json.RawMessage{
			"gpt-5.4": json.RawMessage(`{"slug":"gpt-5.4"}`),
		},
		FetchedAt: time.Now(),
	}
	c := NewCatalog(Snapshot{})
	c.Replace(snap)

	entries[0].ID = "mutated-after-replace"
	snap.CodexRawByID["gpt-5.4"][0] = 'X'

	got := c.Snapshot()
	if got.Entries[0].ID == "mutated-after-replace" {
		t.Fatal("mutating source slice after Replace affected catalog state")
	}
	if got.CodexRawByID["gpt-5.4"][0] == 'X' {
		t.Fatal("mutating source raw message after Replace affected catalog state")
	}
}
