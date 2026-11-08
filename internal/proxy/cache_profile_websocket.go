package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/cacheprofile"
)

type cacheProfileContextKey struct{}
type cacheProfileBrokerContextKey struct{}
type cacheProfileContext struct {
	profiler *CacheProfiler
	session  string
}

func withCacheProfile(ctx context.Context, p *CacheProfiler, headers http.Header) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, cacheProfileContextKey{}, cacheProfileContext{profiler: p, session: cacheProfileRequestSession(headers, []byte(`{}`))})
}

func observeCacheProfilePayload(ctx context.Context, event PayloadEvent, raw []byte) {
	profile, ok := ctx.Value(cacheProfileContextKey{}).(cacheProfileContext)
	if !ok || profile.profiler == nil {
		return
	}
	if event.Direction == "downstream_request" {
		if broker, _ := ctx.Value(cacheProfileBrokerContextKey{}).(bool); !broker {
			beginCacheProfileWebSocket(ctx, raw, event.AccountHint)
		}
		return
	}
	if event.Direction != "upstream_response" {
		return
	}
	key, _ := codexTraceIDs(ctx)
	if key == "" {
		return
	}
	p := profile.profiler
	p.mu.Lock()
	attempt := p.websocket[key]
	if attempt == nil {
		p.mu.Unlock()
		return
	}
	if event.AccountHint != "" {
		attempt.event.AccountHash = p.hash("account", event.AccountHint)
		attempt.event.AccountSource = "route_account"
	}
	if len(raw) > cacheProfileResponseLimit {
		attempt.wsUsage.failed = true
	} else {
		attempt.wsUsage.consume(raw)
	}
	terminal := attempt.wsUsage.terminal
	var envelope struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &envelope)
	if envelope.Type == "error" || envelope.Type == "response.failed" || envelope.Type == "response.incomplete" {
		terminal = true
	}
	if terminal {
		delete(p.websocket, key)
	}
	usage := attempt.wsUsage.result()
	p.mu.Unlock()
	if terminal {
		attempt.finish("completed", usage)
	}
}

// Broker callers invoke this with the actual encoded frame and selected account
// immediately before upstream write. Legacy passthrough uses identical bytes.
func beginCacheProfileWebSocket(ctx context.Context, raw []byte, accountHint string) {
	profile, ok := ctx.Value(cacheProfileContextKey{}).(cacheProfileContext)
	if !ok || profile.profiler == nil {
		return
	}
	var envelope struct {
		Type     string `json:"type"`
		Generate *bool  `json:"generate"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Type != "response.create" || envelope.Generate != nil && !*envelope.Generate {
		return
	}
	key, _ := codexTraceIDs(ctx)
	if key == "" {
		return
	}
	p := profile.profiler
	session := cacheProfileRequestSession(nil, raw)
	if session == "" {
		session = profile.session
	}
	event := CacheProfileEvent{SchemaVersion: 1, EventType: "cache_profile", Time: time.Now().UTC(), Protocol: cacheprofile.Responses, Route: "codex_websocket", Reason: "request_unavailable", AccountSource: "missing", SessionHash: p.hash("session", session)}
	if accountHint != "" {
		event.AccountHash = p.hash("account", accountHint)
		event.AccountSource = "route_account"
	}
	if snapshot, err := p.engine.Capture(cacheprofile.Responses, raw); err == nil {
		event.Snapshot = snapshot
		event.ModelHash = snapshot.ModelHash
		event.PartitionHash = p.partition(raw, nil)
		event.Reason = "unknown_prefix"
	} else {
		event.Reason = "invalid_request"
	}
	attempt := &cacheProfileAttempt{profiler: p, event: event, started: time.Now()}
	var replaced *cacheProfileAttempt
	p.mu.Lock()
	if previous := p.websocket[key]; previous != nil {
		replaced = previous
	} else if len(p.websocket) >= cacheProfileSessionLimit {
		for oldKey, old := range p.websocket {
			replaced = old
			delete(p.websocket, oldKey)
			break
		}
	}
	p.websocket[key] = attempt
	p.mu.Unlock()
	if replaced != nil {
		replaced.finish("interrupted", cacheprofile.Usage{})
	}
}

func finishCacheProfileWebSocket(ctx context.Context, outcome string) {
	profile, ok := ctx.Value(cacheProfileContextKey{}).(cacheProfileContext)
	if !ok || profile.profiler == nil {
		return
	}
	key, _ := codexTraceIDs(ctx)
	if key == "" {
		return
	}
	p := profile.profiler
	p.mu.Lock()
	attempt := p.websocket[key]
	delete(p.websocket, key)
	p.mu.Unlock()
	if attempt != nil {
		attempt.finish(outcome, cacheprofile.Usage{})
	}
}
