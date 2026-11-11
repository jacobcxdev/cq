package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	stdhttputil "net/http/httputil"
	"net/url"
)

type RescueRelay struct {
	// Rescue has no credential coordinator or fresh quota reader. Refuse when
	// any account prohibits credits instead of bypassing worker admission.
	ExtraCredits interface{ Restricted() (bool, error) }
	Transport    http.RoundTripper
	Origin       *url.URL
	// Unavailable refuses rather than running a competing VPN identity while
	// a separate normal worker drains or recovers.
	Unavailable bool
}

func (relay *RescueRelay) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if relay == nil || relay.Unavailable || request == nil || !validRescueOrigin(relay.Origin) {
		writeRescueError(writer, http.StatusServiceUnavailable, "rescue_unavailable")
		return
	}
	if relay.ExtraCredits != nil {
		restricted, err := relay.ExtraCredits.Restricted()
		if err != nil || restricted {
			writeRescueError(writer, http.StatusServiceUnavailable, "extra_credits_policy_unavailable")
			return
		}
	}

	proxy := &stdhttputil.ReverseProxy{
		Rewrite: func(proxyRequest *stdhttputil.ProxyRequest) {
			proxyRequest.SetURL(relay.Origin)
			proxyRequest.Out.URL.RawQuery = proxyRequest.In.URL.RawQuery
		},
		Transport:     rescuePolicyTransport{inner: relay.Transport, policy: relay.ExtraCredits},
		FlushInterval: -1,
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			writeRescueError(writer, http.StatusBadGateway, "rescue_upstream_unavailable")
		},
	}
	proxy.ServeHTTP(writer, request)
}

type rescueCreditPolicy interface{ Restricted() (bool, error) }

// Upgraded connections outlive ServeHTTP's initial policy check. Check every
// client write as well, so a later toggle cannot bypass admission on that tunnel.
type rescuePolicyTransport struct {
	inner  http.RoundTripper
	policy rescueCreditPolicy
}

func (t rescuePolicyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	inner := t.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	response, err := inner.RoundTrip(request)
	if err == nil && response != nil && response.StatusCode == http.StatusSwitchingProtocols && t.policy != nil {
		if body, ok := response.Body.(io.ReadWriteCloser); ok {
			response.Body = &rescuePolicyConnection{ReadWriteCloser: body, policy: t.policy}
		}
	}
	return response, err
}

type rescuePolicyConnection struct {
	io.ReadWriteCloser
	policy rescueCreditPolicy
}

func (c *rescuePolicyConnection) Write(data []byte) (int, error) {
	restricted, err := c.policy.Restricted()
	if err != nil || restricted {
		return 0, errors.New("rescue extra credits policy rejected forwarding")
	}
	return c.ReadWriteCloser.Write(data)
}

func validRescueOrigin(origin *url.URL) bool {
	return origin != nil && origin.Scheme == "https" && origin.Host == "chatgpt.com" && origin.Path == "/backend-api/codex" && origin.RawQuery == "" && origin.Fragment == "" && origin.User == nil
}

func writeRescueError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]string{"type": "rescue_error", "code": code}})
}
