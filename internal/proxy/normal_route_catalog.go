package proxy

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

type NormalCallerBranchClassifier func(method, requestURI string, body []byte) (NormalCallerDomain, error)

func NewNormalCallerBranchClassifier(catalog *modelregistry.Catalog) NormalCallerBranchClassifier {
	return func(method, requestURI string, body []byte) (NormalCallerDomain, error) {
		request, err := http.NewRequest(method, requestURI, nil)
		if err != nil {
			return "", ErrNormalCallerAuthScope
		}
		policy := normalCallerPolicy(request)
		switch policy {
		case normalCallerRouteCodex:
			return NormalCallerCodex, nil
		case normalCallerRouteLocalOrClaude:
			return NormalCallerClaude, nil
		case normalCallerRouteClassified:
			if RouteRequestWithCatalog(method, request.URL.Path, extractModel(body), catalog) == ProviderCodex {
				return NormalCallerCodex, nil
			}
			return NormalCallerClaude, nil
		default:
			return "", errors.New("public route has no provider branch")
		}
	}
}

func normalCallerAllowsBranch(domain, branch NormalCallerDomain) bool {
	return domain == NormalCallerLocal || domain == branch
}

type normalCallerRoutePolicy uint8

const (
	normalCallerRoutePublic normalCallerRoutePolicy = iota
	normalCallerRouteLocal
	normalCallerRouteCodex
	normalCallerRouteLocalOrClaude
	normalCallerRouteClassified
)

func normalCallerPolicy(request *http.Request) normalCallerRoutePolicy {
	if request == nil {
		return normalCallerRouteClassified
	}
	path := request.URL.EscapedPath()
	if request.Method == http.MethodGet && (path == "/health" || path == "/v1/models") {
		return normalCallerRoutePublic
	}
	if request.Method == http.MethodPost && path == "/v1/registry/refresh" {
		return normalCallerRouteLocal
	}
	if (request.Method == http.MethodGet || request.Method == http.MethodPut) && path == RuntimePolicyPath {
		return normalCallerRouteLocal
	}
	if request.Method == http.MethodPost && path == RuntimePolicySessionDigestPath {
		return normalCallerRouteLocal
	}
	if request.Method == http.MethodGet && path == "/v1/registry" {
		return normalCallerRouteLocalOrClaude
	}
	if isNormalClaudeRoute(path) {
		return normalCallerRouteLocalOrClaude
	}
	if isNormalCodexRoute(path) {
		return normalCallerRouteCodex
	}
	return normalCallerRouteClassified
}

func isNormalClaudeRoute(path string) bool {
	switch path {
	case "/v1/messages", "/v1/messages/count_tokens", "/api/event_logging/batch", "/api/claude_code/organizations/metrics", "/v1/files", "/v1/skills":
		return true
	}
	for _, prefix := range []string{"/v1/files/", "/v1/skills/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func isNormalCodexRoute(path string) bool {
	switch path {
	case "/models", "/v1/responses", "/responses", "/v1/responses/compact", "/responses/compact", "/app-server", "/alpha/search", "/live", "/v1/live", "/realtime/calls", "/v1/realtime/calls", "/realtime", "/v1/realtime":
		return true
	}
	for _, prefix := range []string{"/v1/images/", "/images/", "/live/", "/v1/live/", "/realtime/calls/", "/v1/realtime/calls/"} {
		if len(path) >= len(prefix) && path[:len(prefix)] == prefix {
			return true
		}
	}
	for _, pattern := range unsupportedOpenAICompatibleRoutePatterns() {
		if pattern[len(pattern)-1] == '/' {
			if len(path) >= len(pattern) && path[:len(pattern)] == pattern {
				return true
			}
		} else if path == pattern {
			return true
		}
	}
	return false
}
