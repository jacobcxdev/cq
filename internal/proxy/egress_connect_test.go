package proxy

import (
	"crypto/tls"
	"github.com/jacobcxdev/cq/internal/httputil"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEgressOriginDialThroughHTTPAndHTTPSProxy(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("payload", 10000))
	}))
	defer origin.Close()
	for _, secureProxy := range []bool{false, true} {
		var calls atomic.Int32
		proxy := newCodexEgressTestProxy(t, &calls)
		defer proxy.Close()
		proxyURL := proxy.URL
		var trust *tls.Config
		if secureProxy {
			secured := httptest.NewTLSServer(proxy.Config.Handler)
			defer secured.Close()
			proxyURL = secured.URL
			trust = secured.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		}
		egress, err := newCodexEgress(proxyURL, trust)
		if err != nil {
			t.Fatal(err)
		}
		defer egress.Close()
		// Use the raw-origin dialer that workers and rescue share through the broker.
		transport := &http.Transport{DialContext: egress.DialContext, TLSClientConfig: origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		response, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := httputil.ReadBody(response.Body)
		response.Body.Close()
		if err != nil || len(body) != 70000 || calls.Load() != 1 {
			t.Fatalf("raw tunnel payload=%d proxycalls=%d err=%v", len(body), calls.Load(), err)
		}
	}
}
