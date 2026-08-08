package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const codexPrimerResponseLimit = 1 << 20
const defaultCodexPrimerRequestTimeout = time.Minute

type PrimerRequestState string

const (
	PrimerRequestRejected  PrimerRequestState = "rejected"
	PrimerRequestAdmitted  PrimerRequestState = "admitted"
	PrimerRequestAmbiguous PrimerRequestState = "ambiguous"
)

type PrimerRequestResult struct {
	State      PrimerRequestState
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
		return PrimerRequestResult{State: PrimerRequestAmbiguous}, nil
	}
	if failure == CodexPinnedAuthFailure || failure == CodexPinnedHardLimit {
		status := 0
		if response != nil {
			status = response.StatusCode
			closeResponse(response)
		}
		return PrimerRequestResult{State: PrimerRequestRejected, HTTPStatus: status}, nil
	}
	if response == nil || response.Body == nil {
		return PrimerRequestResult{State: PrimerRequestAmbiguous}, nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, codexPrimerResponseLimit))
		return PrimerRequestResult{State: PrimerRequestRejected, HTTPStatus: response.StatusCode}, nil
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, codexPrimerResponseLimit+1))
	if readErr != nil || len(data) > codexPrimerResponseLimit {
		return PrimerRequestResult{State: PrimerRequestAmbiguous, HTTPStatus: response.StatusCode}, nil
	}
	parser := NewCodexSSEParser(codexSSEDefaultMaxEventBytes)
	observations, parseErr := parser.Feed(data)
	if parseErr == nil {
		finished, err := parser.Finish()
		if err != nil {
			parseErr = err
		} else {
			observations = append(observations, finished...)
		}
	}
	if parseErr != nil {
		return PrimerRequestResult{State: PrimerRequestAmbiguous, HTTPStatus: response.StatusCode}, nil
	}
	for _, observation := range observations {
		if observation.Admits {
			return PrimerRequestResult{State: PrimerRequestAdmitted, HTTPStatus: response.StatusCode}, nil
		}
	}
	return PrimerRequestResult{State: PrimerRequestAmbiguous, HTTPStatus: response.StatusCode}, nil
}
