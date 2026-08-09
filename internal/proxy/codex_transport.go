package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	codexSparkModel    = "gpt-5.3-codex-spark"
	codexFallbackModel = "gpt-5.3-codex"
)

// ExplicitAccountExecutor executes one already-selected credential attempt.
type ExplicitAccountExecutor interface {
	Do(context.Context, RouteChoice, CandidateAttempt, *http.Request) (*http.Response, error)
}

type explicitAccountDispatchExecutor interface {
	doOnDispatch(context.Context, RouteChoice, CandidateAttempt, *http.Request, func(CandidateAttempt)) (*http.Response, CandidateAttempt, error)
}

// CodexAttemptExecutor resolves secrets only inside attempt execution.
type CodexAttemptExecutor struct {
	Inventory codex.CredentialInventory
	Secrets   codex.ExactSecretResolver
	Transport *CodexTokenTransport
}

// Do resolves exact candidate material and performs one request. It never selects or retries.
func (e *CodexAttemptExecutor) Do(ctx context.Context, choice RouteChoice, attempt CandidateAttempt, req *http.Request) (*http.Response, error) {
	response, _, err := e.doOnDispatch(ctx, choice, attempt, req, nil)
	return response, err
}

func (e *CodexAttemptExecutor) doOnDispatch(ctx context.Context, choice RouteChoice, attempt CandidateAttempt, req *http.Request, onDispatch func(CandidateAttempt)) (*http.Response, CandidateAttempt, error) {
	if e == nil || e.Secrets == nil || e.Transport == nil {
		return nil, attempt, fmt.Errorf("Codex attempt executor unavailable")
	}
	if attempt.AccountKey == "" || attempt.Candidate.AccountKey != attempt.AccountKey || choice.AccountKey != attempt.AccountKey {
		return nil, attempt, fmt.Errorf("Codex attempt identity mismatch")
	}
	material, resolved, err := codex.ResolvePlannedCandidate(ctx, e.Inventory, e.Secrets, attempt.plannedCandidate())
	actual := candidateAttemptFromPlan(resolved, attempt.Ordinal)
	if err != nil {
		return nil, actual, fmt.Errorf("resolve Codex credential candidate: %w", err)
	}
	if material.AccessToken == "" {
		return nil, actual, fmt.Errorf("resolved Codex credential has no access token")
	}
	dispatch := func() {
		if onDispatch != nil {
			onDispatch(actual)
		}
	}
	response, err := e.Transport.doOnDispatch(req, choice, material, dispatch)
	return response, actual, err
}

// CodexTokenTransport only clones requests, injects explicit credentials, and
// applies the already-selected model rewrite. It never selects or retries.
type CodexTokenTransport struct {
	Inner http.RoundTripper
}

func (t *CodexTokenTransport) inner() http.RoundTripper {
	if t != nil && t.Inner != nil {
		return t.Inner
	}
	return http.DefaultTransport
}

// Do performs one explicit request attempt.
func (t *CodexTokenTransport) Do(req *http.Request, choice RouteChoice, material codex.CredentialMaterial) (*http.Response, error) {
	return t.doOnDispatch(req, choice, material, nil)
}

func (t *CodexTokenTransport) doOnDispatch(req *http.Request, choice RouteChoice, material codex.CredentialMaterial, onDispatch func()) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("Codex request is nil")
	}
	out, err := cloneCodexTransportRequest(req)
	if err != nil {
		return nil, err
	}
	if err := rewriteCodexModelTo(out, choice.RequestedModel, choice.EffectiveModel); err != nil {
		return nil, err
	}
	out.Header.Set("Authorization", "Bearer "+material.AccessToken)
	if material.AccountID != "" {
		out.Header.Set("ChatGPT-Account-ID", material.AccountID)
	} else {
		out.Header.Del("ChatGPT-Account-ID")
	}
	out.Header.Del("x-api-key")
	inner := t.inner()
	if onDispatch != nil {
		onDispatch()
	}
	return inner.RoundTrip(out)
}

func cloneCodexTransportRequest(req *http.Request) (*http.Request, error) {
	out := new(http.Request)
	*out = *req
	out.Header = req.Header.Clone()
	if out.Header == nil {
		out.Header = make(http.Header)
	}
	if req.URL != nil {
		u := *req.URL
		out.URL = &u
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("replay Codex request body: %w", err)
		}
		out.Body = body
		return out, nil
	}
	if req.Body == nil || req.Body == http.NoBody {
		return out, nil
	}
	return nil, errors.New("Codex request body is not replayable")
}

func rewriteCodexModelTo(req *http.Request, requestedModel, effectiveModel string) error {
	if effectiveModel == "" || strings.EqualFold(ParseModel(effectiveModel), ParseModel(requestedModel)) {
		return nil
	}
	if req.Body == nil {
		return errors.New("Codex request body is unavailable for model rewrite")
	}
	limits := codexTransportRewriteLimits()
	data, err := readBoundedCodexRequestBody(req.Body, limits.MaxEncodedBytes)
	if err != nil {
		return err
	}
	contentEncoding, err := parseCodexContentEncoding(req.Header)
	if err != nil {
		return fmt.Errorf("prepare Codex model rewrite: %w", err)
	}
	decoded, err := DecodeCodexRequest(data, contentEncoding, limits)
	if err != nil {
		return fmt.Errorf("prepare Codex model rewrite: %w", err)
	}
	rewritten, ok := rewriteCodexModelBodyTo(decoded.Decoded(), effectiveModel)
	if !ok {
		return errors.New("prepare Codex model rewrite: request has no string model")
	}
	prepared, err := EncodeCodexRequest(rewritten, decoded.Encoding(), limits)
	if err != nil {
		return fmt.Errorf("prepare Codex model rewrite: %w", err)
	}
	hadContentLength := false
	if req.Header != nil {
		_, hadContentLength = req.Header[http.CanonicalHeaderKey("Content-Length")]
	}
	prepared = bytes.Clone(prepared)
	req.Body = io.NopCloser(bytes.NewReader(prepared))
	req.ContentLength = int64(len(prepared))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(prepared)), nil
	}
	if hadContentLength {
		req.Header.Set("Content-Length", strconv.Itoa(len(prepared)))
	}
	switch decoded.Encoding() {
	case "":
		req.Header.Del("Content-Encoding")
	case "identity":
		req.Header.Set("Content-Encoding", "identity")
	case "zstd":
		req.Header.Set("Content-Encoding", "zstd")
	}
	return nil
}

func codexTransportRewriteLimits() CodexZstdLimits {
	// Native and compact handlers admit encoded and decoded bodies up to
	// maxRequestBody. Model rewriting must preserve that accepted envelope.
	limits := DefaultCodexZstdLimits
	limits.MaxEncodedBytes = maxRequestBody
	limits.MaxDecodedBytes = maxRequestBody
	return limits
}

func readBoundedCodexRequestBody(body io.ReadCloser, limit int) ([]byte, error) {
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read Codex request body for model rewrite: %w", err)
	}
	if len(data) > limit {
		return nil, errors.New("Codex encoded request exceeds limit")
	}
	return data, nil
}

func rewriteCodexModelBodyTo(body []byte, effectiveModel string) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, false
	}
	rawCurrent, ok := payload["model"]
	if !ok {
		return nil, false
	}
	var current *string
	if json.Unmarshal(rawCurrent, &current) != nil || current == nil {
		return nil, false
	}
	rawModel, err := json.Marshal(effectiveModel)
	if err != nil {
		return nil, false
	}
	payload["model"] = rawModel
	result, err := json.Marshal(payload)
	return result, err == nil
}

func rewriteCodexModelBody(body []byte) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, false
	}
	rawModel, ok := payload["model"]
	if !ok {
		return nil, false
	}
	var model *string
	if json.Unmarshal(rawModel, &model) != nil || model == nil {
		return nil, false
	}
	rewrittenModel, ok := rewriteCodexModelName(*model)
	if !ok {
		return nil, false
	}
	return rewriteCodexModelBodyTo(body, rewrittenModel)
}

func rewriteCodexModelName(model string) (string, bool) {
	normalised := ParseModel(model)
	lower := strings.ToLower(normalised)
	spark := strings.ToLower(codexSparkModel)
	if lower == spark {
		return codexFallbackModel, true
	}
	if strings.HasPrefix(lower, spark+"-") {
		suffix := normalised[len(codexSparkModel):]
		return codexFallbackModel + suffix, true
	}
	return "", false
}

func withCodexModelContext(req *http.Request) *http.Request {
	if req == nil || codexRequestedModel(req.Context()) != "" || req.GetBody == nil {
		return req
	}
	body, err := req.GetBody()
	if err != nil {
		return req
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return req
	}
	model := extractModel(data)
	if model == "" {
		return req
	}
	return req.WithContext(context.WithValue(req.Context(), codexModelContextKey{}, model))
}
