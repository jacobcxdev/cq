package proxy

import (
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// guardResponsesOrigin keeps subscription access out of cross-origin browser
// requests, including WebSocket upgrades on the standalone listener.
func guardResponsesOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowsResponsesOrigin(r) {
			writeError(w, http.StatusForbidden, "authentication_error", "browser origin not allowed for Responses")
			return
		}
		next(w, r)
	}
}

func allowsResponsesOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if err != nil || origin.Scheme != scheme || !strings.EqualFold(origin.Host, r.Host) ||
		origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return false
	}
	host := origin.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
