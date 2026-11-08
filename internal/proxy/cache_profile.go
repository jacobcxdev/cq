package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/cacheprofile"
)

type CacheProfileEvent struct {
	SchemaVersion         int                      `json:"schema_version"`
	Time                  time.Time                `json:"time"`
	EventType             string                   `json:"event_type"`
	Protocol              cacheprofile.Protocol    `json:"protocol"`
	Route                 string                   `json:"route"`
	OriginHash            string                   `json:"origin_hash,omitempty"`
	SessionHash           string                   `json:"session_hash,omitempty"`
	AccountHash           string                   `json:"account_hash,omitempty"`
	AccountSource         string                   `json:"account_source"`
	ModelHash             string                   `json:"model_hash,omitempty"`
	PartitionHash         string                   `json:"partition_hash,omitempty"`
	Snapshot              cacheprofile.Snapshot    `json:"snapshot"`
	Comparison            *cacheprofile.Comparison `json:"comparison,omitempty"`
	Reason                string                   `json:"reason"`
	Usage                 cacheprofile.Usage       `json:"usage"`
	StatusCode            int                      `json:"status_code,omitempty"`
	Outcome               string                   `json:"outcome"`
	DurationMS            int64                    `json:"duration_ms"`
	InterRequestGapMS     *int64                   `json:"inter_request_gap_ms,omitempty"`
	CachedTokensDecreased bool                     `json:"cached_tokens_decreased,omitempty"`
}

const cacheProfileRequestLimit = 10 << 20
const cacheProfileSessionLimit = 64

// CacheProfiler stores only keyed content fingerprints and numeric usage. Its
// private key lives for this serving process; reopening starts a new baseline.
type CacheProfiler struct {
	writer    *jsonlWriter
	key       [32]byte
	engine    *cacheprofile.Profiler
	mu        sync.Mutex
	previous  map[string]CacheProfileEvent
	order     []string
	websocket map[string]*cacheProfileAttempt
}

func OpenCacheProfiler(path string) (*CacheProfiler, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("cache profiling path is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cache profiling path unavailable")
	}
	w, err := openJSONLWriterWithLimits(path, 8<<20, 2)
	if err != nil {
		return nil, err
	}
	p := &CacheProfiler{writer: w, previous: make(map[string]CacheProfileEvent), websocket: make(map[string]*cacheProfileAttempt)}
	if _, err = rand.Read(p.key[:]); err != nil {
		_ = w.close()
		return nil, fmt.Errorf("cache profiling key unavailable")
	}
	p.engine, err = cacheprofile.New(p.key[:])
	if err != nil {
		_ = w.close()
		return nil, err
	}
	return p, nil
}

func (p *CacheProfiler) Close() error {
	if p == nil || p.writer == nil {
		return nil
	}
	p.mu.Lock()
	pending := make([]*cacheProfileAttempt, 0, len(p.websocket))
	for _, attempt := range p.websocket {
		pending = append(pending, attempt)
	}
	p.websocket = make(map[string]*cacheProfileAttempt)
	p.mu.Unlock()
	for _, attempt := range pending {
		attempt.finish("closed", cacheprofile.Usage{})
	}
	return p.writer.close()
}

func (p *CacheProfiler) WrapTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if p == nil {
		return base
	}
	if wrapped, ok := base.(*cacheProfileTransport); ok && wrapped.profiler == p {
		return base
	}
	return &cacheProfileTransport{base: base, profiler: p}
}

func (p *CacheProfiler) WrapClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	out := *client
	out.Transport = p.WrapTransport(client.Transport)
	return &out
}

func (p *CacheProfiler) hash(domain, value string) string {
	if value == "" {
		return ""
	}
	h := hmac.New(sha256.New, p.key[:])
	_, _ = io.WriteString(h, domain+"\x00"+value)
	return hex.EncodeToString(h.Sum(nil))
}

func cacheProfileSession(headers http.Header) string {
	for _, name := range []string{"Session_id", "X-Session-Id", "X-Claude-Code-Session-Id", "X-Codex-Thread-Id", "X-Thread-Id"} {
		if v := headers.Get(name); v != "" {
			return v
		}
	}
	return ""
}

type cacheProfileSessionContextKey struct{}

func cacheProfileRequestSession(headers http.Header, body []byte) string {
	metadata, err := parseCodexTurnMetadataWithNestedLimit(body, headers.Get(codexTurnMetadataKey), nil, cacheProfileRequestLimit)
	if err == nil && metadata.Found && metadata.Metadata.ThreadID != "" {
		return "native_thread:" + metadata.Metadata.SessionID + ":" + metadata.Metadata.ThreadID
	}
	return cacheProfileSession(headers)
}

func withCacheProfileRequestSession(ctx context.Context, headers http.Header, body []byte) context.Context {
	if session := cacheProfileRequestSession(headers, body); session != "" {
		return context.WithValue(ctx, cacheProfileSessionContextKey{}, session)
	}
	return ctx
}

func (p *CacheProfiler) account(ctx context.Context, headers http.Header) (string, string) {
	if diag, _ := ctx.Value(routeDiagnosticsContextKey{}).(*routeDiagnostics); diag != nil {
		hint, _ := diag.fields()
		if hint != "" {
			return p.hash("account", hint), "route_account"
		}
	}
	for _, name := range []string{"Authorization", "X-Api-Key"} {
		if v := headers.Get(name); v != "" {
			return p.hash("credential", v), "credential"
		}
	}
	return "", "missing"
}

func cacheProfileProtocol(r *http.Request) (cacheprofile.Protocol, string, bool) {
	if r == nil || r.URL == nil || r.Method != http.MethodPost {
		return "", "", false
	}
	path := strings.TrimRight(r.URL.Path, "/")
	route := "native"
	switch {
	case strings.HasSuffix(path, "/messages"):
		if r.URL.Hostname() == "zcode.z.ai" {
			if strings.Contains(path, "/zcode-plan/") {
				route = "zcode-start"
			} else if strings.Contains(path, "/ultra-zai/") {
				route = "zcode"
			}
		}
		return cacheprofile.Messages, route, true
	case strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/responses/compact"):
		return cacheprofile.Responses, route, true
	case strings.HasSuffix(path, "/chat/completions"):
		return cacheprofile.ChatCompletions, route, true
	case strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent"):
		return cacheprofile.Gemini, route, true
	default:
		return "", "", false
	}
}

type cacheProfileTransport struct {
	base     http.RoundTripper
	profiler *CacheProfiler
}

func (t *cacheProfileTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	protocol, route, ok := cacheProfileProtocol(r)
	if !ok {
		return t.base.RoundTrip(r)
	}
	p := t.profiler
	event := CacheProfileEvent{SchemaVersion: 1, Time: time.Now().UTC(), EventType: "cache_profile", Protocol: protocol, Route: route, Reason: "request_unavailable"}
	event.OriginHash = p.hash("origin", r.URL.Scheme+"://"+r.URL.Host)
	event.SessionHash = p.hash("session", cacheProfileSession(r.Header))
	if session, _ := r.Context().Value(cacheProfileSessionContextKey{}).(string); session != "" {
		event.SessionHash = p.hash("session", session)
	}
	event.AccountHash, event.AccountSource = p.account(r.Context(), r.Header)
	if r.GetBody != nil {
		copy, err := r.GetBody()
		if err == nil && copy != nil {
			body, readErr := io.ReadAll(io.LimitReader(copy, cacheProfileRequestLimit+1))
			_ = copy.Close()
			if readErr == nil && len(body) <= cacheProfileRequestLimit {
				encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
				if encoding != "" || codexZstdFrame(body) {
					decoded, decodeErr := DecodeCodexRequest(body, encoding, CodexZstdLimits{MaxEncodedBytes: cacheProfileRequestLimit, MaxDecodedBytes: cacheProfileRequestLimit, MaxExpansion: 128})
					if decodeErr != nil {
						body = nil
					} else {
						body = decoded.Decoded()
					}
				}
				if protocol == cacheprofile.Gemini {
					body = cacheProfileGeminiModelCopy(r, body)
				}
				if session, _ := r.Context().Value(cacheProfileSessionContextKey{}).(string); session == "" {
					event.SessionHash = p.hash("session", cacheProfileRequestSession(r.Header, body))
				}
				snapshot, captureErr := p.engine.Capture(protocol, body)
				event.Snapshot = snapshot
				event.ModelHash = snapshot.ModelHash
				if snapshot.ModelHash != "" {
					event.PartitionHash = p.partition(body, r.Header)
				}
				if captureErr == nil || snapshot.ModelHash != "" {
					event.Reason = "unknown_prefix"
				} else {
					event.Reason = "invalid_request"
				}
			}
		}
	}
	a := &cacheProfileAttempt{profiler: p, event: event, started: time.Now()}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		a.finish("transport_error", cacheprofile.Usage{})
		return response, err
	}
	if response == nil {
		a.finish("empty_response", cacheprofile.Usage{})
		return response, err
	}
	a.event.StatusCode = response.StatusCode
	if response.Body == nil {
		a.finish("empty_response", cacheprofile.Usage{})
		return response, nil
	}
	response.Body = newCacheProfileBody(response.Body, response.Header.Get("Content-Type"), a)
	return response, nil
}

func cacheProfileGeminiModelCopy(r *http.Request, body []byte) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return body
	}
	if _, ok := obj["model"]; ok {
		return body
	}
	_, model, ok := strings.Cut(r.URL.Path, "/models/")
	if !ok {
		return body
	}
	model, _, _ = strings.Cut(model, ":")
	if model == "" {
		return body
	}
	obj["model"], _ = json.Marshal(model)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

type cacheProfileAttempt struct {
	profiler *CacheProfiler
	event    CacheProfileEvent
	started  time.Time
	once     sync.Once
	wsUsage  cacheProfileUsageParser
}

func (a *cacheProfileAttempt) finish(outcome string, usage cacheprofile.Usage) {
	a.once.Do(func() {
		if a.event.StatusCode != 0 && (a.event.StatusCode < 200 || a.event.StatusCode >= 300) {
			usage = cacheprofile.Usage{}
		}
		a.event.Outcome = outcome
		a.event.DurationMS = time.Since(a.started).Milliseconds()
		a.event.Usage = usage
		a.profiler.record(a.event)
	})
}

func (p *CacheProfiler) record(event CacheProfileEvent) {
	p.mu.Lock()
	if event.SessionHash == "" {
		event.Reason = "missing_session"
	} else if event.AccountHash == "" {
		event.Reason = "missing_account"
	} else if event.ModelHash != "" {
		if previous, ok := p.previous[event.SessionHash]; !ok {
			event.Reason = "cold_start"
		} else {
			gap := max(int64(0), event.Time.Sub(previous.Time).Milliseconds()-previous.DurationMS)
			event.InterRequestGapMS = &gap
			switch {
			case previous.AccountHash != event.AccountHash:
				event.Reason = "account_changed"
				if previous.AccountSource == "credential" || event.AccountSource == "credential" {
					event.Reason = "credential_identity_changed"
				}
			case previous.Route != event.Route || previous.Protocol != event.Protocol || previous.OriginHash != event.OriginHash:
				event.Reason = "route_changed"
			case previous.ModelHash != event.ModelHash:
				event.Reason = "model_changed"
			case previous.PartitionHash != event.PartitionHash:
				event.Reason = "cache_partition_changed"
			default:
				// A numerical drop is observable even when the provider owns
				// opaque history. It is a review flag, not proof of a regression.
				event.CachedTokensDecreased = previous.Usage.Known && event.Usage.Known && event.Usage.CachedInputTokens < previous.Usage.CachedInputTokens
				if !previous.Snapshot.Complete || !event.Snapshot.Complete {
					event.Reason = "unknown_prefix"
				} else {
					comparison := cacheprofile.Compare(previous.Snapshot, event.Snapshot)
					event.Comparison = &comparison
					event.Reason = "prefix_changed"
					if comparison.PrefixStable {
						event.Reason = "stable_prefix"
					}
				}
			}
		}
	}
	if event.SessionHash != "" && event.AccountHash != "" && event.ModelHash != "" && event.Outcome == "completed" && (event.StatusCode == 0 || event.StatusCode >= 200 && event.StatusCode < 300) {
		if _, ok := p.previous[event.SessionHash]; !ok {
			if len(p.order) >= cacheProfileSessionLimit {
				delete(p.previous, p.order[0])
				p.order = p.order[1:]
			}
			p.order = append(p.order, event.SessionHash)
		}
		p.previous[event.SessionHash] = event
	}
	p.mu.Unlock()
	if err := p.writer.encode(event); err != nil {
		fmt.Fprintln(os.Stderr, "cq: cache profiling: write failed")
	}
}

func (p *CacheProfiler) partition(body []byte, headers http.Header) string {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	parts := make(map[string]any)
	for _, field := range []string{"prompt_cache_key", "prompt_cache_retention", "service_tier"} {
		if raw, ok := request[field]; ok {
			parts[field] = raw
		}
	}
	for _, field := range []string{"Openai-Organization", "Openai-Project"} {
		if value := headers.Get(field); value != "" {
			parts[field] = value
		}
	}
	data, _ := json.Marshal(parts)
	return p.hash("cache_partition", string(data))
}
