package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const defaultCodexPrimerRequestTimeout = time.Minute

type PrimerRequestState string

const (
	PrimerRequestRejected  PrimerRequestState = "rejected"
	PrimerRequestAdmitted  PrimerRequestState = "admitted"
	PrimerRequestAmbiguous PrimerRequestState = "ambiguous"
)

type PrimerRequestResultCode string

const (
	PrimerRequestCodeAuthRejected      PrimerRequestResultCode = "auth_rejected"
	PrimerRequestCodeHardLimit         PrimerRequestResultCode = "hard_limit"
	PrimerRequestCodeHTTPPreAdmission  PrimerRequestResultCode = "http_pre_admission"
	PrimerRequestCodeLifecycleObserved PrimerRequestResultCode = "lifecycle_observed"
	PrimerRequestCodeTransportError    PrimerRequestResultCode = "transport_error"
	PrimerRequestCodeTimeout           PrimerRequestResultCode = "timeout"
	PrimerRequestCodeResponseReadError PrimerRequestResultCode = "response_read_error"
	PrimerRequestCodeHTTPAmbiguous     PrimerRequestResultCode = "http_ambiguous"
	PrimerRequestCodeSSEMalformed      PrimerRequestResultCode = "sse_malformed"
	PrimerRequestCodeSSETruncated      PrimerRequestResultCode = "sse_truncated"
	PrimerRequestCodeLifecycleMissing  PrimerRequestResultCode = "lifecycle_missing"
)

type PrimerRequestResult struct {
	State      PrimerRequestState
	Code       PrimerRequestResultCode
	HTTPStatus int
}

type CodexPrimerRequester struct {
	Router       *CodexRequestRouter
	ResponsesURL string
	Timeout      time.Duration
}

func (r *CodexPrimerRequester) Send(ctx context.Context, account codex.AccountKey, modelID string) (PrimerRequestResult, error) {
	if r == nil || r.Router == nil || r.ResponsesURL == "" || account == "" || modelID == "" {
		return PrimerRequestResult{}, fmt.Errorf("Codex primer requester unavailable")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultCodexPrimerRequestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload := map[string]any{
		"model":        modelID,
		"instructions": "Reply with pong.",
		"input": []any{map[string]any{
			"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "ping"}},
		}},
		"tools":               []any{},
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"store":               false,
		"stream":              true,
		"client_metadata":     map[string]any{"cq.synthetic": "window-primer-v1"},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return PrimerRequestResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ResponsesURL, bytes.NewReader(body))
	if err != nil {
		return PrimerRequestResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	choice := RouteChoice{
		AccountKey: account, RequestedModel: modelID, EffectiveModel: modelID,
		RequiredBuckets: []CapacityBucket{CapacityBucketForModel(modelID)},
	}
	response, _, failure, err := r.Router.DoPinned(ctx, choice, req)
	if err != nil {
		code := PrimerRequestCodeTransportError
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = PrimerRequestCodeTimeout
		}
		return PrimerRequestResult{State: PrimerRequestAmbiguous, Code: code}, nil
	}
	if failure == CodexPinnedAuthFailure || failure == CodexPinnedHardLimit {
		status := 0
		if response != nil {
			status = response.StatusCode
			_, _ = readCodexPrimerResponse(response)
		}
		code := PrimerRequestCodeAuthRejected
		if failure == CodexPinnedHardLimit {
			code = PrimerRequestCodeHardLimit
		}
		return PrimerRequestResult{State: PrimerRequestRejected, Code: code, HTTPStatus: status}, nil
	}
	if response == nil || response.Body == nil {
		closeResponse(response)
		return PrimerRequestResult{State: PrimerRequestAmbiguous, Code: PrimerRequestCodeTransportError}, nil
	}
	status := response.StatusCode
	data, readErr := readCodexPrimerResponse(response)
	if readErr != nil {
		return PrimerRequestResult{State: PrimerRequestAmbiguous, Code: PrimerRequestCodeResponseReadError, HTTPStatus: status}, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if isCodexPrimerPreAdmissionStatus(response.StatusCode) {
			return PrimerRequestResult{State: PrimerRequestRejected, Code: PrimerRequestCodeHTTPPreAdmission, HTTPStatus: status}, nil
		}
		return PrimerRequestResult{State: PrimerRequestAmbiguous, Code: PrimerRequestCodeHTTPAmbiguous, HTTPStatus: status}, nil
	}
	code := classifyCodexPrimerLifecycle(data)
	if code == PrimerRequestCodeLifecycleObserved {
		return PrimerRequestResult{State: PrimerRequestAdmitted, Code: code, HTTPStatus: status}, nil
	}
	return PrimerRequestResult{State: PrimerRequestAmbiguous, Code: code, HTTPStatus: status}, nil
}

func readCodexPrimerResponse(response *http.Response) ([]byte, error) {
	if response == nil || response.Body == nil {
		closeResponse(response)
		return nil, fmt.Errorf("Codex primer response body unavailable")
	}
	defer response.Body.Close()
	return httputil.ReadBody(response.Body)
}

func isCodexPrimerPreAdmissionStatus(status int) bool {
	switch status {
	case http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func classifyCodexPrimerLifecycle(data []byte) PrimerRequestResultCode {
	parser := NewCodexSSEParser(codexSSEDefaultMaxEventBytes)
	for len(data) != 0 {
		chunkSize := bytes.IndexByte(data, '\n')
		if chunkSize < 0 {
			chunkSize = len(data)
		} else {
			chunkSize++
		}
		observations, err := parser.Feed(data[:chunkSize])
		for _, observation := range observations {
			if codexPrimerLifecycleObserved(observation) {
				return PrimerRequestCodeLifecycleObserved
			}
			if observation.ParseError != nil {
				return PrimerRequestCodeSSEMalformed
			}
		}
		if err != nil {
			return PrimerRequestCodeSSEMalformed
		}
		data = data[chunkSize:]
	}
	observations, err := parser.Finish()
	for _, observation := range observations {
		if codexPrimerLifecycleObserved(observation) {
			return PrimerRequestCodeLifecycleObserved
		}
		if observation.ParseError != nil {
			return PrimerRequestCodeSSEMalformed
		}
	}
	if errors.Is(err, ErrCodexSSETruncated) {
		return PrimerRequestCodeSSETruncated
	}
	if err != nil {
		return PrimerRequestCodeSSEMalformed
	}
	return PrimerRequestCodeLifecycleMissing
}

func codexPrimerLifecycleObserved(observation CodexSSEObservation) bool {
	if observation.Admits || observation.Kind == CodexSSECompleted {
		return true
	}
	return observation.Kind == CodexSSEError && (observation.Type == "response.failed" || observation.Type == "response.incomplete")
}
