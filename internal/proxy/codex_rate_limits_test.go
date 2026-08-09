package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestParseCodexRateLimitHeadersProducesExactBucketObservations(t *testing.T) {
	headers := make(http.Header)
	headers.Set("X-Codex-Primary-Used-Percent", "12.5")
	headers.Set("X-Codex-Primary-Window-Minutes", "300")
	headers.Set("X-Codex-Primary-Reset-At", "1704069000")
	headers.Set("X-Codex-Secondary-Used-Percent", "70.4")
	headers.Set("X-Codex-Secondary-Window-Minutes", "10080")
	headers.Set("X-Codex-Secondary-Reset-At", "1704068000")
	headers.Set("X-Codex-Bengalfox-Primary-Used-Percent", "25.4")
	headers.Set("X-Codex-Bengalfox-Primary-Window-Minutes", "60")
	headers.Set("X-Codex-Bengalfox-Primary-Reset-At", "1704070000")
	headers.Set("X-Codex-Bengalfox-Limit-Name", "GPT-5.3-Codex-Spark")

	observations, err := ParseCodexRateLimitHeaders(headers)
	if err != nil {
		t.Fatal(err)
	}
	want := []CodexRateLimitObservation{
		{
			Bucket:       CapacityBucketBase,
			RemainingPct: 30,
			ResetAt:      time.Unix(1704068000, 0),
		},
		{
			Bucket:       CapacityBucket("model:gpt-5.3-codex-spark"),
			RemainingPct: 75,
			ResetAt:      time.Unix(1704070000, 0),
		},
	}
	assertCodexRateLimitObservations(t, observations, want)
}

func TestParseCodexRateLimitHeadersRejectsInvalidAuthority(t *testing.T) {
	tests := map[string]http.Header{
		"duplicate value": {
			"X-Codex-Primary-Used-Percent": {"10", "10"},
		},
		"conflicting case aliases": {
			"X-Codex-Primary-Used-Percent": {"10"},
			"x-codex-primary-used-percent": {"20"},
		},
		"non finite percentage": {
			"X-Codex-Primary-Used-Percent": {"NaN"},
		},
		"negative percentage": {
			"X-Codex-Primary-Used-Percent": {"-0.1"},
		},
		"percentage above one hundred": {
			"X-Codex-Primary-Used-Percent": {"100.1"},
		},
		"reset without percentage": {
			"X-Codex-Primary-Reset-At": {"1704069000"},
		},
		"window without percentage": {
			"X-Codex-Primary-Window-Minutes": {"300"},
		},
		"invalid reset": {
			"X-Codex-Primary-Used-Percent": {"10"},
			"X-Codex-Primary-Reset-At":     {"tomorrow"},
		},
		"non positive reset": {
			"X-Codex-Primary-Used-Percent": {"10"},
			"X-Codex-Primary-Reset-At":     {"0"},
		},
		"invalid window": {
			"X-Codex-Primary-Used-Percent":   {"10"},
			"X-Codex-Primary-Window-Minutes": {"soon"},
		},
		"non positive window": {
			"X-Codex-Primary-Used-Percent":   {"10"},
			"X-Codex-Primary-Window-Minutes": {"0"},
		},
		"two families target one bucket": {
			"X-Codex-Alpha-Primary-Used-Percent": {"10"},
			"X-Codex-Alpha-Limit-Name":           {codexSparkModel},
			"X-Codex-Beta-Primary-Used-Percent":  {"20"},
			"X-Codex-Beta-Limit-Name":            {codexSparkModel},
		},
	}
	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			observations, err := ParseCodexRateLimitHeaders(headers)
			if !errors.Is(err, ErrCodexRateLimitInvalid) {
				t.Fatalf("error = %v, want ErrCodexRateLimitInvalid", err)
			}
			if len(observations) != 0 {
				t.Fatalf("observations = %+v, want none", observations)
			}
		})
	}
}

func TestParseCodexRateLimitHeadersDoesNotGuessUnknownScope(t *testing.T) {
	tests := map[string]http.Header{
		"opaque family without name": {
			"X-Codex-Bengalfox-Primary-Used-Percent": {"20"},
		},
		"unknown provider limit name": {
			"X-Codex-Bengalfox-Primary-Used-Percent": {"20"},
			"X-Codex-Bengalfox-Limit-Name":           {"gpt-5.2-codex-sonic"},
		},
	}
	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			observations, err := ParseCodexRateLimitHeaders(headers)
			if err != nil {
				t.Fatal(err)
			}
			if len(observations) != 0 {
				t.Fatalf("observations = %+v, want none", observations)
			}
		})
	}
}

func TestParseCodexRateLimitHeadersNormalisesOfficialLimitNames(t *testing.T) {
	headers := http.Header{
		"X-Codex-Bengalfox-Primary-Used-Percent": {"25.4"},
		"X-Codex-Bengalfox-Limit-Name":           {" GPT_5.3_CODEX_SPARK "},
	}

	observations, err := ParseCodexRateLimitHeaders(headers)
	if err != nil {
		t.Fatal(err)
	}
	want := []CodexRateLimitObservation{{
		Bucket:       CapacityBucket("model:gpt-5.3-codex-spark"),
		RemainingPct: 75,
	}}
	assertCodexRateLimitObservations(t, observations, want)
}

func TestParseCodexRateLimitHeadersAreBoundedAndPrivacySafe(t *testing.T) {
	marker := "secret-looking-family-marker"
	headers := http.Header{
		"X-" + marker + "-Limit-Name": {strings.Repeat("x", 64<<10)},
	}

	observations, err := ParseCodexRateLimitHeaders(headers)
	if !errors.Is(err, ErrCodexRateLimitInvalid) {
		t.Fatalf("error = %v, want ErrCodexRateLimitInvalid", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error leaked provider-controlled header identity: %v", err)
	}
	if len(observations) != 0 {
		t.Fatalf("observations = %+v, want none", observations)
	}
}

func TestParseCodexRateLimitEventProducesExactBucketObservations(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    CodexRateLimitObservation
	}{
		{
			name: "default base scope",
			payload: `{
				"type":"codex.rate_limits",
				"rate_limits":{
					"primary":{"used_percent":12.5,"window_minutes":300,"reset_at":1704069000},
					"secondary":{"used_percent":70.4,"window_minutes":10080,"reset_at":1704068000}
				}
			}`,
			want: CodexRateLimitObservation{
				Bucket:       CapacityBucketBase,
				RemainingPct: 30,
				ResetAt:      time.Unix(1704068000, 0),
			},
		},
		{
			name: "metered exact model scope",
			payload: `{
				"type":"codex.rate_limits",
				"metered_limit_name":"GPT-5.3-Codex-Spark",
				"rate_limits":{"primary":{"used_percent":25.4,"window_minutes":60,"reset_at":1704070000}}
			}`,
			want: CodexRateLimitObservation{
				Bucket:       CapacityBucket("model:gpt-5.3-codex-spark"),
				RemainingPct: 75,
				ResetAt:      time.Unix(1704070000, 0),
			},
		},
		{
			name: "legacy exact model scope",
			payload: `{
				"type":"codex.rate_limits",
				"limit_name":"gpt-5.3-codex-spark",
				"rate_limits":{"primary":{"used_percent":100}}
			}`,
			want: CodexRateLimitObservation{
				Bucket:       CapacityBucket("model:gpt-5.3-codex-spark"),
				RemainingPct: 0,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observations, err := ParseCodexRateLimitEvent([]byte(test.payload))
			if err != nil {
				t.Fatal(err)
			}
			assertCodexRateLimitObservations(t, observations, []CodexRateLimitObservation{test.want})
		})
	}
}

func TestParseCodexRateLimitWindowsUseLimitingReset(t *testing.T) {
	secondaryReset := time.Unix(1_704_067_600, 0)
	tests := []struct {
		name  string
		parse func() ([]CodexRateLimitObservation, error)
		want  time.Time
	}{
		{
			name: "header secondary is limiting",
			parse: func() ([]CodexRateLimitObservation, error) {
				return ParseCodexRateLimitHeaders(http.Header{
					"X-Codex-Primary-Used-Percent":   {"10"},
					"X-Codex-Primary-Reset-At":       {"1704067060"},
					"X-Codex-Secondary-Used-Percent": {"100"},
					"X-Codex-Secondary-Reset-At":     {"1704067600"},
				})
			},
			want: secondaryReset,
		},
		{
			name: "event equal minima both constrain",
			parse: func() ([]CodexRateLimitObservation, error) {
				return ParseCodexRateLimitEvent([]byte(`{
					"type":"codex.rate_limits",
					"rate_limits":{
						"primary":{"used_percent":100,"reset_at":1704067060},
						"secondary":{"used_percent":100,"reset_at":1704067600}
					}
				}`))
			},
			want: secondaryReset,
		},
		{
			name: "missing limiting reset uses horizon",
			parse: func() ([]CodexRateLimitObservation, error) {
				return ParseCodexRateLimitHeaders(http.Header{
					"X-Codex-Primary-Used-Percent":   {"10"},
					"X-Codex-Primary-Reset-At":       {"1704067060"},
					"X-Codex-Secondary-Used-Percent": {"100"},
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observations, err := test.parse()
			if err != nil {
				t.Fatal(err)
			}
			if len(observations) != 1 || observations[0].RemainingPct != 0 || !observations[0].ResetAt.Equal(test.want) {
				t.Fatalf("observations = %+v, want zero remaining reset %v", observations, test.want)
			}
		})
	}
}

func TestCodexRateLimitLimitingResetKeepsZeroCapacityFenced(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	producer := newCodexRateLimitProducer(ledger, ledger.NewObservationStream(), "account-a", func() time.Time { return now }, true)
	if err := producer.ObserveHeaders(http.Header{
		"X-Codex-Primary-Used-Percent":   {"10"},
		"X-Codex-Primary-Reset-At":       {"1704067060"},
		"X-Codex-Secondary-Used-Percent": {"100"},
		"X-Codex-Secondary-Reset-At":     {"1704067600"},
	}); err != nil {
		t.Fatal(err)
	}
	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityZero {
		t.Fatalf("initial capacity = %+v, want zero", view)
	}
	now = now.Add(2 * time.Minute)
	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityZero {
		t.Fatalf("capacity after non-limiting reset = %+v, want zero", view)
	}
	now = time.Unix(1_704_067_600, 0)
	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityUnknown {
		t.Fatalf("capacity at limiting reset = %+v, want unknown", view)
	}
}

func TestParseCodexRateLimitEventRejectsInvalidAuthority(t *testing.T) {
	tests := map[string]string{
		"duplicate official field": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"used_percent":10,"used_percent":20}}
		}`,
		"case folded duplicate official field": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"used_percent":10,"USED_PERCENT":20}}
		}`,
		"unicode folded duplicate official field": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"used_percent":10,"uſed_percent":20}}
		}`,
		"conflicting scope aliases": `{
			"type":"codex.rate_limits",
			"metered_limit_name":"codex",
			"limit_name":"gpt-5.3-codex-spark",
			"rate_limits":{"primary":{"used_percent":10}}
		}`,
		"non finite percentage": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"used_percent":1e400}}
		}`,
		"negative percentage": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"used_percent":-0.1}}
		}`,
		"percentage above one hundred": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"used_percent":100.1}}
		}`,
		"reset without percentage": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"reset_at":1704069000}}
		}`,
		"window without percentage": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"window_minutes":300}}
		}`,
		"invalid reset": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"used_percent":10,"reset_at":0}}
		}`,
		"invalid window": `{
			"type":"codex.rate_limits",
			"rate_limits":{"primary":{"used_percent":10,"window_minutes":0}}
		}`,
		"wrong rate limits shape": `{
			"type":"codex.rate_limits",
			"rate_limits":[]
		}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			observations, err := ParseCodexRateLimitEvent([]byte(payload))
			if !errors.Is(err, ErrCodexRateLimitInvalid) {
				t.Fatalf("error = %v, want ErrCodexRateLimitInvalid", err)
			}
			if len(observations) != 0 {
				t.Fatalf("observations = %+v, want none", observations)
			}
		})
	}
}

func TestParseCodexRateLimitEventNormalisesOfficialLimitName(t *testing.T) {
	payload := []byte(`{
		"type":"codex.rate_limits",
		"metered_limit_name":" GPT_5.3_CODEX_SPARK ",
		"rate_limits":{"primary":{"used_percent":25.4}}
	}`)

	observations, err := ParseCodexRateLimitEvent(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := []CodexRateLimitObservation{{
		Bucket:       CapacityBucket("model:gpt-5.3-codex-spark"),
		RemainingPct: 75,
	}}
	assertCodexRateLimitObservations(t, observations, want)
}

func TestParseCodexRateLimitEventIsBounded(t *testing.T) {
	payload := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":` +
		strings.Repeat(" ", codexSSEDefaultMaxEventBytes) + `10}}}`)

	observations, err := ParseCodexRateLimitEvent(payload)
	if !errors.Is(err, ErrCodexRateLimitInvalid) {
		t.Fatalf("error = %v, want ErrCodexRateLimitInvalid", err)
	}
	if len(observations) != 0 {
		t.Fatalf("observations = %+v, want none", observations)
	}
}

func TestParseCodexRateLimitEventFieldCountIsBounded(t *testing.T) {
	var payload strings.Builder
	payload.WriteString(`{"type":"unrelated"`)
	for index := range 129 {
		fmt.Fprintf(&payload, `,"field_%d":%d`, index, index)
	}
	payload.WriteByte('}')

	observations, err := ParseCodexRateLimitEvent([]byte(payload.String()))
	if !errors.Is(err, ErrCodexRateLimitInvalid) {
		t.Fatalf("error = %v, want ErrCodexRateLimitInvalid", err)
	}
	if len(observations) != 0 {
		t.Fatalf("observations = %+v, want none", observations)
	}
}

func TestParseCodexRateLimitEventDoesNotGuessUnknownScope(t *testing.T) {
	for name, payload := range map[string]string{
		"unknown exact model": `{
			"type":"codex.rate_limits",
			"metered_limit_name":"gpt-5.2-codex-sonic",
			"rate_limits":{"primary":{"used_percent":20}}
		}`,
		"opaque metered id": `{
			"type":"codex.rate_limits",
			"metered_limit_name":"codex_bengalfox",
			"rate_limits":{"primary":{"used_percent":20}}
		}`,
		"unrelated event": `{
			"type":"response.completed",
			"rate_limits":{"primary":{"used_percent":20}}
		}`,
	} {
		t.Run(name, func(t *testing.T) {
			observations, err := ParseCodexRateLimitEvent([]byte(payload))
			if err != nil {
				t.Fatal(err)
			}
			if len(observations) != 0 {
				t.Fatalf("observations = %+v, want none", observations)
			}
		})
	}
}

func TestCodexRateLimitObservationProducesNeutralCapacityFact(t *testing.T) {
	observedAt := time.Unix(1704067000, 0)
	resetAt := time.Unix(1704069000, 0)
	observation := CodexRateLimitObservation{
		Bucket:       CapacityBucket("model:gpt-5.3-codex-spark"),
		RemainingPct: 42,
		ResetAt:      resetAt,
	}
	fact := observation.CapacityFact(
		"account-a",
		CapacitySourceLiveRateLimits,
		7,
		3,
		observedAt,
	)
	want := CapacityFact{
		AccountKey:           "account-a",
		Bucket:               CapacityBucket("model:gpt-5.3-codex-spark"),
		RemainingPct:         42,
		Source:               CapacitySourceLiveRateLimits,
		Sequence:             7,
		ConnectionGeneration: 3,
		ObservedAt:           observedAt,
		ResetAt:              resetAt,
		Confidence:           CapacityConfidenceAuthoritative,
	}
	if fact != want {
		t.Fatalf("fact = %+v, want %+v", fact, want)
	}
}

func TestCodexRateLimitProducerOrdersHeadersAndEventsOnOneResponse(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	sink := &recordingCodexCapacitySink{ledger: ledger}
	producer := newCodexRateLimitProducer(sink, ledger.NewObservationStream(), "account-a", func() time.Time { return now }, true)
	header := http.Header{
		"X-Codex-Primary-Used-Percent":           {"12.5"},
		"X-Codex-Primary-Reset-At":               {"1704069000"},
		"X-Codex-Bengalfox-Primary-Used-Percent": {"25.4"},
		"X-Codex-Bengalfox-Primary-Reset-At":     {"1704070000"},
		"X-Codex-Bengalfox-Limit-Name":           {codexSparkModel},
	}
	if err := producer.ObserveHeaders(header); err != nil {
		t.Fatal(err)
	}
	if err := producer.ObserveEvent([]byte(`{
		"type":"codex.rate_limits",
		"rate_limits":{"primary":{"used_percent":40,"reset_at":1704071000}}
	}`)); err != nil {
		t.Fatal(err)
	}

	if len(sink.facts) != 3 {
		t.Fatalf("facts = %+v, want three", sink.facts)
	}
	wantBuckets := []CapacityBucket{CapacityBucketBase, CapacityBucketForModel(codexSparkModel), CapacityBucketBase}
	wantSources := []CapacitySource{CapacitySourceHTTPHeaders, CapacitySourceHTTPHeaders, CapacitySourceLiveRateLimits}
	for index, fact := range sink.facts {
		if fact.AccountKey != "account-a" || fact.Bucket != wantBuckets[index] || fact.Source != wantSources[index] || fact.Confidence != CapacityConfidenceAuthoritative || !fact.ObservedAt.Equal(now) {
			t.Fatalf("fact[%d] = %+v", index, fact)
		}
		if fact.ConnectionGeneration == 0 || fact.ConnectionGeneration != sink.facts[0].ConnectionGeneration || fact.Sequence != uint64(index+1) {
			t.Fatalf("fact[%d] cursor = (%d,%d), want (%d,%d)", index, fact.ConnectionGeneration, fact.Sequence, sink.facts[0].ConnectionGeneration, index+1)
		}
	}
	if sink.facts[0].RemainingPct != 88 || !sink.facts[0].ResetAt.Equal(time.Unix(1_704_069_000, 0)) {
		t.Fatalf("base header fact = %+v", sink.facts[0])
	}
	if sink.facts[1].RemainingPct != 75 {
		t.Fatalf("model header fact = %+v", sink.facts[1])
	}
	if sink.facts[2].RemainingPct != 60 || !sink.facts[2].ResetAt.Equal(time.Unix(1_704_071_000, 0)) {
		t.Fatalf("live fact = %+v", sink.facts[2])
	}
}

func TestCodexRateLimitProducerAllocatesOneGenerationPerResponse(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	sink := &recordingCodexCapacitySink{ledger: ledger}
	first := newCodexRateLimitProducer(sink, ledger.NewObservationStream(), "account-a", func() time.Time { return now }, true)
	second := newCodexRateLimitProducer(sink, ledger.NewObservationStream(), "account-a", func() time.Time { return now }, true)
	payload := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":20}}}`)
	if err := first.ObserveEvent(payload); err != nil {
		t.Fatal(err)
	}
	if err := second.ObserveEvent(payload); err != nil {
		t.Fatal(err)
	}
	if len(sink.facts) != 2 || sink.facts[0].ConnectionGeneration == 0 || sink.facts[1].ConnectionGeneration <= sink.facts[0].ConnectionGeneration || sink.facts[0].Sequence != 1 || sink.facts[1].Sequence != 1 {
		t.Fatalf("facts = %+v, want separate increasing generations", sink.facts)
	}
}

func TestCodexUnknownMeteredLimitDoesNotGuessBucket(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	sink := &recordingCodexCapacitySink{ledger: ledger}
	producer := newCodexRateLimitProducer(sink, ledger.NewObservationStream(), "account-a", func() time.Time { return now }, true)
	if err := producer.ObserveEvent([]byte(`{
		"type":"codex.rate_limits",
		"metered_limit_name":"opaque-provider-scope",
		"rate_limits":{"primary":{"used_percent":20}}
	}`)); err != nil {
		t.Fatal(err)
	}
	if len(sink.facts) != 0 {
		t.Fatalf("facts = %+v, want none", sink.facts)
	}
}

func TestCodexRateLimitProducerRejectsDecodedBodyAuthority(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	sink := &recordingCodexCapacitySink{ledger: ledger}
	producer := newCodexRateLimitProducer(sink, ledger.NewObservationStream(), "account-a", func() time.Time { return now }, false)
	if err := producer.ObserveEvent([]byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":20}}}`)); err != nil {
		t.Fatal(err)
	}
	if len(sink.facts) != 0 {
		t.Fatalf("facts = %+v, want none", sink.facts)
	}
}

func TestCodexRateLimitProducerHardFenceUsesSameResponseStream(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	sink := &recordingCodexCapacitySink{ledger: ledger}
	producer := newCodexRateLimitProducer(sink, ledger.NewObservationStream(), "account-a", func() time.Time { return now }, true)
	if err := producer.ObserveHeaders(http.Header{"X-Codex-Primary-Used-Percent": {"10"}}); err != nil {
		t.Fatal(err)
	}
	producer.ObserveHardLimit(CapacityBucketForModel("gpt-5.4"), now.Add(time.Minute))
	if len(sink.facts) != 2 || sink.facts[0].ConnectionGeneration != sink.facts[1].ConnectionGeneration || sink.facts[0].Sequence != 1 || sink.facts[1].Sequence != 2 {
		t.Fatalf("facts = %+v, want ordered header/hard facts", sink.facts)
	}
	if fact := sink.facts[1]; fact.Source != CapacitySourceHardLimit || fact.RemainingPct != 0 || fact.Bucket != CapacityBucketBase || !fact.ResetAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("hard fact = %+v", fact)
	}
}

func TestCodexObserverProducesCapacityFactsWithoutChangingTurnOrBytes(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	manager := NewCodexTurnLeaseManager(1, false, nil)
	observer := newCodexTurnObserverWithKey(manager, nil, []byte("01234567890123456789012345678901"))
	observer.BindCapacity(ledger)
	request := []byte(`{"type":"response.create","model":"gpt-5.4","client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","request_kind":"turn"}}}`)
	handle := observer.BeginHTTP(context.Background(), request, "identity", "", false)
	handle.Selected(RouteChoice{AccountKey: "account-a", RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4"}, false)
	wireBody := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"response-a\"}}\n\n" +
		"data: {\"type\":\"codex.rate_limits\",\"rate_limits\":{\"primary\":{\"used_percent\":20,\"reset_at\":1704071000}}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-a\"}}\n\n")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":                 {"text/event-stream"},
			"X-Codex-Primary-Used-Percent": {"10"},
			"X-Codex-Primary-Reset-At":     {"1704069000"},
		},
		Body: io.NopCloser(bytes.NewReader(wireBody)),
	}
	handle.Response(response)
	observeCodexResponseBody(response, handle)
	gotBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, wireBody) {
		t.Fatalf("relayed body changed: got %q, want %q", gotBody, wireBody)
	}
	if got := response.Header.Get("X-Codex-Primary-Used-Percent"); got != "10" {
		t.Fatalf("response header changed to %q", got)
	}

	ledger.mu.RLock()
	headerFact, haveHeader := ledger.facts[capacityFactKey{account: "account-a", bucket: CapacityBucketBase, source: CapacitySourceHTTPHeaders}]
	liveFact, haveLive := ledger.facts[capacityFactKey{account: "account-a", bucket: CapacityBucketBase, source: CapacitySourceLiveRateLimits}]
	ledger.mu.RUnlock()
	if !haveHeader || !haveLive {
		t.Fatalf("header/live facts = %v/%v", haveHeader, haveLive)
	}
	if headerFact.ConnectionGeneration == 0 || headerFact.ConnectionGeneration != liveFact.ConnectionGeneration || headerFact.Sequence != 1 || liveFact.Sequence != 2 {
		t.Fatalf("header/live cursors = (%d,%d)/(%d,%d)", headerFact.ConnectionGeneration, headerFact.Sequence, liveFact.ConnectionGeneration, liveFact.Sequence)
	}
	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityPositive || view.RemainingPct != 80 || view.Source != CapacitySourceLiveRateLimits {
		t.Fatalf("capacity = %+v, want live 80%%", view)
	}
	leases := manager.Snapshot()
	if len(leases) != 1 || leases[0].AccountKey != "account-a" {
		t.Fatalf("leases = %+v, want admitted account-a", leases)
	}
	if health := observer.Health(); health.QuotaEvents != 2 {
		t.Fatalf("quota events = %d, want one header response and one live event", health.QuotaEvents)
	}
}

func TestCodexObserverRejectsAmbiguousResponseBodyAuthority(t *testing.T) {
	for _, test := range []struct {
		name         string
		header       http.Header
		uncompressed bool
	}{
		{name: "transparent decompression", uncompressed: true},
		{name: "encoded", header: http.Header{"Content-Encoding": {"gzip"}}},
		{name: "duplicate identity", header: http.Header{"Content-Encoding": {"identity", "identity"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(1_704_067_000, 0)
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
			observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(1, false, nil), nil, []byte("01234567890123456789012345678901"))
			observer.BindCapacity(ledger)
			handle := observer.BeginHTTP(context.Background(), []byte(`{"type":"response.create"}`), "identity", "", false)
			handle.Selected(RouteChoice{AccountKey: "account-a"}, false)
			handle.Response(&http.Response{StatusCode: http.StatusOK, Header: test.header, Uncompressed: test.uncompressed})
			handle.ObserveBytes([]byte("data: {\"type\":\"codex.rate_limits\",\"rate_limits\":{\"primary\":{\"used_percent\":20}}}\n\n"))
			handle.Finish(nil)
			ledger.mu.RLock()
			_, haveLive := ledger.facts[capacityFactKey{account: "account-a", bucket: CapacityBucketBase, source: CapacitySourceLiveRateLimits}]
			ledger.mu.RUnlock()
			if haveLive {
				t.Fatal("ambiguous or decoded response body produced authoritative live capacity")
			}
			if health := observer.Health(); health.QuotaEvents != 1 {
				t.Fatalf("quota events = %d, want event telemetry retained", health.QuotaEvents)
			}
		})
	}
}

func TestCodexObserverUnknownRateLimitCountsWithoutFact(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(1, false, nil), nil, []byte("01234567890123456789012345678901"))
	observer.BindCapacity(ledger)
	handle := observer.BeginHTTP(context.Background(), []byte(`{"type":"response.create"}`), "identity", "", false)
	handle.Selected(RouteChoice{AccountKey: "account-a"}, false)
	handle.Response(&http.Response{StatusCode: http.StatusOK})
	handle.ObserveBytes([]byte("data: {\"type\":\"codex.rate_limits\",\"metered_limit_name\":\"opaque-provider-scope\",\"rate_limits\":{\"primary\":{\"used_percent\":20}}}\n\n"))
	handle.Finish(nil)
	if view := ledger.Capacity("account-a", CapacityBucketBase); view.State != CapacityUnknown {
		t.Fatalf("capacity = %+v, want unknown", view)
	}
	if health := observer.Health(); health.QuotaEvents != 1 {
		t.Fatalf("quota events = %d, want one", health.QuotaEvents)
	}
}

func TestCodexWebSocketSessionUsesOneCapacityGeneration(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	sink := &recordingCodexCapacitySink{ledger: ledger}
	producer := newCodexRateLimitProducer(sink, ledger.NewObservationStream(), "account-a", func() time.Time { return now }, true)
	observer := newCodexTurnObserverWithKey(NewCodexTurnLeaseManager(1, false, nil), nil, []byte("01234567890123456789012345678901"))
	session := newCodexWSObservationSession(observer, context.Background(), RouteChoice{AccountKey: "account-a"}, producer)
	for index, used := range []int{20, 30} {
		session.ObserveClient([]byte(`{"type":"response.create"}`))
		session.ObserveUpstream([]byte(fmt.Sprintf(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":%d}}}`, used)))
		if len(sink.facts) != index+1 {
			t.Fatalf("facts after event %d = %+v", index, sink.facts)
		}
	}
	session.Close(nil)
	if sink.facts[0].ConnectionGeneration == 0 || sink.facts[0].ConnectionGeneration != sink.facts[1].ConnectionGeneration || sink.facts[0].Sequence != 1 || sink.facts[1].Sequence != 2 {
		t.Fatalf("facts = %+v, want one connection generation", sink.facts)
	}
	if health := observer.Health(); health.QuotaEvents != 2 {
		t.Fatalf("quota events = %d, want two", health.QuotaEvents)
	}
}

func TestCodexWebSocketCapacitySessionDoesNotRequireTurnObserver(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	sink := &recordingCodexCapacitySink{ledger: ledger}
	producer := newCodexRateLimitProducer(sink, ledger.NewObservationStream(), "account-a", func() time.Time { return now }, true)
	session := newCodexWSObservationSession(nil, context.Background(), RouteChoice{AccountKey: "account-a"}, producer)
	if session == nil {
		t.Fatal("capacity-only WebSocket session is nil")
	}
	session.ObserveUpstream([]byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":20}}}`))
	session.Close(nil)
	if len(sink.facts) != 1 || sink.facts[0].Source != CapacitySourceLiveRateLimits {
		t.Fatalf("facts = %+v, want one live fact", sink.facts)
	}
}

func TestCodexHTTPEnforcerBindsRouterCapacity(t *testing.T) {
	ledger := NewCodexCapacityLedger(time.Now, time.Hour)
	enforcer, err := NewCodexHTTPEnforcer(
		&CodexRequestRouter{Capacity: ledger},
		1,
		openTestCodexLeaseStore(t, fsutil.NewMemFS()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if enforcer.Observer.capacity.Load() != ledger {
		t.Fatal("enforcer observer is not bound to router selector ledger")
	}
}

func TestCodexRequestRouterObservesRejectedResponseHeadersAndHardLimitInOrder(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	router := &CodexRequestRouter{Capacity: ledger, Now: func() time.Time { return now }}
	choice := RouteChoice{AccountKey: "account-a", RequestedModel: "gpt-5.3-codex-spark", EffectiveModel: "gpt-5.3-codex-spark"}
	body := []byte(codexLiveUsageLimitBody)
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"X-Codex-Primary-Used-Percent": {"10"},
			"Retry-After":                  {"60"},
		},
		Body: io.NopCloser(bytes.NewReader(body)),
	}
	failure, err := router.classifyAttemptResponse(choice, response)
	if err != nil {
		t.Fatal(err)
	}
	if failure != CodexPinnedHardLimit {
		t.Fatalf("failure = %v, want hard limit", failure)
	}
	gotBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("classified response body changed: got %q, want %q", gotBody, body)
	}
	ledger.mu.RLock()
	headerFact, haveHeader := ledger.facts[capacityFactKey{account: "account-a", bucket: CapacityBucketBase, source: CapacitySourceHTTPHeaders}]
	hardBucket := CapacityBucketForModel(choice.EffectiveModel)
	hardFact, haveHard := ledger.facts[capacityFactKey{account: "account-a", bucket: hardBucket, source: CapacitySourceHardLimit}]
	ledger.mu.RUnlock()
	if !haveHeader || !haveHard {
		t.Fatalf("header/hard facts = %v/%v", haveHeader, haveHard)
	}
	if headerFact.ConnectionGeneration == 0 || headerFact.ConnectionGeneration != hardFact.ConnectionGeneration || headerFact.Sequence != 1 || hardFact.Sequence != 2 {
		t.Fatalf("header/hard cursors = (%d,%d)/(%d,%d)", headerFact.ConnectionGeneration, headerFact.Sequence, hardFact.ConnectionGeneration, hardFact.Sequence)
	}
	if hardFact.AccountKey != "account-a" || hardFact.Bucket != CapacityBucketForModel(codexSparkModel) || !hardFact.ResetAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("hard fact = %+v", hardFact)
	}
}

func TestCodexRequestRouterObservesHeadersOnlyOnRejectedResponses(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		wantFact bool
	}{
		{name: "rejected", status: http.StatusUnauthorized, wantFact: true},
		{name: "accepted belongs to observer", status: http.StatusOK, wantFact: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(1_704_067_000, 0)
			ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
			router := &CodexRequestRouter{Capacity: ledger, Now: func() time.Time { return now }}
			response := &http.Response{
				StatusCode: test.status,
				Header:     http.Header{"X-Codex-Primary-Used-Percent": {"10"}},
				Body:       http.NoBody,
			}
			if _, err := router.classifyAttemptResponse(RouteChoice{AccountKey: "account-a"}, response); err != nil {
				t.Fatal(err)
			}
			ledger.mu.RLock()
			_, haveFact := ledger.facts[capacityFactKey{account: "account-a", bucket: CapacityBucketBase, source: CapacitySourceHTTPHeaders}]
			ledger.mu.RUnlock()
			if haveFact != test.wantFact {
				t.Fatalf("header fact present = %v, want %v", haveFact, test.wantFact)
			}
		})
	}
}

func TestServerCodexWebSocketObservesHandshakeHeadersAndEventsOnOneConnection(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	plan := requestPlan("account-a", "candidate-a")
	executor := codexWebSocketExecutorFunc(func(_ context.Context, _ RouteChoice, _ CandidateAttempt, _ string, _ http.Header) (*websocket.Conn, *http.Response, []byte, error) {
		return new(websocket.Conn), &http.Response{
			StatusCode: http.StatusSwitchingProtocols,
			Header:     http.Header{"X-Codex-Primary-Used-Percent": {"10"}},
		}, nil, nil
	})
	server := &Server{
		CodexRequests:          &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{plan}}, Capacity: ledger, Now: func() time.Time { return now }},
		CodexWebSocketExecutor: executor,
	}
	connection, choice, _, producer, err := server.dialCodexWebSocketWithCapacity(context.Background(), "wss://upstream.test/responses", nil, "gpt-5.4")
	if err != nil || connection == nil || choice.AccountKey != "account-a" {
		t.Fatalf("connection=%p choice=%q error=%v", connection, choice.AccountKey, err)
	}
	session := newCodexWSObservationSession(nil, context.Background(), choice, producer)
	session.ObserveUpstream([]byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":20}}}`))
	session.Close(nil)
	ledger.mu.RLock()
	headerFact, haveHeader := ledger.facts[capacityFactKey{account: "account-a", bucket: CapacityBucketBase, source: CapacitySourceHTTPHeaders}]
	liveFact, haveLive := ledger.facts[capacityFactKey{account: "account-a", bucket: CapacityBucketBase, source: CapacitySourceLiveRateLimits}]
	ledger.mu.RUnlock()
	if !haveHeader || !haveLive {
		t.Fatalf("header/live facts = %v/%v", haveHeader, haveLive)
	}
	if headerFact.ConnectionGeneration == 0 || headerFact.ConnectionGeneration != liveFact.ConnectionGeneration || headerFact.Sequence != 1 || liveFact.Sequence != 2 || headerFact.RemainingPct != 90 || liveFact.RemainingPct != 80 {
		t.Fatalf("header/live facts = %+v/%+v", headerFact, liveFact)
	}
}

func TestServerCodexWebSocketObservesRejectedHeadersAndHardLimitInOrder(t *testing.T) {
	now := time.Unix(1_704_067_000, 0)
	ledger := NewCodexCapacityLedger(func() time.Time { return now }, time.Hour)
	first := requestPlan("account-a", "candidate-a")
	second := requestPlan("account-b", "candidate-b")
	first.Choice.RequestedModel = codexSparkModel
	first.Choice.EffectiveModel = codexSparkModel
	first.Choice.RequiredBuckets = []CapacityBucket{CapacityBucketForModel(codexSparkModel)}
	executor := codexWebSocketExecutorFunc(func(_ context.Context, choice RouteChoice, _ CandidateAttempt, _ string, _ http.Header) (*websocket.Conn, *http.Response, []byte, error) {
		if choice.AccountKey == "account-b" {
			return new(websocket.Conn), nil, nil, nil
		}
		return nil, &http.Response{
			Status:     "429 Too Many Requests",
			StatusCode: http.StatusTooManyRequests,
			Header: http.Header{
				"X-Codex-Primary-Used-Percent": {"10"},
				"Retry-After":                  {"60"},
			},
			Body: http.NoBody,
		}, []byte(codexLiveUsageLimitBody), errors.New("websocket: bad handshake")
	})
	server := &Server{
		CodexRequests:          &CodexRequestRouter{Scope: &queuedRequestScope{plans: []CodexRequestPlan{first, second}}, Capacity: ledger, Now: func() time.Time { return now }},
		CodexWebSocketExecutor: executor,
	}
	connection, choice, _, err := server.dialCodexWebSocket(context.Background(), "wss://upstream.test/responses", nil, codexSparkModel)
	if err != nil || connection == nil || choice.AccountKey != "account-b" {
		t.Fatalf("connection=%p choice=%q error=%v", connection, choice.AccountKey, err)
	}
	ledger.mu.RLock()
	headerFact, haveHeader := ledger.facts[capacityFactKey{account: "account-a", bucket: CapacityBucketBase, source: CapacitySourceHTTPHeaders}]
	hardBucket := CapacityBucketForModel(codexSparkModel)
	hardFact, haveHard := ledger.facts[capacityFactKey{account: "account-a", bucket: hardBucket, source: CapacitySourceHardLimit}]
	ledger.mu.RUnlock()
	if !haveHeader || !haveHard {
		t.Fatalf("header/hard facts = %v/%v", haveHeader, haveHard)
	}
	if headerFact.ConnectionGeneration == 0 || headerFact.ConnectionGeneration != hardFact.ConnectionGeneration || headerFact.Sequence != 1 || hardFact.Sequence != 2 {
		t.Fatalf("header/hard cursors = (%d,%d)/(%d,%d)", headerFact.ConnectionGeneration, headerFact.Sequence, hardFact.ConnectionGeneration, hardFact.Sequence)
	}
	if !hardFact.ResetAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("hard reset = %v, want %v", hardFact.ResetAt, now.Add(time.Minute))
	}
}

type recordingCodexCapacitySink struct {
	ledger *CodexCapacityLedger
	facts  []CapacityFact
}

func (sink *recordingCodexCapacitySink) Observe(fact CapacityFact) bool {
	if sink == nil || sink.ledger == nil || !sink.ledger.Observe(fact) {
		return false
	}
	sink.facts = append(sink.facts, fact)
	return true
}

func assertCodexRateLimitObservations(t *testing.T, got, want []CodexRateLimitObservation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("observation count = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Bucket != want[i].Bucket || got[i].RemainingPct != want[i].RemainingPct || !got[i].ResetAt.Equal(want[i].ResetAt) {
			t.Fatalf("observation[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
