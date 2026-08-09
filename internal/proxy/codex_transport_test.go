package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/klauspost/compress/zstd"
)

type codexTransportRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexTransportRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// multiCodexSelector supports exclusion filtering in legacy handler tests.
type multiCodexSelector struct {
	accounts []codex.CodexAccount
}

func (s *multiCodexSelector) Select(_ context.Context, exclude ...codex.SelectionExclusion) (*codex.CodexAccount, error) {
	excludedAccounts := make(map[codex.AccountKey]bool, len(exclude))
	excludedCandidates := make(map[codex.CandidateID]bool, len(exclude))
	for _, exclusion := range exclude {
		excludedAccounts[exclusion.AccountKey] = true
		excludedCandidates[exclusion.CandidateID] = true
	}
	for i := range s.accounts {
		a := &s.accounts[i]
		if codexAcctExcluded(a, excludedAccounts, excludedCandidates) || a.AccessToken == "" {
			continue
		}
		result := *a
		return &result, nil
	}
	return nil, fmt.Errorf("no codex accounts available")
}

func makeCodexRequest(body string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
	return req
}

func makeCodexBytesRequest(body []byte) *http.Request {
	original := bytes.Clone(body)
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(original))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(original)), nil
	}
	req.ContentLength = int64(len(original))
	return req
}

var (
	handlerAcceptedRewriteOnce sync.Once
	handlerAcceptedRewriteBody []byte
	handlerAcceptedRewriteZstd []byte
)

func handlerAcceptedRewriteFixtures(t *testing.T) ([]byte, []byte) {
	t.Helper()
	handlerAcceptedRewriteOnce.Do(func() {
		prefix := []byte(`{"model":"gpt-5.3-codex-spark","input":"`)
		suffix := []byte(`"}`)
		decodedBytes := DefaultCodexZstdLimits.MaxDecodedBytes + 1024
		noise := make([]byte, decodedBytes-len(prefix)-len(suffix))
		const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
		state := uint64(0x9e3779b97f4a7c15)
		for index := range noise {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			noise[index] = alphabet[state&63]
		}
		handlerAcceptedRewriteBody = append(prefix, noise...)
		handlerAcceptedRewriteBody = append(handlerAcceptedRewriteBody, suffix...)
		handlerAcceptedRewriteZstd = encodeCodexZstd(t, handlerAcceptedRewriteBody)
	})
	if len(handlerAcceptedRewriteBody) <= DefaultCodexZstdLimits.MaxDecodedBytes || len(handlerAcceptedRewriteBody) > maxRequestBody {
		t.Fatalf("decoded fixture bytes = %d, want (%d, %d]", len(handlerAcceptedRewriteBody), DefaultCodexZstdLimits.MaxDecodedBytes, maxRequestBody)
	}
	if len(handlerAcceptedRewriteZstd) <= DefaultCodexZstdLimits.MaxEncodedBytes || len(handlerAcceptedRewriteZstd) > maxRequestBody {
		t.Fatalf("zstd fixture bytes = %d, want (%d, %d]", len(handlerAcceptedRewriteZstd), DefaultCodexZstdLimits.MaxEncodedBytes, maxRequestBody)
	}
	return handlerAcceptedRewriteBody, handlerAcceptedRewriteZstd
}

func TestCodexTokenTransportInjectsExplicitMaterial(t *testing.T) {
	var gotAuth, gotAccount, gotAPIKey string
	transport := &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		gotAccount = req.Header.Get("ChatGPT-Account-ID")
		gotAPIKey = req.Header.Get("x-api-key")
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	req := makeCodexRequest(`{"model":"gpt-5.4"}`)
	req.Header.Set("Authorization", "Bearer caller")
	req.Header.Set("x-api-key", "caller-key")
	choice := RouteChoice{AccountKey: "identity", RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4"}
	_, err := transport.Do(req, choice, codex.CredentialMaterial{AccessToken: "secret", AccountID: "account"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" || gotAccount != "account" || gotAPIKey != "" {
		t.Fatalf("headers auth=%q account=%q api-key=%q", gotAuth, gotAccount, gotAPIKey)
	}
	if req.Header.Get("Authorization") != "Bearer caller" || req.Header.Get("x-api-key") != "caller-key" {
		t.Fatal("caller request was mutated")
	}
}

func TestCodexTokenTransportConsumesRouteModel(t *testing.T) {
	var body string
	transport := &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(req.Body)
		body = string(data)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	choice := RouteChoice{AccountKey: "identity", RequestedModel: codexSparkModel, EffectiveModel: codexFallbackModel}
	_, err := transport.Do(makeCodexRequest(`{"model":"gpt-5.3-codex-spark","input":[]}`), choice, codex.CredentialMaterial{AccessToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"model":"gpt-5.3-codex"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestCodexTokenTransportRewritesEncodedEffectiveModel(t *testing.T) {
	originalJSON := []byte("{\n  \"input\" : [{\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"hello\"}]}],\n  \"model\" : \"gpt-5.3-codex-spark\",\n  \"metadata\" : {\"trace\":\"kept\"},\n  \"include\" : [\"reasoning.encrypted_content\"]\n}\n")
	encoded := encodeCodexZstd(t, originalJSON)
	req := makeCodexBytesRequest(encoded)
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("Content-Length", strconv.Itoa(len(encoded)))

	type capturedRequest struct {
		body                []byte
		getBody             []byte
		contentEncoding     string
		contentLength       int64
		headerContentLength string
	}
	var captured []capturedRequest
	transport := &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(out *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(out.Body)
		if err != nil {
			return nil, err
		}
		replay, err := out.GetBody()
		if err != nil {
			return nil, err
		}
		getBody, err := io.ReadAll(replay)
		_ = replay.Close()
		if err != nil {
			return nil, err
		}
		captured = append(captured, capturedRequest{
			body:                body,
			getBody:             getBody,
			contentEncoding:     out.Header.Get("Content-Encoding"),
			contentLength:       out.ContentLength,
			headerContentLength: out.Header.Get("Content-Length"),
		})
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	choice := RouteChoice{AccountKey: "identity", RequestedModel: codexSparkModel, EffectiveModel: codexFallbackModel}
	for range 2 {
		if _, err := transport.Do(req, choice, codex.CredentialMaterial{AccessToken: "secret"}); err != nil {
			t.Fatal(err)
		}
	}

	if len(captured) != 2 {
		t.Fatalf("attempts = %d, want 2", len(captured))
	}
	for i, got := range captured {
		if got.contentEncoding != "zstd" {
			t.Errorf("attempt %d Content-Encoding = %q", i, got.contentEncoding)
		}
		if got.contentLength != int64(len(got.body)) {
			t.Errorf("attempt %d ContentLength = %d, body = %d", i, got.contentLength, len(got.body))
		}
		if got.headerContentLength != strconv.Itoa(len(got.body)) {
			t.Errorf("attempt %d Content-Length = %q, body = %d", i, got.headerContentLength, len(got.body))
		}
		if !bytes.Equal(got.getBody, got.body) {
			t.Errorf("attempt %d GetBody differs from Body", i)
		}
		decoded, err := DecodeCodexRequest(got.body, got.contentEncoding, DefaultCodexZstdLimits)
		if err != nil {
			t.Fatalf("attempt %d decode: %v", i, err)
		}
		var wantPayload, gotPayload map[string]any
		if err := json.Unmarshal(originalJSON, &wantPayload); err != nil {
			t.Fatal(err)
		}
		wantPayload["model"] = codexFallbackModel
		if err := json.Unmarshal(decoded.Decoded(), &gotPayload); err != nil {
			t.Fatalf("attempt %d JSON: %v", i, err)
		}
		if !reflect.DeepEqual(gotPayload, wantPayload) {
			t.Errorf("attempt %d payload = %#v, want %#v", i, gotPayload, wantPayload)
		}
	}
	if !bytes.Equal(captured[0].body, captured[1].body) {
		t.Fatal("encoded retries differ")
	}
	callerBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	callerReplay, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	callerGetBody, err := io.ReadAll(callerReplay)
	_ = callerReplay.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(callerBody, encoded) || !bytes.Equal(callerGetBody, encoded) {
		t.Fatal("caller body or GetBody was mutated")
	}
	if req.ContentLength != int64(len(encoded)) || req.Header.Get("Content-Length") != strconv.Itoa(len(encoded)) || req.Header.Get("Content-Encoding") != "zstd" {
		t.Fatalf("caller framing changed: ContentLength=%d headers=%v", req.ContentLength, req.Header)
	}
}

func TestCodexTokenTransportAcceptsLowExpansionFrameWithLargeWindow(t *testing.T) {
	body := codexLargeWindowRewriteBody(23 << 10)
	encoded := encodeCodexZstdStreamingWindow(t, body, 2<<20)
	assertCodexLargeWindowFixture(t, encoded, body)

	upstreamCalls := 0
	var upstreamBody []byte
	transport := &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		upstreamCalls++
		var err error
		upstreamBody, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	request := makeCodexBytesRequest(encoded)
	request.Header.Set("Content-Encoding", "zstd")
	choice := RouteChoice{AccountKey: "identity", RequestedModel: codexSparkModel, EffectiveModel: codexFallbackModel}
	response, err := transport.Do(request, choice, codex.CredentialMaterial{AccessToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls)
	}
	decoded, err := DecodeCodexRequest(upstreamBody, "zstd", codexTransportRewriteLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(decoded.Decoded(), []byte(`"model":"`+codexFallbackModel+`"`)) {
		t.Fatal("upstream model was not rewritten")
	}
}

func TestCodexTokenTransportRejectsLargeWindowExpansionBeforeDispatch(t *testing.T) {
	body := codexLargeWindowRewriteBody(2 << 20)
	encoded := encodeCodexZstdStreamingWindow(t, body, 2<<20)
	if len(body) <= len(encoded)*DefaultCodexZstdLimits.MaxExpansion {
		t.Fatalf("fixture expansion = %d/%d, want over %d", len(body), len(encoded), DefaultCodexZstdLimits.MaxExpansion)
	}

	upstreamCalls := 0
	transport := &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls++
		return nil, errors.New("unexpected upstream dispatch")
	})}
	request := makeCodexBytesRequest(encoded)
	request.Header.Set("Content-Encoding", "zstd")
	choice := RouteChoice{AccountKey: "identity", RequestedModel: codexSparkModel, EffectiveModel: codexFallbackModel}
	_, err := transport.Do(request, choice, codex.CredentialMaterial{AccessToken: "secret"})
	if err == nil || !strings.Contains(err.Error(), "Codex zstd expansion ratio exceeds limit") {
		t.Fatalf("error = %v, want expansion limit", err)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func codexLargeWindowRewriteBody(repeatedBytes int) []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
	noise := make([]byte, 13<<10)
	state := uint64(0x9e3779b97f4a7c15)
	for index := range noise {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		noise[index] = alphabet[state&63]
	}
	body := []byte(`{"model":"gpt-5.3-codex-spark","input":"`)
	body = append(body, noise...)
	body = append(body, bytes.Repeat([]byte("x"), repeatedBytes)...)
	return append(body, []byte(`"}`)...)
}

func assertCodexLargeWindowFixture(t *testing.T, encoded, decoded []byte) {
	t.Helper()
	var header zstd.Header
	if err := header.Decode(encoded); err != nil {
		t.Fatal(err)
	}
	if header.HasFCS || header.WindowSize != 2<<20 {
		t.Fatalf("frame FCS=%v window=%d, want false/%d", header.HasFCS, header.WindowSize, 2<<20)
	}
	decodeLimit, ratioLimited := codexZstdDecodeLimit(len(encoded), codexTransportRewriteLimits())
	if !ratioLimited || len(decoded) > decodeLimit || decodeLimit >= int(header.WindowSize) {
		t.Fatalf("fixture encoded=%d decoded=%d decode_limit=%d window=%d", len(encoded), len(decoded), decodeLimit, header.WindowSize)
	}
}

func TestCodexTokenTransportPreservesIdentityBodyWithoutRewrite(t *testing.T) {
	original := []byte(" { \"model\" : \"gpt-5.4\", \"input\" : [ ] } \n")
	req := makeCodexBytesRequest(original)
	req.Header.Set("Content-Encoding", "identity")
	req.Header.Set("Content-Length", "caller-length")
	var gotBody, gotReplay []byte
	var gotEncoding, gotHeaderLength string
	var gotLength int64
	transport := &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(out *http.Request) (*http.Response, error) {
		var err error
		gotBody, err = io.ReadAll(out.Body)
		if err != nil {
			return nil, err
		}
		replay, err := out.GetBody()
		if err != nil {
			return nil, err
		}
		gotReplay, err = io.ReadAll(replay)
		_ = replay.Close()
		gotEncoding = out.Header.Get("Content-Encoding")
		gotHeaderLength = out.Header.Get("Content-Length")
		gotLength = out.ContentLength
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, err
	})}
	choice := RouteChoice{AccountKey: "identity", RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4"}
	if _, err := transport.Do(req, choice, codex.CredentialMaterial{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, original) || !bytes.Equal(gotReplay, original) {
		t.Fatal("identity no-rewrite body changed")
	}
	if gotEncoding != "identity" || gotLength != int64(len(original)) || gotHeaderLength != "caller-length" {
		t.Fatalf("framing = encoding %q ContentLength %d Content-Length %q", gotEncoding, gotLength, gotHeaderLength)
	}
	callerBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(callerBody, original) || req.Header.Get("Content-Length") != "caller-length" {
		t.Fatal("caller request changed")
	}
}

func TestCodexTokenTransportIdentityRewriteUpdatesFraming(t *testing.T) {
	original := []byte(`{"model":"gpt-5.3-codex-spark","input":[]}`)
	req := makeCodexBytesRequest(original)
	req.Header.Set("Content-Encoding", "identity")
	req.Header.Set("Content-Length", strconv.Itoa(len(original)))
	var gotBody, gotReplay []byte
	var gotHeaderLength string
	var gotLength int64
	transport := &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(out *http.Request) (*http.Response, error) {
		var err error
		gotBody, err = io.ReadAll(out.Body)
		if err != nil {
			return nil, err
		}
		replay, err := out.GetBody()
		if err != nil {
			return nil, err
		}
		gotReplay, err = io.ReadAll(replay)
		_ = replay.Close()
		gotHeaderLength = out.Header.Get("Content-Length")
		gotLength = out.ContentLength
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, err
	})}
	choice := RouteChoice{AccountKey: "identity", RequestedModel: codexSparkModel, EffectiveModel: codexFallbackModel}
	if _, err := transport.Do(req, choice, codex.CredentialMaterial{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, gotReplay) {
		t.Fatal("rewritten Body and GetBody differ")
	}
	if gotLength != int64(len(gotBody)) || gotHeaderLength != strconv.Itoa(len(gotBody)) {
		t.Fatalf("framing = ContentLength %d Content-Length %q body %d", gotLength, gotHeaderLength, len(gotBody))
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil || payload.Model != codexFallbackModel {
		t.Fatalf("payload = %q error=%v", gotBody, err)
	}
	callerBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(callerBody, original) || req.ContentLength != int64(len(original)) || req.Header.Get("Content-Length") != strconv.Itoa(len(original)) {
		t.Fatal("caller request changed")
	}
}

func TestCodexTokenTransportStopsBeforeInnerOnRewriteFailure(t *testing.T) {
	malformedZstd := []byte("not a zstd frame")
	tests := []struct {
		name     string
		body     []byte
		encoding string
		mutate   func(*http.Request)
		wantErr  string
	}{
		{name: "unsupported encoding", body: []byte(`{"model":"gpt-5.3-codex-spark"}`), encoding: "gzip"},
		{name: "malformed zstd", body: malformedZstd, encoding: "zstd"},
		{name: "malformed JSON", body: []byte(`{"model":`), encoding: "identity"},
		{name: "missing model", body: []byte(`{"input":[]}`), encoding: "identity"},
		{name: "non-string model", body: []byte(`{"model":42}`), encoding: "identity"},
		{name: "encoded over handler limit", body: bytes.Repeat([]byte("x"), maxRequestBody+1), encoding: "identity", wantErr: "Codex encoded request exceeds limit"},
		{
			name:     "rewritten body exceeds handler limit",
			body:     []byte(`{"model":"gpt-5.3-codex-spark","input":"` + strings.Repeat("&", maxRequestBody/6+1) + `"}`),
			encoding: "identity",
			wantErr:  "prepare Codex model rewrite: Codex decoded request exceeds limit",
		},
		{name: "duplicate identity encoding", body: []byte(`{"model":"gpt-5.3-codex-spark"}`), encoding: "identity", mutate: func(req *http.Request) {
			req.Header.Add("Content-Encoding", "identity")
		}},
		{name: "conflicting encodings", body: []byte(`{"model":"gpt-5.3-codex-spark"}`), encoding: "identity", mutate: func(req *http.Request) {
			req.Header.Add("Content-Encoding", "zstd")
		}},
		{name: "GetBody error", body: []byte(`{"model":"gpt-5.3-codex-spark"}`), encoding: "identity", mutate: func(req *http.Request) {
			req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("replay failed") }
		}},
		{name: "body is not replayable", body: []byte(`{"model":"gpt-5.3-codex-spark"}`), encoding: "identity", mutate: func(req *http.Request) {
			req.GetBody = nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := makeCodexBytesRequest(tc.body)
			req.Header.Set("Content-Encoding", tc.encoding)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			innerCalls := 0
			transport := &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(*http.Request) (*http.Response, error) {
				innerCalls++
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
			})}
			choice := RouteChoice{AccountKey: "identity", RequestedModel: codexSparkModel, EffectiveModel: codexFallbackModel}
			_, err := transport.Do(req, choice, codex.CredentialMaterial{AccessToken: "secret"})
			if err == nil {
				t.Fatal("expected rewrite error")
			}
			if tc.wantErr != "" && err.Error() != tc.wantErr {
				t.Fatalf("error = %q, want %q", err, tc.wantErr)
			}
			if innerCalls != 0 {
				t.Fatalf("inner calls = %d, want 0", innerCalls)
			}
		})
	}
}

func TestCodexAttemptExecutorRejectsIdentityMismatchBeforeSecretResolution(t *testing.T) {
	resolver := &testSecretResolver{materials: map[codex.CandidateID]codex.CredentialMaterial{"candidate": {AccessToken: "secret"}}}
	executor := &CodexAttemptExecutor{Secrets: resolver, Transport: &CodexTokenTransport{}}
	_, err := executor.Do(context.Background(), RouteChoice{AccountKey: "one"}, CandidateAttempt{
		AccountKey: "two",
		Candidate:  codex.CandidateRef{AccountKey: "two", CandidateID: "candidate"},
	}, makeCodexRequest(`{"model":"gpt-5.4"}`))
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("err = %v", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("secret resolutions = %d", resolver.calls)
	}
}

func TestCodexAttemptExecutorRelistsSameIdentityRevisionBeforeDispatch(t *testing.T) {
	identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
	ref := codex.CandidateRef{AccountKey: "identity", CandidateID: "candidate"}
	inventory := staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{{
		Key: "identity", Identity: identity, Routable: true,
		Candidates: []codex.CredentialCandidate{{
			Ref: ref, Revision: "revision-new", Source: codex.SourceSystem, Routable: true,
		}},
	}}}}
	resolver := &testExactSecretResolver{
		errors: map[codex.Revision]error{"revision-old": codex.ErrStaleRevision},
		materials: map[codex.Revision]codex.CredentialMaterial{
			"revision-new": testExactCredentialMaterial(identity, "rotated-secret"),
		},
	}
	var authorization string
	executor := &CodexAttemptExecutor{
		Inventory: inventory,
		Secrets:   resolver,
		Transport: &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			authorization = request.Header.Get("Authorization")
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
		})},
	}
	attempt := CandidateAttempt{
		AccountKey: "identity", Candidate: ref, Revision: "revision-old",
		Source: codex.SourceSystem, Identity: identity, Ordinal: 1,
	}
	var dispatched CandidateAttempt
	response, actual, err := executor.doOnDispatch(
		context.Background(), RouteChoice{AccountKey: "identity"}, attempt,
		makeCodexRequest(`{"model":"gpt-5.4"}`),
		func(resolved CandidateAttempt) { dispatched = resolved },
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if authorization != "Bearer rotated-secret" {
		t.Fatalf("authorization = %q", authorization)
	}
	if actual.Revision != "revision-new" || dispatched.Revision != "revision-new" {
		t.Fatalf("actual revision = %q, dispatched revision = %q", actual.Revision, dispatched.Revision)
	}
	if actual.Candidate != ref || actual.Source != codex.SourceSystem || actual.Identity != identity || actual.Ordinal != attempt.Ordinal {
		t.Fatalf("actual attempt = %+v", actual)
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if len(resolver.plans) != 2 || resolver.plans[0].Revision != "revision-old" || resolver.plans[1].Revision != "revision-new" {
		t.Fatalf("resolved plans = %+v", resolver.plans)
	}
}

func TestCodexAttemptExecutorRejectsUnsafeRelistBeforeDispatch(t *testing.T) {
	identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
	ref := codex.CandidateRef{AccountKey: "identity", CandidateID: "candidate"}
	attempt := CandidateAttempt{
		AccountKey: "identity", Candidate: ref, Revision: "revision-old",
		Source: codex.SourceSystem, Identity: identity, Ordinal: 1,
	}
	for name, inventory := range testRejectedReplanInventories(ref, identity) {
		t.Run(name, func(t *testing.T) {
			resolver := &testExactSecretResolver{
				errors:    map[codex.Revision]error{"revision-old": codex.ErrStaleRevision},
				materials: make(map[codex.Revision]codex.CredentialMaterial),
			}
			upstreamCalls := 0
			executor := &CodexAttemptExecutor{
				Inventory: staticCredentialInventory{inventory: inventory},
				Secrets:   resolver,
				Transport: &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(*http.Request) (*http.Response, error) {
					upstreamCalls++
					return nil, errors.New("unexpected upstream dispatch")
				})},
			}
			_, err := executor.Do(context.Background(), RouteChoice{AccountKey: "identity"}, attempt, makeCodexRequest(`{"model":"gpt-5.4"}`))
			if !errors.Is(err, codex.ErrStaleRevision) {
				t.Fatalf("error = %v, want ErrStaleRevision", err)
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d", upstreamCalls)
			}
			resolver.mu.Lock()
			plans := len(resolver.plans)
			resolver.mu.Unlock()
			if plans != 1 {
				t.Fatalf("exact resolutions = %d, want 1", plans)
			}
		})
	}
}

func TestRewriteCodexModelNamePreservesSuffix(t *testing.T) {
	got, ok := rewriteCodexModelName("gpt-5.3-codex-spark-high")
	if !ok || got != "gpt-5.3-codex-high" {
		t.Fatalf("rewrite = %q, %v", got, ok)
	}
}
