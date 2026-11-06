package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

type responsesProviderRouteContextKey struct{}

// responsesProviderRoute inspects a body already read under native admission.
// A decline leaves the encoded bytes intact for the existing Codex planner.
type responsesProviderRoute func(http.ResponseWriter, *http.Request, []byte) (bool, string)

// serveSubscriptionThroughNativeAdmission reuses the real native handler's
// request lifetime, body cancellation and shutdown drain before selecting a
// subscription provider. Legacy or injected handlers retain their own contract.
func (s *Server) serveSubscriptionThroughNativeAdmission(w http.ResponseWriter, r *http.Request, compact bool) bool {
	if _, ok := s.CodexNativeHTTP.(*CodexNativeHTTPHandler); !ok {
		return false
	}
	route := responsesProviderRoute(func(w http.ResponseWriter, request *http.Request, encoded []byte) (bool, string) {
		if handled, model := s.tryHandleGrokEncoded(w, request, encoded); handled {
			return true, model
		}
		// Z.ai's adapter can reuse the already bounded body as well. It must
		// never read the original downstream stream outside native admission.
		buffered := request.Clone(request.Context())
		buffered.Body = io.NopCloser(bytes.NewReader(encoded))
		return s.tryHandleZAI(w, buffered)
	})
	r = r.WithContext(context.WithValue(r.Context(), responsesProviderRouteContextKey{}, route))
	if compact {
		s.handleNativeCodexCompact(w, r, r.URL.Path)
	} else {
		s.handleNativeCodex(w, r)
	}
	return true
}
