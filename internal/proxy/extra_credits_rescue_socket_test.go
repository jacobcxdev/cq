package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type rescueDynamicCredits struct{ blocked atomic.Bool }

func (p *rescueDynamicCredits) Restricted() (bool, error) { return p.blocked.Load(), nil }
func TestExtraCreditsRescueOpenSocketHonoursToggle(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			kind, body, err := c.ReadMessage()
			if err != nil {
				return
			}
			calls.Add(1)
			if c.WriteMessage(kind, body) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	policy := &rescueDynamicCredits{}
	relay := &RescueRelay{Origin: testRescueOrigin(t), ExtraCredits: policy, Transport: rescueRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = target.Scheme
		u.Host = target.Host
		out.URL = &u
		out.Host = u.Host
		return http.DefaultTransport.RoundTrip(out)
	})}
	server := httptest.NewServer(relay)
	defer server.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	policy.blocked.Store(true)
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra"}`)); err != nil {
		return
	}
	_, _, _ = c.ReadMessage()
	if got := calls.Load(); got != 1 {
		t.Fatalf("disabled rescue socket forwarded new inference: upstream calls=%d, want 1", got)
	}
}
