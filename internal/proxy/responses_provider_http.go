package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

type responsesProviderRouteContextKey struct{}
type codexHTTPSubscriptionFallbackKey struct{}

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
		if handled, model := s.tryHandleProviderResponsesEncoded(w, request, encoded); handled {
			return true, model
		}
		// Z.ai's adapter can reuse the already bounded body as well. It must
		// never read the original downstream stream outside native admission.
		buffered := request.Clone(request.Context())
		buffered.Body = io.NopCloser(bytes.NewReader(encoded))
		return s.tryHandleZAI(w, buffered)
	})
	ctx := context.WithValue(r.Context(), responsesProviderRouteContextKey{}, route)
	ctx = context.WithValue(ctx, codexHTTPSubscriptionFallbackKey{}, responsesProviderRoute(s.tryCodexSharedBudgetFallback))
	r = r.WithContext(ctx)
	if compact {
		s.handleNativeCodexCompact(w, r, r.URL.Path)
	} else {
		s.handleNativeCodex(w, r)
	}
	return true
}

func (s *Server) tryHandleProviderResponses(w http.ResponseWriter, r *http.Request) (bool, string) {
	if r.Body == nil {
		return false, ""
	}
	body, err := readCodexNativeHTTPRequest(r)
	if err != nil {
		writeError(w, 400, "invalid_request_error", "failed to read request body")
		return true, ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return s.tryHandleProviderResponsesEncoded(w, r, body)
}

func (s *Server) tryHandleProviderResponsesEncoded(w http.ResponseWriter, r *http.Request, body []byte) (bool, string) {
	encoding, err := parseCodexContentEncoding(r.Header)
	if err != nil {
		return false, ""
	}
	decoded := body
	if encoding == "zstd" || encoding == "" && codexZstdFrame(body) {
		envelope, err := DecodeCodexRequest(body, encoding, codexHTTPZstdLimits())
		if err != nil {
			return false, ""
		}
		decoded = envelope.Decoded()
	}
	model := extractModel(decoded)
	provider := s.routeCodexModel(model)
	switch provider {
	case ProviderSharedBudget, ProviderZAI, ProviderGrok:
		if !standardResponsesAccessPrograms(decoded) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "subscription route cannot enforce the requested access programme")
			return true, model
		}
	}
	if s.serveSharedBudgetContinuation(w, r, decoded, model) {
		return true, model
	}
	if selected, bound := s.sharedBudgetTurnRoute(r.Context(), r.Header, decoded, model); bound {
		if selected.id == "" {
			writeError(w, http.StatusConflict, "invalid_request_error", errSharedBudgetContinuation.Error())
		} else {
			s.serveSelectedSharedBudgetHTTP(w, r, decoded, selected)
		}
		return true, model
	}
	if provider == ProviderSharedBudget {
		s.serveSharedBudgetHTTP(w, r, decoded)
		return true, model
	}
	if provider == ProviderGrok {
		return s.serveGrokHTTP(w, r, body, decoded, model)
	}
	return false, ""
}
