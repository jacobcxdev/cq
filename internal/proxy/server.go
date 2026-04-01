package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const maxRequestBody = 10 << 20 // 10 MiB

// Server is the reverse proxy HTTP server.
type Server struct {
	Config         *Config
	Selector       ClaudeSelector
	Discover       ClaudeDiscoverer
	Transport      http.RoundTripper
	CodexSelector  CodexSelector
	CodexDiscover  CodexDiscoverer
	CodexTransport http.RoundTripper
}

// ListenAndServe starts the proxy and blocks until the context is cancelled or a signal is received.
func (s *Server) ListenAndServe(ctx context.Context) error {
	upstream, err := url.Parse(s.Config.ClaudeUpstream)
	if err != nil {
		return fmt.Errorf("parse upstream URL: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("/", s.proxyHandler(upstream))

	addr := fmt.Sprintf("127.0.0.1:%d", s.Config.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Fprintf(os.Stderr, "cq: proxy listening on %s\n", addr)

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	var claudeCount int
	if s.Discover != nil {
		claudeCount = len(s.Discover())
	}
	var codexCount int
	if s.CodexDiscover != nil {
		codexCount = len(s.CodexDiscover())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"accounts": map[string]int{
			"claude": claudeCount,
			"codex":  codexCount,
		},
	})
}

// isValidToken returns true if token matches the local proxy token or the
// access token of any known Claude account. This allows Claude Code to
// authenticate with its own OAuth token (preserving subscriber detection)
// instead of requiring ANTHROPIC_API_KEY which disables OAuth features.
func (s *Server) isValidToken(token string) bool {
	if token == s.Config.LocalToken {
		return true
	}
	if s.Discover == nil {
		return false
	}
	for _, acct := range s.Discover() {
		if acct.AccessToken != "" && acct.AccessToken == token {
			return true
		}
	}
	return false
}

func (s *Server) proxyHandler(upstream *url.URL) http.HandlerFunc {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("x-api-key")
		},
		Transport:     s.Transport,
		FlushInterval: -1, // flush immediately for SSE streaming
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "api_error", err.Error())
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.Request != nil {
				fmt.Fprintf(os.Stderr, "cq: proxy %s %s → %d\n",
					resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
			}
			return nil
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// Auth check: accept local proxy token or a known Claude account token.
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !s.isValidToken(token) {
			writeError(w, http.StatusForbidden, "authentication_error", "invalid proxy token")
			return
		}

		// Buffer body for replay via GetBody on 401/429 retries.
		var buf []byte
		if r.Body != nil {
			var err error
			buf, err = io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
			r.Body.Close()
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
				return
			}
			if len(buf) > maxRequestBody {
				writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds 10 MiB")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(buf))
			r.ContentLength = int64(len(buf))
			r.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(buf)), nil
			}
		}

		// Route based on model.
		model := extractModel(buf)
		if RouteModel(model) == ProviderCodex {
			s.handleCodex(w, r, buf)
			return
		}

		rp.ServeHTTP(w, r)
	}
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}
