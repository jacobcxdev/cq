package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCodexCapacityObservationStreamOrdersFacts(t *testing.T) {
	ledger := NewCodexCapacityLedger(nil, time.Hour)
	firstStream := ledger.NewObservationStream()
	first := firstStream.Stamp(CapacityFact{ConnectionGeneration: 99, Sequence: 99})
	second := firstStream.Stamp(CapacityFact{})
	secondStreamFirst := ledger.NewObservationStream().Stamp(CapacityFact{})

	if first.ConnectionGeneration == 0 {
		t.Fatal("first generation is zero")
	}
	if second.ConnectionGeneration != first.ConnectionGeneration {
		t.Fatalf("second generation = %d, want %d", second.ConnectionGeneration, first.ConnectionGeneration)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("first stream sequences = %d, %d, want 1, 2", first.Sequence, second.Sequence)
	}
	if secondStreamFirst.ConnectionGeneration <= first.ConnectionGeneration {
		t.Fatalf("second stream generation = %d, want greater than %d", secondStreamFirst.ConnectionGeneration, first.ConnectionGeneration)
	}
	if secondStreamFirst.Sequence != 1 {
		t.Fatalf("second stream sequence = %d, want 1", secondStreamFirst.Sequence)
	}
}

func TestCodexCapacityObservationStreamsAreRaceSafe(t *testing.T) {
	const (
		streamCount    = 32
		factsPerStream = 32
	)
	type cursor struct {
		generation uint64
		sequence   uint64
	}

	ledger := NewCodexCapacityLedger(nil, time.Hour)
	cursors := make(chan cursor, streamCount*factsPerStream)
	streams := make([]*CodexCapacityObservationStream, streamCount)
	var wg sync.WaitGroup
	for index := range streamCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			streams[index] = ledger.NewObservationStream()
		}()
	}
	wg.Wait()
	for range factsPerStream {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, stream := range streams {
				fact := stream.Stamp(CapacityFact{})
				cursors <- cursor{generation: fact.ConnectionGeneration, sequence: fact.Sequence}
			}
		}()
	}
	wg.Wait()
	close(cursors)

	seen := make(map[cursor]bool, streamCount*factsPerStream)
	sequencesByGeneration := make(map[uint64]map[uint64]bool, streamCount)
	for got := range cursors {
		if got.generation == 0 || got.sequence == 0 {
			t.Fatalf("zero cursor component: %+v", got)
		}
		if seen[got] {
			t.Fatalf("duplicate cursor: %+v", got)
		}
		seen[got] = true
		if sequencesByGeneration[got.generation] == nil {
			sequencesByGeneration[got.generation] = make(map[uint64]bool, factsPerStream)
		}
		sequencesByGeneration[got.generation][got.sequence] = true
	}
	if len(sequencesByGeneration) != streamCount {
		t.Fatalf("generation count = %d, want %d", len(sequencesByGeneration), streamCount)
	}
	for generation, sequences := range sequencesByGeneration {
		if len(sequences) != factsPerStream {
			t.Fatalf("generation %d sequence count = %d, want %d", generation, len(sequences), factsPerStream)
		}
		for sequence := uint64(1); sequence <= factsPerStream; sequence++ {
			if !sequences[sequence] {
				t.Fatalf("generation %d missing sequence %d", generation, sequence)
			}
		}
	}
}

func TestCodexCapacityLedgerRejectsInvalidFacts(t *testing.T) {
	now := time.Unix(900, 0)
	baseline := CapacityFact{
		AccountKey: "account-a", Bucket: CapacityBucketBase,
		Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 63,
		ObservedAt: now, Confidence: CapacityConfidenceAdvisory,
	}
	tests := []struct {
		name string
		fact CapacityFact
	}{
		{name: "unknown source", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySource(99), Sequence: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative}},
		{name: "unknown confidence", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceUsageCache, Sequence: 2, RemainingPct: 0, ObservedAt: now.Add(time.Second), Confidence: CapacityConfidence(99)}},
		{name: "negative percentage", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceUsageCache, Sequence: 2, RemainingPct: -1, ObservedAt: now.Add(time.Second), Confidence: CapacityConfidenceAdvisory}},
		{name: "percentage over one hundred", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceUsageCache, Sequence: 2, RemainingPct: 101, ObservedAt: now.Add(time.Second), Confidence: CapacityConfidenceAdvisory}},
		{name: "authoritative usage", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceUsageCache, Sequence: 2, RemainingPct: 0, ObservedAt: now.Add(time.Second), Confidence: CapacityConfidenceAuthoritative}},
		{name: "usage connection generation", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceUsageCache, ConnectionGeneration: 1, Sequence: 2, RemainingPct: 0, ObservedAt: now.Add(time.Second), Confidence: CapacityConfidenceAdvisory}},
		{name: "usage missing sequence", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceUsageCache, RemainingPct: 0, ObservedAt: now.Add(time.Second), Confidence: CapacityConfidenceAdvisory}},
		{name: "advisory hard limit", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceHardLimit, ConnectionGeneration: 1, Sequence: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAdvisory}},
		{name: "positive hard limit", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceHardLimit, ConnectionGeneration: 1, Sequence: 1, RemainingPct: 1, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative}},
		{name: "advisory headers", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceHTTPHeaders, ConnectionGeneration: 1, Sequence: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAdvisory}},
		{name: "advisory live event", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 1, Sequence: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAdvisory}},
		{name: "network generation missing", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceHTTPHeaders, Sequence: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative}},
		{name: "network sequence missing", fact: CapacityFact{AccountKey: "account-a", Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
			if !ledger.Observe(baseline) {
				t.Fatal("valid baseline rejected")
			}
			before := ledger.Capacity("account-a", CapacityBucketBase)
			if ledger.Observe(tt.fact) {
				t.Fatalf("Observe(%+v) accepted", tt.fact)
			}
			if after := ledger.Capacity("account-a", CapacityBucketBase); after != before {
				t.Fatalf("capacity changed from %+v to %+v", before, after)
			}
		})
	}
}

func TestCodexCapacityLedgerAdvisoryZeroIsUnknown(t *testing.T) {
	now := time.Unix(950, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	window := func(remaining int) quota.Result {
		return quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window7Day: {RemainingPct: remaining},
		}}
	}

	ledger.ObserveQuotaSnapshot(key, QuotaSnapshot{Result: window(0), FetchedAt: now})
	view := ledger.Capacity(key, CapacityBucketBase)
	if view.State != CapacityUnknown || view.RemainingPct != -1 || view.Source != CapacitySourceUsageCache {
		t.Fatalf("advisory zero capacity = %+v, want unknown", view)
	}

	ledger.ObserveQuotaSnapshot(key, QuotaSnapshot{Result: window(42), FetchedAt: now.Add(time.Second)})
	view = ledger.Capacity(key, CapacityBucketBase)
	if view.State != CapacityPositive || view.RemainingPct != 42 {
		t.Fatalf("advisory positive capacity = %+v, want positive 42%%", view)
	}
}

func TestCodexCapacityLedgerAuthoritativeZeroGates(t *testing.T) {
	now := time.Unix(975, 0)
	for _, source := range []CapacitySource{
		CapacitySourceHardLimit,
		CapacitySourceHTTPHeaders,
		CapacitySourceLiveRateLimits,
	} {
		t.Run(fmt.Sprintf("source_%d", source), func(t *testing.T) {
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
			fact := CapacityFact{
				AccountKey: "account-a", Bucket: CapacityBucketBase,
				Source: source, ConnectionGeneration: 1, Sequence: 1,
				RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative,
			}
			if !ledger.Observe(fact) {
				t.Fatalf("Observe(%+v) rejected", fact)
			}
			if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityZero {
				t.Fatalf("capacity = %+v, want zero", view)
			}
			if view := ledger.Capacity("account-a", CapacityBucket("model:other")); view.State != CapacityUnknown || view.Exact {
				t.Fatalf("other bucket capacity = %+v, want inexact unknown", view)
			}
			if view := ledger.Capacity("account-b", CapacityBucketBase); view.State != CapacityUnknown {
				t.Fatalf("other account capacity = %+v, want unknown", view)
			}
		})
	}
}

func TestCodexCapacityLedgerSourcePrecedence(t *testing.T) {
	now := time.Unix(1_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	for _, fact := range []CapacityFact{
		{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 80, ObservedAt: now, Confidence: CapacityConfidenceAdvisory},
		{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceHTTPHeaders, ConnectionGeneration: 1, Sequence: 1, RemainingPct: 60, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative},
		{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 2, Sequence: 1, RemainingPct: 40, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative},
	} {
		if !ledger.Observe(fact) {
			t.Fatalf("Observe(%+v) rejected", fact)
		}
	}
	view := ledger.Capacity(key, CapacityBucketBase)
	if view.State != CapacityPositive || view.RemainingPct != 40 || view.Source != CapacitySourceLiveRateLimits {
		t.Fatalf("capacity = %+v, want live 40%%", view)
	}
}

func TestCodexCapacityLedgerRejectsStaleStreamFacts(t *testing.T) {
	now := time.Unix(2_000, 0)
	key := codex.AccountKey("account-a")
	base := CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 2, Sequence: 3, RemainingPct: 60, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative}
	for _, tt := range []struct {
		name string
		fact CapacityFact
	}{
		{name: "lower generation", fact: CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 1, Sequence: 99, RemainingPct: 0, ObservedAt: now.Add(time.Minute), Confidence: CapacityConfidenceAuthoritative}},
		{name: "lower sequence", fact: CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 2, Sequence: 2, RemainingPct: 0, ObservedAt: now.Add(time.Minute), Confidence: CapacityConfidenceAuthoritative}},
		{name: "equal cursor later timestamp", fact: CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 2, Sequence: 3, RemainingPct: 0, ObservedAt: now.Add(time.Minute), Confidence: CapacityConfidenceAuthoritative}},
		{name: "equal cursor later reset", fact: CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 2, Sequence: 3, RemainingPct: 0, ObservedAt: now, ResetAt: now.Add(time.Hour), Confidence: CapacityConfidenceAuthoritative}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
			if !ledger.Observe(base) {
				t.Fatal("initial fact rejected")
			}
			if ledger.Observe(tt.fact) {
				t.Fatalf("stale fact accepted: %+v", tt.fact)
			}
			if got := ledger.Capacity(key, CapacityBucketBase).RemainingPct; got != 60 {
				t.Fatalf("remaining = %d, want 60", got)
			}
		})
	}

	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	if !ledger.Observe(base) {
		t.Fatal("initial fact rejected")
	}
	if !ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 3, Sequence: 1, RemainingPct: 70, ObservedAt: now.Add(-time.Minute), Confidence: CapacityConfidenceAuthoritative}) {
		t.Fatal("new connection generation rejected")
	}
}

func TestCodexCapacityLedgerRetainsHighWaterAfterExpiry(t *testing.T) {
	for _, tt := range []struct {
		name    string
		maxAge  time.Duration
		resetAt time.Time
		advance time.Duration
	}{
		{name: "reset", maxAge: time.Hour, resetAt: time.Unix(2_500, 0).Add(time.Minute), advance: time.Minute},
		{name: "horizon", maxAge: time.Minute, advance: time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Unix(2_500, 0)
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, tt.maxAge)
			fact := CapacityFact{
				AccountKey: "account-a", Bucket: CapacityBucketBase,
				Source: CapacitySourceHTTPHeaders, ConnectionGeneration: 5, Sequence: 3,
				RemainingPct: 0, ObservedAt: now, ResetAt: tt.resetAt,
				Confidence: CapacityConfidenceAuthoritative,
			}
			if !ledger.Observe(fact) {
				t.Fatal("initial fact rejected")
			}
			now = now.Add(tt.advance)
			if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityUnknown {
				t.Fatalf("expired capacity = %+v, want unknown", view)
			}

			equal := fact
			equal.RemainingPct = 90
			equal.ObservedAt = now
			equal.ResetAt = now.Add(time.Hour)
			if ledger.Observe(equal) {
				t.Fatal("equal cursor repopulated expired fact")
			}
			older := equal
			older.ConnectionGeneration = 4
			older.Sequence = 99
			if ledger.Observe(older) {
				t.Fatal("older cursor repopulated expired fact")
			}
			newer := equal
			newer.ConnectionGeneration = 6
			newer.Sequence = 1
			if !ledger.Observe(newer) {
				t.Fatal("newer cursor rejected after expiry")
			}
			if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 90 {
				t.Fatalf("new capacity = %+v, want positive 90%%", view)
			}
		})
	}
}

func TestCodexCapacityLedgerExpiresAtResetOrHorizon(t *testing.T) {
	now := time.Unix(3_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, 10*time.Minute)
	key := codex.AccountKey("account-a")
	ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceHTTPHeaders, ConnectionGeneration: 1, Sequence: 1, RemainingPct: 0, ObservedAt: now, ResetAt: now.Add(2 * time.Minute), Confidence: CapacityConfidenceAuthoritative})
	now = now.Add(2 * time.Minute)
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityUnknown {
		t.Fatalf("at reset capacity = %+v, want unknown", view)
	}
	ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAdvisory})
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityUnknown || view.RemainingPct != -1 {
		t.Fatalf("post-reset advisory capacity = %+v, want unknown", view)
	}
	now = now.Add(10 * time.Minute)
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityUnknown {
		t.Fatalf("past horizon capacity = %+v, want unknown", view)
	}
}

func TestCodexCapacityLedgerHardZeroFence(t *testing.T) {
	now := time.Unix(4_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 1, Sequence: 1, RemainingPct: 70, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative})
	ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceHardLimit, ConnectionGeneration: 2, Sequence: 1, RemainingPct: 0, ObservedAt: now.Add(time.Second), ResetAt: now.Add(time.Hour), Confidence: CapacityConfidenceAuthoritative})
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityZero || view.Source != CapacitySourceHardLimit {
		t.Fatalf("fenced capacity = %+v, want hard zero", view)
	}
	if !ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 3, Sequence: 1, RemainingPct: 55, ObservedAt: now.Add(2 * time.Second), ResetAt: now.Add(time.Hour), Confidence: CapacityConfidenceAuthoritative}) {
		t.Fatal("newer live fact rejected")
	}
	if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 55 {
		t.Fatalf("lifted capacity = %+v, want positive 55", view)
	}
}

func TestCodexCapacityLedgerHardZeroRequiresNewerLivePositive(t *testing.T) {
	now := time.Unix(4_500, 0)
	hardReset := now.Add(time.Hour)
	hard := CapacityFact{
		AccountKey: "account-a", Bucket: CapacityBucketBase,
		Source: CapacitySourceHardLimit, ConnectionGeneration: 5, Sequence: 2,
		RemainingPct: 0, ObservedAt: now, ResetAt: hardReset,
		Confidence: CapacityConfidenceAuthoritative,
	}
	live := func(generation, sequence uint64) CapacityFact {
		return CapacityFact{
			AccountKey: "account-a", Bucket: CapacityBucketBase,
			Source:               CapacitySourceLiveRateLimits,
			ConnectionGeneration: generation, Sequence: sequence,
			RemainingPct: 55, ObservedAt: now.Add(time.Minute), ResetAt: hardReset,
			Confidence: CapacityConfidenceAuthoritative,
		}
	}
	tests := []struct {
		name         string
		fact         CapacityFact
		wantAccepted bool
		wantLift     bool
	}{
		{name: "lower generation", fact: live(4, 99), wantAccepted: true},
		{name: "equal cursor timestamp only", fact: live(5, 2), wantAccepted: true},
		{name: "same generation lower sequence", fact: live(5, 1), wantAccepted: true},
		{name: "same generation higher sequence", fact: func() CapacityFact { fact := live(5, 3); fact.ObservedAt = now.Add(-time.Minute); return fact }(), wantAccepted: true, wantLift: true},
		{name: "higher generation", fact: func() CapacityFact { fact := live(6, 1); fact.ObservedAt = now.Add(-time.Minute); return fact }(), wantAccepted: true, wantLift: true},
		{name: "usage positive", fact: CapacityFact{AccountKey: "account-a", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 55, ObservedAt: now.Add(time.Minute), Confidence: CapacityConfidenceAdvisory}, wantAccepted: true},
		{name: "header positive", fact: CapacityFact{AccountKey: "account-a", Bucket: CapacityBucketBase, Source: CapacitySourceHTTPHeaders, ConnectionGeneration: 6, Sequence: 1, RemainingPct: 55, ObservedAt: now.Add(time.Minute), ResetAt: hardReset, Confidence: CapacityConfidenceAuthoritative}, wantAccepted: true},
		{name: "missing live cursor", fact: CapacityFact{AccountKey: "account-a", Bucket: CapacityBucketBase, Source: CapacitySourceLiveRateLimits, RemainingPct: 55, ObservedAt: now.Add(time.Minute), ResetAt: hardReset, Confidence: CapacityConfidenceAuthoritative}},
		{name: "other bucket", fact: CapacityFact{AccountKey: "account-a", Bucket: CapacityBucket("model:other"), Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 6, Sequence: 1, RemainingPct: 55, ObservedAt: now.Add(time.Minute), ResetAt: hardReset, Confidence: CapacityConfidenceAuthoritative}, wantAccepted: true},
		{name: "reset epoch regression", fact: func() CapacityFact { fact := live(6, 1); fact.ResetAt = hardReset.Add(-time.Minute); return fact }(), wantAccepted: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, 2*time.Hour)
			if !ledger.Observe(hard) {
				t.Fatal("hard fact rejected")
			}
			if accepted := ledger.Observe(tt.fact); accepted != tt.wantAccepted {
				t.Fatalf("Observe(%+v) = %t, want %t", tt.fact, accepted, tt.wantAccepted)
			}
			view := ledger.Capacity("account-a", CapacityBucketBase)
			if tt.wantLift {
				if view.State != CapacityPositive || view.RemainingPct != 55 || view.Source != CapacitySourceLiveRateLimits {
					t.Fatalf("lifted capacity = %+v, want live positive", view)
				}
			} else if view.State != CapacityZero || view.Source != CapacitySourceHardLimit {
				t.Fatalf("fenced capacity = %+v, want hard zero", view)
			}
		})
	}
}

func TestCodexCapacityLedgerLateOlderHardZeroDoesNotRefence(t *testing.T) {
	now := time.Unix(4_750, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, 10*time.Minute)
	if !ledger.Observe(CapacityFact{
		AccountKey: "account-a", Bucket: CapacityBucketBase,
		Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 8, Sequence: 1,
		RemainingPct: 70, ObservedAt: now,
		Confidence: CapacityConfidenceAuthoritative,
	}) {
		t.Fatal("live fact rejected")
	}
	now = now.Add(5 * time.Minute)
	if !ledger.Observe(CapacityFact{
		AccountKey: "account-a", Bucket: CapacityBucketBase,
		Source: CapacitySourceHardLimit, ConnectionGeneration: 7, Sequence: 1,
		RemainingPct: 0, ObservedAt: now,
		Confidence: CapacityConfidenceAuthoritative,
	}) {
		t.Fatal("late hard fact rejected")
	}
	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 70 {
		t.Fatalf("capacity = %+v, want newer live positive", view)
	}
	now = now.Add(5 * time.Minute)
	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityUnknown {
		t.Fatalf("capacity after live horizon = %+v, want unknown without old hard re-fence", view)
	}
	if !ledger.Observe(CapacityFact{
		AccountKey: "account-a", Bucket: CapacityBucketBase,
		Source: CapacitySourceHardLimit, ConnectionGeneration: 9, Sequence: 1,
		RemainingPct: 0, ObservedAt: now,
		Confidence: CapacityConfidenceAuthoritative,
	}) {
		t.Fatal("newer hard fact rejected")
	}
	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityZero || view.Source != CapacitySourceHardLimit {
		t.Fatalf("capacity after newer hard fact = %+v, want hard zero", view)
	}
}

func TestCodexCapacityLedgerLateOlderHardZeroHonoursResetEpoch(t *testing.T) {
	now := time.Unix(4_850, 0)
	liveReset := now.Add(5 * time.Minute)
	hardReset := now.Add(time.Hour)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, 2*time.Hour)
	if !ledger.Observe(CapacityFact{
		AccountKey: "account-a", Bucket: CapacityBucketBase,
		Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 8, Sequence: 1,
		RemainingPct: 70, ObservedAt: now, ResetAt: liveReset,
		Confidence: CapacityConfidenceAuthoritative,
	}) {
		t.Fatal("live fact rejected")
	}
	now = now.Add(time.Minute)
	if !ledger.Observe(CapacityFact{
		AccountKey: "account-a", Bucket: CapacityBucketBase,
		Source: CapacitySourceHardLimit, ConnectionGeneration: 7, Sequence: 1,
		RemainingPct: 0, ObservedAt: now, ResetAt: hardReset,
		Confidence: CapacityConfidenceAuthoritative,
	}) {
		t.Fatal("late hard fact rejected")
	}
	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityZero || view.Source != CapacitySourceHardLimit || view.ResetAt != hardReset {
		t.Fatalf("capacity = %+v, want longer hard fence", view)
	}
}

func TestCodexCapacityLedgerConcurrentDeliveryUsesCursorOrder(t *testing.T) {
	now := time.Unix(4_900, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	newerAccepted := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ledger.Observe(CapacityFact{
			AccountKey: "account-a", Bucket: CapacityBucketBase,
			Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 2, Sequence: 1,
			RemainingPct: 80, ObservedAt: now, Confidence: CapacityConfidenceAuthoritative,
		})
		close(newerAccepted)
	}()
	go func() {
		defer wg.Done()
		<-newerAccepted
		ledger.Observe(CapacityFact{
			AccountKey: "account-a", Bucket: CapacityBucketBase,
			Source: CapacitySourceLiveRateLimits, ConnectionGeneration: 1, Sequence: 99,
			RemainingPct: 10, ObservedAt: now.Add(time.Minute), Confidence: CapacityConfidenceAuthoritative,
		})
	}()
	wg.Wait()

	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 80 {
		t.Fatalf("capacity = %+v, want newer cursor 80%%", view)
	}
}

func TestCodexCapacityLedgerScopedBucketAndSharedFallback(t *testing.T) {
	now := time.Unix(5_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	ledger.ObserveQuotaSnapshot(key, QuotaSnapshot{
		FetchedAt: now,
		Result: quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour:                          {RemainingPct: 0},
			quota.WindowName("5h:gpt-5.3-codex-spark"): {RemainingPct: 70},
			quota.WindowName("7d:gpt-5.3-codex-spark"): {RemainingPct: 40},
		}},
	})
	spark := ledger.Capacity(key, CapacityBucketForModel(codexSparkModel))
	if spark.State != CapacityPositive || spark.RemainingPct != 40 || !spark.Exact {
		t.Fatalf("spark capacity = %+v, want exact 40%%", spark)
	}
	other := ledger.Capacity(key, CapacityBucket("model:other"))
	if other.State != CapacityUnknown || other.Exact {
		t.Fatalf("other capacity = %+v, want advisory shared fallback to remain unknown", other)
	}
}

func TestCodexCapacityLedgerRejectsStaleUsageSnapshots(t *testing.T) {
	now := time.Unix(5_500, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	key := codex.AccountKey("account-a")
	result := func(remaining int) quota.Result {
		return quota.Result{Windows: map[quota.WindowName]quota.Window{
			quota.Window5Hour: {RemainingPct: remaining},
		}}
	}
	ledger.ObserveQuotaSnapshot(key, QuotaSnapshot{Result: result(75), FetchedAt: now})
	for _, fetchedAt := range []time.Time{now, now.Add(-time.Minute)} {
		ledger.ObserveQuotaSnapshot(key, QuotaSnapshot{Result: result(0), FetchedAt: fetchedAt})
		if view := ledger.Capacity(key, CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 75 {
			t.Fatalf("capacity after snapshot at %v = %+v, want newer 75%% snapshot", fetchedAt, view)
		}
	}
}

func TestCodexRouteChoicePrefersKnownPositiveAndIgnoresSystemActive(t *testing.T) {
	now := time.Unix(6_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	ledger.Observe(CapacityFact{AccountKey: "known", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 20, ObservedAt: now, Confidence: CapacityConfidenceAdvisory})
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountKey: "unknown", AccessToken: "token-a", IsActive: true},
			{AccountKey: "known", AccessToken: "token-b"},
		}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "known" {
		t.Fatalf("account = %q, want known", choice.AccountKey)
	}
}

func TestCodexRouteChoiceUsesActiveLeaseCountOnlyAsTieBreak(t *testing.T) {
	now := time.Unix(7_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	for _, key := range []codex.AccountKey{"busy", "idle"} {
		ledger.Observe(CapacityFact{AccountKey: key, Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 50, ObservedAt: now, Confidence: CapacityConfidenceAdvisory})
	}
	ledger.SetActiveLeases("busy", 2)
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{{AccountKey: "busy", AccessToken: "a"}, {AccountKey: "idle", AccessToken: "b", IsActive: true}}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "idle" {
		t.Fatalf("account = %q, want idle", choice.AccountKey)
	}
}

func TestCodexRouteChoiceUsesExactSparkCapacityAndScopedFallback(t *testing.T) {
	now := time.Unix(8_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAdvisory})
	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketForModel(codexSparkModel), Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 60, ObservedAt: now, Confidence: CapacityConfidenceAdvisory})
	ledger.Observe(CapacityFact{AccountKey: "plus", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 40, ObservedAt: now, Confidence: CapacityConfidenceAdvisory})
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountKey: "plus", AccessToken: "plus-token", PlanType: "plus"},
			{AccountKey: "pro", AccessToken: "pro-token", PlanType: "pro"},
		}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: codexSparkModel})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "pro" || choice.EffectiveModel != codexSparkModel {
		t.Fatalf("choice = %+v, want pro Spark", choice)
	}
	if len(choice.RequiredBuckets) != 1 || choice.RequiredBuckets[0] != CapacityBucketForModel(codexSparkModel) {
		t.Fatalf("buckets = %v, want Spark", choice.RequiredBuckets)
	}

	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketForModel(codexSparkModel), Source: CapacitySourceUsageCache, Sequence: 2, RemainingPct: 0, ObservedAt: now.Add(time.Second), Confidence: CapacityConfidenceAdvisory})
	choice, err = selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: codexSparkModel})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "plus" || choice.EffectiveModel != codexFallbackModel {
		t.Fatalf("fallback choice = %+v, want plus base model", choice)
	}
}

func TestCodexRouteChoiceRequiresAllPreturnBuckets(t *testing.T) {
	now := time.Unix(9_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 0, ObservedAt: now, Confidence: CapacityConfidenceAdvisory})
	ledger.Observe(CapacityFact{AccountKey: "pro", Bucket: CapacityBucketForModel(codexSparkModel), Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 80, ObservedAt: now, Confidence: CapacityConfidenceAdvisory})
	ledger.Observe(CapacityFact{AccountKey: "plus", Bucket: CapacityBucketBase, Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 30, ObservedAt: now, Confidence: CapacityConfidenceAdvisory})
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{
			{AccountKey: "pro", AccessToken: "pro-token", PlanType: "pro"},
			{AccountKey: "plus", AccessToken: "plus-token", PlanType: "plus"},
		}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{
		RequestedModel: codexSparkModel,
		RequiredModels: []string{codexFallbackModel},
	})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "plus" {
		t.Fatalf("account = %q, want plus with shared capacity", choice.AccountKey)
	}
}

func TestCodexRouteChoiceReturnsTypedCachedLimit(t *testing.T) {
	now := time.Unix(10_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	ledger.Observe(CapacityFact{AccountKey: "zero", Bucket: CapacityBucketBase, Source: CapacitySourceHardLimit, ConnectionGeneration: 1, Sequence: 1, RemainingPct: 0, ObservedAt: now, ResetAt: now.Add(time.Hour), Confidence: CapacityConfidenceAuthoritative})
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{{AccountKey: "zero", AccessToken: "token"}}
	}, nil, ledger)
	_, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	var limit *CachedUsageLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("error = %v, want CachedUsageLimitError", err)
	}
	if limit.ResetAt != now.Add(time.Hour) {
		t.Fatalf("reset = %v, want %v", limit.ResetAt, now.Add(time.Hour))
	}
}

func TestCodexRouteChoiceDoesNotCacheAdvisoryLimit(t *testing.T) {
	now := time.Unix(10_500, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	if !ledger.Observe(CapacityFact{
		AccountKey: "advisory", Bucket: CapacityBucketBase,
		Source: CapacitySourceUsageCache, Sequence: 1, RemainingPct: 0,
		ObservedAt: now, Confidence: CapacityConfidenceAdvisory,
	}) {
		t.Fatal("advisory fact rejected")
	}
	selector := newCodexSelectorWithCapacity(func() []codex.CodexAccount {
		return []codex.CodexAccount{{AccountKey: "advisory", AccessToken: "token"}}
	}, nil, ledger)
	choice, err := selector.Choose(context.Background(), CodexRouteRequirements{RequestedModel: "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	if choice.AccountKey != "advisory" {
		t.Fatalf("account = %q, want advisory", choice.AccountKey)
	}
}
