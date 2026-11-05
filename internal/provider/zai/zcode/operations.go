package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const resetOpportunityURL = "https://zcode.z.ai/api/v1/coding-plan/reset/opportunity"
const resetUseURL = "https://zcode.z.ai/api/v1/coding-plan/reset/use"

// OpportunityResult contains only the server's grant and next-attempt decision.
// NextTryAt is the documented next_try_at Unix-millisecond timestamp.
type OpportunityResult struct {
	Granted   bool       `json:"granted"`
	NextTryAt *time.Time `json:"next_try_at,omitempty"`
}

// OperationError excludes upstream messages and credentials. An ambiguous
// outcome must not be retried with a new idempotency key or treated as unused.
type OperationError struct {
	Code       string
	HTTPStatus int
	VendorCode int
	Ambiguous  bool
}

func (e *OperationError) Error() string {
	return fmt.Sprintf("ZCode operation failed: %s", e.Code)
}

// Opportunity explicitly asks the server to evaluate eligibility and may issue
// a reset card. Status reads never call it. Callers own scheduling and retain the
// same idempotency key when retrying an unresolved attempt.
func (c Client) Opportunity(ctx context.Context, session Session, idempotencyKey string) (OpportunityResult, error) {
	if !validHeaderValue(idempotencyKey, 64) {
		return OpportunityResult{}, &OperationError{Code: "invalid_idempotency_key"}
	}
	body, _ := json.Marshal(struct {
		Key string `json:"idempotency_key"`
	}{idempotencyKey})
	envelope, err := c.requestOperation(ctx, resetOpportunityURL, session, body)
	if err != nil {
		return OpportunityResult{}, err
	}
	var decoded struct {
		Granted   *bool           `json:"granted"`
		NextTryAt json.RawMessage `json:"next_try_at"`
	}
	if json.Unmarshal(envelope.Data, &decoded) != nil || decoded.Granted == nil {
		return OpportunityResult{}, &OperationError{Code: "invalid_opportunity", Ambiguous: true}
	}
	if *decoded.Granted {
		if *envelope.Code != 0 || len(decoded.NextTryAt) > 0 && string(decoded.NextTryAt) != "null" {
			return OpportunityResult{}, &OperationError{Code: "invalid_opportunity", Ambiguous: true}
		}
		return OpportunityResult{Granted: true}, nil
	}
	var millis int64
	if json.Unmarshal(decoded.NextTryAt, &millis) != nil || millis <= 0 || millis > 253402300799999 {
		return OpportunityResult{}, &OperationError{Code: "invalid_opportunity", Ambiguous: true}
	}
	next := time.UnixMilli(millis)
	return OpportunityResult{NextTryAt: &next}, nil
}

// UseReset consumes the explicitly selected reset type once. It neither selects
// another type nor refreshes status, claims packs or retries after a failure.
func (c Client) UseReset(ctx context.Context, session Session, resetType, idempotencyKey string) error {
	if resetType != "FIVE_HOUR" && resetType != "WEEK" {
		return &OperationError{Code: "invalid_reset_type"}
	}
	if !validHeaderValue(idempotencyKey, 64) {
		return &OperationError{Code: "invalid_idempotency_key"}
	}
	body, _ := json.Marshal(struct {
		Key  string `json:"idempotency_key"`
		Type string `json:"reset_type"`
	}{idempotencyKey, resetType})
	envelope, err := c.requestOperation(ctx, resetUseURL, session, body)
	if err != nil {
		return err
	}
	var result struct {
		Used *bool `json:"used"`
	}
	if *envelope.Code != 0 || json.Unmarshal(envelope.Data, &result) != nil || result.Used == nil || !*result.Used {
		return &OperationError{Code: "invalid_reset_use", Ambiguous: true}
	}
	return nil
}

type operationEnvelope struct {
	Code    *int            `json:"code"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

func (c Client) requestOperation(ctx context.Context, endpoint string, session Session, body []byte) (operationEnvelope, error) {
	if c.HTTP == nil || !validHeaderValue(c.Version, 80) || !validHeaderValue(c.DeviceMid, 256) {
		return operationEnvelope{}, &OperationError{Code: "client_unavailable"}
	}
	if session.Family != "zai" || !validHeaderValue(session.ZCodeJWT, 16<<10) || !validHeaderValue(session.BusinessJWT, 16<<10) {
		return operationEnvelope{}, &OperationError{Code: "session_unavailable"}
	}
	if ctx.Err() != nil {
		return operationEnvelope{}, &OperationError{Code: "cancelled"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return operationEnvelope{}, &OperationError{Code: "invalid_request"}
	}
	req.Header.Set("Authorization", "Bearer "+session.ZCodeJWT)
	req.Header.Set("X-Bigmodel-Authorization", session.BusinessJWT)
	req.Header.Set("Bigmodel-Target-Type", "PERSONAL")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+c.Version)
	req.Header.Set("X-Title", "Z Code@electron")
	req.Header.Set("HTTP-Referer", "https://zcode.z.ai")
	req.Header.Set("X-ZCode-App-Version", c.Version)
	req.Header.Set("X-App-Id", "zcode")
	req.Header.Set("X-Client-Version", c.Version)
	req.Header.Set("X-Device-Mid", c.DeviceMid)
	doer := c.HTTP
	if client, ok := doer.(*http.Client); ok {
		private := *client
		private.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		doer = &private
	}
	response, err := doer.Do(req)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return operationEnvelope{}, &OperationError{Code: "request_failed", Ambiguous: true}
	}
	if response == nil || response.Body == nil {
		return operationEnvelope{}, &OperationError{Code: "invalid_response", Ambiguous: true}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return operationEnvelope{}, &OperationError{
			Code: "http_error", HTTPStatus: response.StatusCode,
			Ambiguous: response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500,
		}
	}
	data, err := httputil.ReadBody(response.Body)
	if err != nil {
		return operationEnvelope{}, &OperationError{Code: "read_failed", Ambiguous: true}
	}
	var envelope operationEnvelope
	if json.Unmarshal(data, &envelope) != nil || envelope.Code == nil {
		return operationEnvelope{}, &OperationError{Code: "invalid_envelope", Ambiguous: true}
	}
	if *envelope.Code != 0 && *envelope.Code != 3301 || *envelope.Code == 0 && envelope.Success != nil && !*envelope.Success {
		return operationEnvelope{}, &OperationError{Code: "api_error", VendorCode: *envelope.Code, Ambiguous: true}
	}
	envelope.Data = bytes.TrimSpace(envelope.Data)
	if len(envelope.Data) == 0 || envelope.Data[0] != '{' {
		return operationEnvelope{}, &OperationError{Code: "invalid_envelope", Ambiguous: true}
	}
	return envelope, nil
}
