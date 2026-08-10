package proxy

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"sync/atomic"
)

var codexInstalledHTTPClientBuildPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

// codexInstalledHTTPRouteAudit is attached only to the one-shot validation
// listener. It records aggregate route counts and never retains URL, query,
// header, body, model, account, request, response, or credential material.
type codexInstalledHTTPRouteAudit struct {
	clientBuild      string
	localToken       string
	modelRequests    atomic.Uint64
	unexpectedRoutes atomic.Uint64
}

func newCodexInstalledHTTPRouteAudit(clientBuild, localToken string) (*codexInstalledHTTPRouteAudit, error) {
	if !codexInstalledHTTPClientBuildPattern.MatchString(clientBuild) || !validCodexInstalledHTTPValidationToken(localToken) {
		return nil, errors.New("installed Codex client build is invalid")
	}
	return &codexInstalledHTTPRouteAudit{clientBuild: clientBuild, localToken: localToken}, nil
}

func (audit *codexInstalledHTTPRouteAudit) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if audit == nil || next == nil || request == nil {
			http.Error(writer, "installed Codex route audit unavailable", http.StatusServiceUnavailable)
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/health" && request.URL.RawQuery == "" {
			next.ServeHTTP(writer, request)
			return
		}
		expectedAuthorization := []byte("Bearer " + audit.localToken)
		presentedAuthorization := []byte(request.Header.Get("Authorization"))
		authorised := len(expectedAuthorization) == len(presentedAuthorization) && subtle.ConstantTimeCompare(expectedAuthorization, presentedAuthorization) == 1
		clearBytes(expectedAuthorization)
		clearBytes(presentedAuthorization)
		if !authorised {
			http.NotFound(writer, request)
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/models" && request.URL.RawQuery == "client_version="+audit.clientBuild:
			audit.modelRequests.Add(1)
		case request.Method == http.MethodPost && request.URL.RawQuery == "" &&
			(request.URL.Path == codexResponsesPath ||
				request.URL.Path == legacyCodexResponsesPath ||
				request.URL.Path == codexCompactResponsesPath ||
				request.URL.Path == legacyCodexCompactResponsesPath):
		default:
			audit.unexpectedRoutes.Add(1)
			http.NotFound(writer, request)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (audit *codexInstalledHTTPRouteAudit) snapshot() (modelRequests, unexpectedRoutes uint64) {
	if audit == nil {
		return 0, 0
	}
	return audit.modelRequests.Load(), audit.unexpectedRoutes.Load()
}
