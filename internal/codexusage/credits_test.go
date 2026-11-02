package codexusage

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFloorCreditResponsePreservesExactNumbers(t *testing.T) {
	input := []byte(`{"credits":{"balance":0,"has_credits":false},"rate_limit":{"allowed":false,"reset_at":9007199254740993},"opaque":1e100}`)
	want := []byte(`{"credits":{"balance":1,"has_credits":true},"rate_limit":{"allowed":false,"reset_at":9007199254740993},"opaque":1e100}`)
	assertJSON(t, FloorCreditResponse(input), want)
}

func TestFloorCreditResponsePreservesUnsupportedAndUnchangedInputs(t *testing.T) {
	for _, input := range []string{
		` { "credits": { "balance": "1", "has_credits": true } } `,
		`{"credits":{"balance":9007199254740993,"has_credits":true}}`,
		`{"credits":{"balance":"unknown","has_credits":false}}`,
		`{"credits":{"balance":"NaN","has_credits":false}}`,
		`{"credits":[]}`,
		`{"credits":`,
		`{"credits":{"balance":0}} {}`,
		`{"credits":{"balance":0}} invalid`,
		`[]`,
		`null`,
		` `,
	} {
		t.Run(input, func(t *testing.T) {
			data := []byte(input)
			if got := FloorCreditResponse(data); !bytes.Equal(got, data) {
				t.Fatalf("response changed: %s", got)
			}
		})
	}
}

func TestFloorCreditMessageSingleton(t *testing.T) {
	input := usageEvent("codex-work-usage-push-v1", "message", "usage.snapshot", `{"balance":"0","has_credits":false}`)
	want := strings.Replace(input, `"balance":"0","has_credits":false`, `"balance":"1","has_credits":true`, 1)
	assertJSON(t, FloorCreditMessage([]byte(input)), []byte(want))
}

func TestFloorCreditMessageMixedBatch(t *testing.T) {
	first := usageEvent("codex-work-usage-push-v1", "message", "usage.snapshot", `{"balance":0,"has_credits":false}`)
	second := usageEvent("codex-work-usage-push-v1", "message", "usage.snapshot", `{"balance":"0.5","has_credits":false}`)
	other := usageEvent("another-topic", "message", "usage.snapshot", `{"balance":"0","has_credits":false}`)
	input := `[null,"keep",12.00,` + other + `,` + first + `,{"type":"reply","payload":{"credits":{"balance":"0"}}},` + second + `]`
	wantFirst := strings.Replace(first, `"balance":0,"has_credits":false`, `"balance":1,"has_credits":true`, 1)
	wantSecond := strings.Replace(second, `"balance":"0.5","has_credits":false`, `"balance":"1","has_credits":true`, 1)
	want := `[null,"keep",12.00,` + other + `,` + wantFirst + `,{"type":"reply","payload":{"credits":{"balance":"0"}}},` + wantSecond + `]`
	assertJSON(t, FloorCreditMessage([]byte(input)), []byte(want))
}

func TestFloorCreditMessageSubscriptionCatchups(t *testing.T) {
	usage := usageEvent("codex-work-usage-push-v1", "message", "usage.snapshot", `{"balance":"0","has_credits":false}`)
	other := usageEvent("other-topic", "message", "usage.snapshot", `{"balance":"0","has_credits":false}`)
	input := `{"id":1,"reply":{"type":"subscribe","topic_id":"codex-work-usage-push-v1","recovered":true,"catchups":[` + other + `,` + usage + `]}}`
	want := strings.Replace(input, usage, strings.Replace(usage, `"balance":"0","has_credits":false`, `"balance":"1","has_credits":true`, 1), 1)
	assertJSON(t, FloorCreditMessage([]byte(input)), []byte(want))
	assertJSON(t, FloorCreditMessage([]byte("["+input+"]")), []byte("["+want+"]"))
	for _, unchanged := range []string{
		strings.Replace(input, `"type":"subscribe"`, `"type":"other"`, 1),
		strings.Replace(input, `"topic_id":"codex-work-usage-push-v1"`, `"topic_id":"other-topic"`, 1),
		strings.Replace(input, `"recovered":true`, `"recovered":false`, 1),
		strings.Replace(input, usage, other, 1),
	} {
		if got := FloorCreditMessage([]byte(unchanged)); string(got) != unchanged {
			t.Fatal("unrelated subscription changed")
		}
	}
}

func TestFloorCreditMessagePreservesUnsupportedAndUnchangedInputs(t *testing.T) {
	valid := usageEvent("codex-work-usage-push-v1", "message", "usage.snapshot", `{"balance":"0","has_credits":false}`)
	for _, input := range []string{
		" \n" + usageEvent("codex-work-usage-push-v1", "message", "usage.snapshot", `{"balance":"1","has_credits":true}`) + "\t",
		usageEvent("another-topic", "message", "usage.snapshot", `{"balance":"0","has_credits":false}`),
		usageEvent("codex-work-usage-push-v1", "reply", "usage.snapshot", `{"balance":"0","has_credits":false}`),
		usageEvent("codex-work-usage-push-v1", "message", "another-event", `{"balance":"0","has_credits":false}`),
		usageEvent("codex-work-usage-push-v1", "message", "usage.snapshot", `{"balance":"invalid","has_credits":false}`),
		` { "type": "message", "topic_id": "codex-work-usage-push-v1", "payload": [] } `,
		`{"type":"message","topic_id":"codex-work-usage-push-v1","payload":{"type":"usage.snapshot","payload":{"usage_by_client":{"browser":{"credits":{"balance":"0"}}}}}}`,
		` [ null, false, 9007199254740993, { "type": "reply" } ] `,
		`{"credits":`,
		`[` + valid + `,`,
		valid + ` {}`,
		`[` + valid + `] invalid`,
		`"text"`,
		`[]`,
		`null`,
		` `,
	} {
		t.Run(input, func(t *testing.T) {
			data := []byte(input)
			if got := FloorCreditMessage(data); !bytes.Equal(got, data) {
				t.Fatalf("message changed: %s", got)
			}
		})
	}
}

func usageEvent(topic, kind, event, credits string) string {
	return `{"type":"` + kind + `","topic_id":"` + topic + `","offset":"cursor","opaque":1e100,"payload":{"type":"` + event + `","payload":{"sequence":9007199254740993,"usage_by_client":{"electron":{"credits":` + credits + `,"rate_limit":{"allowed":false,"limit_reached":true}},"browser":{"credits":{"balance":"0","has_credits":false}}}}}}`
}

func assertJSON(t *testing.T, got, want []byte) {
	t.Helper()
	parse := func(data []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(parse(got), parse(want)) {
		t.Fatalf("JSON changed incorrectly:\ngot  %s\nwant %s", got, want)
	}
}
