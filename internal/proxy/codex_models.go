package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	cqhttp "github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

// Model entitlements belong to the native caller, independently of the pool
// used for inference. Never replace its bearer or retry under another account.
func (s *Server) handleCodexNativeModels(w http.ResponseWriter, r *http.Request) {
	if s.CodexModelsTransport == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "Codex model discovery not configured")
		return
	}
	if bearerToken(r) == "" || s.isValidToken(bearerToken(r)) {
		writeError(w, http.StatusUnauthorized, "authentication_error", "Codex model discovery requires native account authentication")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	upstream, err := url.Parse(strings.TrimRight(s.Config.CodexUpstream, "/"))
	if err != nil || upstream.Host == "" || (upstream.Scheme != "https" && upstream.Scheme != "http") {
		writeError(w, http.StatusServiceUnavailable, "api_error", "Codex model discovery upstream unavailable")
		return
	}
	rp := &httputil.ReverseProxy{
		Transport: s.CodexModelsTransport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			// CQ additions have their own lifetime, so an upstream ETag cannot
			// validate the combined representation.
			pr.Out.Header.Del("If-None-Match")
			pr.Out.Header.Del("If-Modified-Since")
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: func(resp *http.Response) error {
			body, err := cqhttp.ReadBody(resp.Body)
			if err != nil {
				return fmt.Errorf("read Codex models response")
			}
			if resp.StatusCode == http.StatusOK {
				snap := modelregistry.Snapshot{}
				if s.Catalog != nil {
					snap = s.Catalog.Snapshot()
				}
				body, err = modelregistry.AugmentCodexModels(body, snap)
				if err != nil {
					return fmt.Errorf("invalid Codex models response")
				}
				resp.Header.Del("ETag")
				resp.Header.Set("Content-Type", "application/json")
				resp.Header.Set("Cache-Control", "no-store")
			}
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Del("Content-Length")
			resp.Header.Del("Content-Encoding")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeError(w, http.StatusBadGateway, "api_error", "Codex model discovery failed")
		},
	}
	rp.ServeHTTP(w, r)
}
