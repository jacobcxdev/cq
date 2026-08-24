package proxy

import (
	"encoding/json"
	"net/http"
	stdhttputil "net/http/httputil"
	"net/url"
)

type RescueRelay struct {
	Transport http.RoundTripper
	Origin    *url.URL
}

func (relay *RescueRelay) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if relay == nil || request == nil || !validRescueOrigin(relay.Origin) {
		writeRescueError(writer, http.StatusServiceUnavailable, "rescue_unavailable")
		return
	}

	proxy := &stdhttputil.ReverseProxy{
		Rewrite: func(proxyRequest *stdhttputil.ProxyRequest) {
			proxyRequest.SetURL(relay.Origin)
			proxyRequest.Out.URL.RawQuery = proxyRequest.In.URL.RawQuery
		},
		Transport:     relay.Transport,
		FlushInterval: -1,
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			writeRescueError(writer, http.StatusBadGateway, "rescue_upstream_unavailable")
		},
	}
	proxy.ServeHTTP(writer, request)
}

func validRescueOrigin(origin *url.URL) bool {
	return origin != nil && origin.Scheme == "https" && origin.Host == "chatgpt.com" && origin.Path == "/backend-api/codex" && origin.RawQuery == "" && origin.Fragment == "" && origin.User == nil
}

func writeRescueError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]string{"type": "rescue_error", "code": code}})
}
