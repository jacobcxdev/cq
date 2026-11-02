package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testCodexInstalledLocalToken = "Qx9m0c9Yx6L-1wH2fBzE3pV8uN5kT7rS4aD6jG0lM2o"

func TestCodexInstalledHTTPRouteAuditRejectsFailedCatalogueEvidence(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable} {
		audit, err := newCodexInstalledHTTPRouteAudit("0.146.0", testCodexInstalledLocalToken)
		if err != nil {
			t.Fatal(err)
		}
		handler := audit.guard(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(status)
		}))
		request := httptest.NewRequest(http.MethodGet, "/models", nil)
		request.Header.Set("Authorization", "Bearer "+testCodexInstalledLocalToken)
		handler.ServeHTTP(httptest.NewRecorder(), request)
		models, unexpected := audit.snapshot()
		if models != 0 || unexpected != 1 {
			t.Fatalf("status %d audit = models %d unexpected %d, want 0/1", status, models, unexpected)
		}
	}
}

func TestCodexInstalledHTTPRouteAuditCountsAuthorisedInstalledClientCatalogueRoutes(t *testing.T) {
	t.Parallel()
	audit, err := newCodexInstalledHTTPRouteAudit("0.146.0", testCodexInstalledLocalToken)
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	handler := audit.guard(next)

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/health", nil),
		httptest.NewRequest(http.MethodPost, codexResponsesPath, nil),
		httptest.NewRequest(http.MethodPost, codexCompactResponsesPath, nil),
		httptest.NewRequest(http.MethodPost, legacyCodexResponsesPath, nil),
		httptest.NewRequest(http.MethodPost, legacyCodexCompactResponsesPath, nil),
		httptest.NewRequest(http.MethodGet, "/models?client_version=0.146.0", nil),
		httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0-alpha.6.5&originator=codex_cli_rs", nil),
	} {
		request.Header.Set("Authorization", "Bearer "+testCodexInstalledLocalToken)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d", request.Method, request.URL.String(), recorder.Code)
		}
	}

	models, unexpected := audit.snapshot()
	if models != 2 || unexpected != 0 {
		t.Fatalf("audit = models %d unexpected %d, want 2/0", models, unexpected)
	}
}

func TestCodexInstalledHTTPRouteAuditRejectsNearMatchAndExtraRoutes(t *testing.T) {
	t.Parallel()
	audit, err := newCodexInstalledHTTPRouteAudit("0.146.0", testCodexInstalledLocalToken)
	if err != nil {
		t.Fatal(err)
	}
	nextCalls := 0
	handler := audit.guard(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		nextCalls++
		writer.WriteHeader(http.StatusNoContent)
	}))

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.146.0", nil),
		httptest.NewRequest(http.MethodPost, codexResponsesPath+"?extra=1", nil),
		httptest.NewRequest(http.MethodGet, codexCompactResponsesPath, nil),
	} {
		request.Header.Set("Authorization", "Bearer "+testCodexInstalledLocalToken)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("unexpected route status = %d, want 404", recorder.Code)
		}
	}

	models, unexpected := audit.snapshot()
	if models != 0 || unexpected != 3 {
		t.Fatalf("audit = models %d unexpected %d, want 0/3", models, unexpected)
	}
	if nextCalls != 0 {
		t.Fatalf("unexpected routes reached production mux %d times", nextCalls)
	}
}

func TestCodexInstalledHTTPRouteAuditRejectsNonExactClientBuild(t *testing.T) {
	t.Parallel()
	for _, build := range []string{"", "codex-cli 0.146.0", "0.146", "0.146.0\n", "0.146.0 other"} {
		if _, err := newCodexInstalledHTTPRouteAudit(build, testCodexInstalledLocalToken); err == nil {
			t.Fatalf("build %q accepted", build)
		}
	}
}

func TestServerHandlerInstallsCodexInstalledHTTPRouteAudit(t *testing.T) {
	t.Parallel()
	audit, err := newCodexInstalledHTTPRouteAudit("0.146.0", testCodexInstalledLocalToken)
	if err != nil {
		t.Fatal(err)
	}
	modelsHandler, closeModels, err := newCodexInstalledModelsHandler(testCodexInstalledLocalToken)
	if err != nil {
		t.Fatal(err)
	}
	defer closeModels()
	audit.modelsHandler = modelsHandler
	server := &Server{
		Config:                       &Config{ClaudeUpstream: "http://127.0.0.1:1", LocalToken: testCodexInstalledLocalToken},
		codexInstalledHTTPRouteAudit: audit,
	}
	handler, err := server.handler()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/models?client_version=0.146.0", nil)
	request.Header.Set("Authorization", "Bearer "+testCodexInstalledLocalToken)
	request.Header.Set("ChatGPT-Account-ID", "acceptance-bootstrap")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"cq_validation_native_auth":true`) {
		t.Fatalf("catalogue response = %d %s", recorder.Code, recorder.Body.String())
	}

	models, unexpected := audit.snapshot()
	if models != 1 || unexpected != 0 {
		t.Fatalf("server route audit = models %d unexpected %d, want 1/0", models, unexpected)
	}
}

func TestCodexInstalledModelsHandlerRejectsWrongNativeIdentity(t *testing.T) {
	handler, closeModels, err := newCodexInstalledModelsHandler(testCodexInstalledLocalToken)
	if err != nil {
		t.Fatal(err)
	}
	defer closeModels()
	for _, test := range []struct{ token, account string }{
		{"wrong-native-token", "acceptance-bootstrap"},
		{testCodexInstalledLocalToken, "wrong-account"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/models", nil)
		request.Header.Set("Authorization", "Bearer "+test.token)
		request.Header.Set("ChatGPT-Account-ID", test.account)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("wrong native identity status = %d", recorder.Code)
		}
	}
}

func TestCodexInstalledHTTPRouteAuditRejectsUnauthenticatedRoutesWithoutCounting(t *testing.T) {
	audit, err := newCodexInstalledHTTPRouteAudit("0.146.0", testCodexInstalledLocalToken)
	if err != nil {
		t.Fatal(err)
	}
	nextCalls := 0
	handler := audit.guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ }))
	for _, authorization := range []string{"", "Bearer " + codexAcceptanceLocalToken, "Bearer " + testCodexInstalledLocalToken + "x"} {
		request := httptest.NewRequest(http.MethodGet, "/models?client_version=0.146.0", nil)
		request.Header.Set("Authorization", authorization)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("authorization %q status = %d", authorization, recorder.Code)
		}
	}
	models, unexpected := audit.snapshot()
	if models != 0 || unexpected != 0 || nextCalls != 0 {
		t.Fatalf("unauthorised audit = models %d unexpected %d next %d", models, unexpected, nextCalls)
	}
}

func TestCodexInstalledHTTPRouteAuditLeavesServingHealthChallengeAvailable(t *testing.T) {
	audit, err := newCodexInstalledHTTPRouteAudit("0.146.0", testCodexInstalledLocalToken)
	if err != nil {
		t.Fatal(err)
	}
	nextCalls := 0
	handler := audit.guard(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		nextCalls++
		if request.Header.Get(ServingProofChallengeHeader) == "challenge" {
			writer.Header().Set(ServingProofResponseHeader, "proof")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.Header.Set(ServingProofChallengeHeader, "challenge")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	models, unexpected := audit.snapshot()
	if recorder.Code != http.StatusNoContent || recorder.Header().Get(ServingProofResponseHeader) != "proof" || nextCalls != 1 || models != 0 || unexpected != 0 {
		t.Fatalf("health result = status %d proof %q next %d counters %d/%d", recorder.Code, recorder.Header().Get(ServingProofResponseHeader), nextCalls, models, unexpected)
	}
}
