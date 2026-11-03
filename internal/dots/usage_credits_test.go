package dots

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNativeUsageCreditFloor(t *testing.T) {
	for _, test := range []struct {
		name, credits, want string
	}{
		{"zero string", `{"balance":"0","has_credits":false,"unlimited":false}`, `{"balance":"1","has_credits":true,"unlimited":false}`},
		{"fraction string", `{"balance":"0.5","has_credits":false}`, `{"balance":"1","has_credits":true}`},
		{"zero number", `{"balance":0,"has_credits":false}`, `{"balance":1,"has_credits":true}`},
		{"negative", `{"balance":-2}`, `{"balance":1,"has_credits":true}`},
		{"higher balance", `{"balance":"12.75","has_credits":false}`, `{"balance":"12.75","has_credits":true}`},
		{"exact high number", `{"balance":9007199254740993,"has_credits":true}`, `{"balance":9007199254740993,"has_credits":true}`},
		{"null balance", `{"balance":null,"unlimited":true}`, `{"balance":"1","has_credits":true,"unlimited":true}`},
		{"absent balance", `{"unlimited":false}`, `{"balance":"1","has_credits":true,"unlimited":false}`},
		{"null credits", `null`, `{"balance":"1","has_credits":true}`},
		{"absent credits", ``, `{"balance":"1","has_credits":true}`},
		{"malformed balance", `{"balance":"unknown","has_credits":false}`, `{"balance":"unknown","has_credits":false}`},
		{"non-finite balance", `{"balance":"NaN","has_credits":false}`, `{"balance":"NaN","has_credits":false}`},
		{"malformed credits", `[]`, `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, _, _ := probeFixtureForTest(t)
			body := usageCreditFixture(test.credits)
			p.options.HTTP = probeDoer(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("usage query did not request identity encoding")
				}
				response := probeSuccess(body)
				response.Header.Set("Content-Length", strconv.Itoa(len(body)))
				response.Header.Set("ETag", "upstream")
				return response, nil
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, probeRequest("GET", "/backend-api/wham/usage", ""))
			if w.Code != 200 {
				t.Fatalf("status = %d", w.Code)
			}
			if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
				t.Error("response length does not match projected body")
			}
			if w.Body.String() != body && w.Header().Get("ETag") != "" {
				t.Error("projected response retained the upstream ETag")
			}
			assertUsageJSON(t, w.Body.Bytes(), []byte(usageCreditFixture(test.want)))
		})
	}
}

func TestNativeUsageCreditFloorPreservesOtherResponses(t *testing.T) {
	for _, test := range []struct {
		name, mode, method, path, body string
		status                         int
	}{
		{"isolated", "isolated", "GET", "/backend-api/wham/usage", usageCreditFixture(`{"balance":"0","has_credits":false}`), 200},
		{"error", "native", "GET", "/backend-api/wham/usage", usageCreditFixture(`{"balance":"0","has_credits":false}`), 429},
		{"other query", "native", "GET", "/backend-api/wham/other", usageCreditFixture(`{"balance":"0","has_credits":false}`), 200},
		{"other method", "native", "POST", "/backend-api/wham/usage", usageCreditFixture(`{"balance":"0","has_credits":false}`), 200},
		{"invalid JSON", "native", "GET", "/backend-api/wham/usage", `{"credits":`, 200},
		{"non object", "native", "GET", "/backend-api/wham/usage", `[]`, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, _, _ := probeFixtureForTest(t)
			p.settings.IntegrationMode = test.mode
			p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) {
				response := probeSuccess(test.body)
				response.StatusCode = test.status
				return response, nil
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, probeRequest(test.method, test.path, ""))
			if w.Code != test.status || w.Body.String() != test.body {
				t.Fatalf("response changed: status=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
}

func TestNativeCelsiusUsageCreditFloor(t *testing.T) {
	for _, test := range []struct {
		name, mode, topic string
		batch             bool
		floor             bool
	}{
		{"native batch", "native", "codex-work-usage-push-v1", true, true},
		{"native singleton", "native", "codex-work-usage-push-v1", false, true},
		{"isolated", "isolated", "codex-work-usage-push-v1", true, false},
		{"other topic", "native", "unrelated", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := celsiusTestBroker()
			b.Settings.IntegrationMode = test.mode
			primary, worker, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
			b.Dial = func(_ context.Context, endpoint string) (CelsiusSocket, error) {
				if strings.HasSuffix(endpoint, "/primary") {
					return primary, nil
				}
				return worker, nil
			}
			celsiusRunTest(t, b, client)
			frame := `{"type":"message","topic_id":"` + test.topic + `","offset":"cursor","payload":{"type":"usage.snapshot","payload":{"version":1,"sequence":9007199254740993,"usage_by_client":{"electron":` + usageCreditFixture(`{"balance":"0","has_credits":false}`) + `,"browser":` + usageCreditFixture(`{"balance":"0","has_credits":false}`) + `}}}}`
			want := frame
			if test.floor {
				want = strings.Replace(want, `"credits":{"balance":"0","has_credits":false}`, `"credits":{"balance":"1","has_credits":true}`, 1)
			}
			if test.batch {
				frame, want = "["+frame+"]", "["+want+"]"
			}
			primary.input <- []byte(frame)
			select {
			case data := <-client.writes:
				assertUsageJSON(t, data, []byte(want))
			case <-time.After(2 * time.Second):
				t.Fatal("usage push timed out")
			}
		})
	}
}

func TestNativeCelsiusRecoveredUsageCreditFloor(t *testing.T) {
	for _, test := range []struct {
		name, mode, command, topic, replyType, eventType, eventTopic, credits, want string
		recovered                                                                   bool
	}{
		{"string balance", "native", "subscribe", "codex-work-usage-push-v1", "subscribe", "usage.snapshot", "codex-work-usage-push-v1", `{"balance":"0","has_credits":false}`, `{"balance":"1","has_credits":true}`, true},
		{"number balance", "native", "subscribe", "codex-work-usage-push-v1", "subscribe", "usage.snapshot", "codex-work-usage-push-v1", `{"balance":0.5,"has_credits":false}`, `{"balance":1,"has_credits":true}`, true},
		{"exact high number", "native", "subscribe", "codex-work-usage-push-v1", "subscribe", "usage.snapshot", "codex-work-usage-push-v1", `{"balance":9007199254740993,"has_credits":false}`, `{"balance":9007199254740993,"has_credits":true}`, true},
		{"isolated", "isolated", "subscribe", "codex-work-usage-push-v1", "subscribe", "usage.snapshot", "codex-work-usage-push-v1", `{"balance":0}`, `{"balance":0}`, true},
		{"other subscription", "native", "subscribe", "unrelated", "subscribe", "usage.snapshot", "codex-work-usage-push-v1", `{"balance":0}`, `{"balance":0}`, true},
		{"other command", "native", "future/command", "codex-work-usage-push-v1", "subscribe", "usage.snapshot", "codex-work-usage-push-v1", `{"balance":0}`, `{"balance":0}`, true},
		{"other reply", "native", "subscribe", "codex-work-usage-push-v1", "error", "usage.snapshot", "codex-work-usage-push-v1", `{"balance":0}`, `{"balance":0}`, true},
		{"not recovered", "native", "subscribe", "codex-work-usage-push-v1", "subscribe", "usage.snapshot", "codex-work-usage-push-v1", `{"balance":0}`, `{"balance":0}`, false},
		{"other event", "native", "subscribe", "codex-work-usage-push-v1", "subscribe", "future/event", "codex-work-usage-push-v1", `{"balance":0}`, `{"balance":0}`, true},
		{"other event topic", "native", "subscribe", "codex-work-usage-push-v1", "subscribe", "usage.snapshot", "unrelated", `{"balance":0}`, `{"balance":0}`, true},
		{"malformed credits", "native", "subscribe", "codex-work-usage-push-v1", "subscribe", "usage.snapshot", "codex-work-usage-push-v1", `[]`, `[]`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := celsiusTestBroker()
			b.Settings.IntegrationMode = test.mode
			primary, worker, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
			b.Dial = func(_ context.Context, endpoint string) (CelsiusSocket, error) {
				if strings.HasSuffix(endpoint, "/primary") {
					return primary, nil
				}
				return worker, nil
			}
			celsiusRunTest(t, b, client)
			client.push([]any{celsiusCommand(99, test.command, test.topic)})
			wire := celsiusReadBatch(t, primary)[0]
			id, _ := celsiusID(wire["id"])
			event := `{"type":"message","topic_id":"` + test.eventTopic + `","offset":"cursor","unknown":9007199254740993,"payload":{"type":"` + test.eventType + `","payload":{"version":1,"sequence":9007199254740993,"usage_by_client":{"electron":` + usageCreditFixture(test.credits) + `,"browser":` + usageCreditFixture(`{"balance":"0","has_credits":false}`) + `}}}}`
			reply := `{"type":"` + test.replyType + `","recovered":` + strconv.FormatBool(test.recovered) + `,"offset":"reply-cursor","future":9007199254740993,"catchups":[null,"unknown",` + event + `]}`
			primary.input <- []byte(`[{"id":` + strconv.FormatInt(id, 10) + `,"reply":` + reply + `}]`)
			want := strings.Replace(reply, `"credits":`+test.credits, `"credits":`+test.want, 1)
			select {
			case data := <-client.writes:
				assertUsageJSON(t, data, []byte(`[{"id":99,"reply":`+want+`}]`))
			case <-time.After(2 * time.Second):
				t.Fatal("recovered usage reply timed out")
			}
		})
	}
}

func usageCreditFixture(credits string) string {
	field := ""
	if credits != "" {
		field = `"credits":` + credits + `,`
	}
	return `{"account_id":"system","user_id":"system-user","plan_type":"pro","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"reset_at":9007199254740993}},"spend_control":{"reached":true},` + field + `"unknown":{"enabled":true}}`
}

func assertUsageJSON(t *testing.T, got, want []byte) {
	t.Helper()
	decode := func(data []byte) any {
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(decode(got), decode(want)) {
		t.Fatalf("JSON changed incorrectly:\ngot  %s\nwant %s", got, want)
	}
}
