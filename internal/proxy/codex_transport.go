package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// CodexAttemptExecutor resolves secrets only inside attempt execution.
type CodexAttemptExecutor struct {
	Secrets   codex.SecretResolver
	Transport *CodexTokenTransport
}

// Do resolves exact candidate material and performs one request. It never selects or retries.
func (e *CodexAttemptExecutor) Do(ctx context.Context, choice RouteChoice, attempt CandidateAttempt, req *http.Request) (*http.Response, error) {
	if e == nil || e.Secrets == nil || e.Transport == nil {
		return nil, fmt.Errorf("Codex attempt executor unavailable")
	}
	if attempt.AccountKey == "" || attempt.Candidate.AccountKey != attempt.AccountKey || choice.AccountKey != attempt.AccountKey {
		return nil, fmt.Errorf("Codex attempt identity mismatch")
	}
	material, err := e.Secrets.Resolve(ctx, attempt.Candidate)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex credential candidate: %w", err)
	}
	if material.AccessToken == "" {
		return nil, fmt.Errorf("resolved Codex credential has no access token")
	}
	return e.Transport.Do(req, choice, material)
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
	if req == nil {
		return nil, fmt.Errorf("Codex request is nil")
	}
	out := shallowCloneRequest(req)
	rewriteCodexModelTo(out, choice.RequestedModel, choice.EffectiveModel)
	out.Header.Set("Authorization", "Bearer "+material.AccessToken)
	if material.AccountID != "" {
		out.Header.Set("ChatGPT-Account-ID", material.AccountID)
	} else {
		out.Header.Del("ChatGPT-Account-ID")
	}
	out.Header.Del("x-api-key")
	return t.inner().RoundTrip(out)
}

func rewriteCodexModelTo(req *http.Request, requestedModel, effectiveModel string) {
	if effectiveModel == "" || strings.EqualFold(ParseModel(effectiveModel), ParseModel(requestedModel)) || req.GetBody == nil {
		return
	}
	body, err := req.GetBody()
	if err != nil {
		return
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return
	}
	rewritten, ok := rewriteCodexModelBodyTo(data, effectiveModel)
	if !ok {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(rewritten))
	req.ContentLength = int64(len(rewritten))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(rewritten)), nil
	}
}

func rewriteCodexModelBodyTo(body []byte, effectiveModel string) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, false
	}
	if _, ok := payload["model"]; !ok {
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
	var model string
	if json.Unmarshal(rawModel, &model) != nil {
		return nil, false
	}
	rewrittenModel, ok := rewriteCodexModelName(model)
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

func isHardExhaustion(body []byte) bool {
	var parsed struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return false
	}
	return parsed.Error.Type == "insufficient_quota"
}

func makeBufferedResponse(orig *http.Response, body []byte) *http.Response {
	return &http.Response{
		Status:        orig.Status,
		StatusCode:    orig.StatusCode,
		Proto:         orig.Proto,
		ProtoMajor:    orig.ProtoMajor,
		ProtoMinor:    orig.ProtoMinor,
		Header:        orig.Header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       orig.Request,
	}
}
