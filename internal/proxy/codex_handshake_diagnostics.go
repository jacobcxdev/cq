package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
)

// CodexUpstreamErrorDetails contains only projected error metadata. Free-form
// messages, arbitrary headers and response bodies belong to opt-in payload
// capture, never ordinary diagnostics.
type CodexUpstreamErrorDetails struct {
	BodyTruncated bool   `json:"body_truncated,omitempty"`
	BodyKind      string `json:"body_kind"`
	BodyBytes     int    `json:"body_bytes"`
	ErrorType     string `json:"error_type,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	CFRay         string `json:"cf_ray,omitempty"`
}

var (
	codexHandshakeRequestID = regexp.MustCompile(`^(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|req_[0-9a-f]{32})$`)
	codexHandshakeCFRay     = regexp.MustCompile(`^[0-9a-f]{16}(?:-[A-Z]{3})?$`)
)

func projectCodexHandshakeError(response *http.Response, body []byte) *CodexUpstreamErrorDetails {
	if response == nil {
		return nil
	}
	// Gorilla retains at most 1024 rejection-body bytes before closing its
	// connection. Unknown-length captures at that boundary may be truncated.
	details := &CodexUpstreamErrorDetails{
		BodyBytes:     len(body),
		BodyTruncated: response.ContentLength > int64(len(body)) || response.ContentLength <= 0 && len(body) >= 1024,
	}
	for _, name := range []string{"X-Request-Id", "Openai-Request-Id", "Request-Id"} {
		if value := response.Header.Get(name); codexHandshakeRequestID.MatchString(value) {
			details.RequestID = value
			break
		}
	}
	if value := response.Header.Get("Cf-Ray"); codexHandshakeCFRay.MatchString(value) {
		details.CFRay = value
	}
	if len(body) > codexAttemptResponseLimit || response.ContentLength > codexAttemptResponseLimit {
		details.BodyTruncated = true
		details.BodyKind = "oversized"
		return details
	}
	decoded, complete := decodeCodexErrorResponseBody(body, response.Header, response.Uncompressed)
	if !complete {
		details.BodyKind = "undecodable"
		return details
	}
	trimmed := bytes.TrimSpace(decoded)
	switch {
	case len(trimmed) == 0:
		details.BodyKind = "empty"
	case json.Valid(trimmed):
		details.BodyKind = "json"
		// Reuse strict duplicate-field and shape validation before projecting symbols.
		wrapped, err := parseCodexHTTPError(trimmed, response.StatusCode)
		if err == nil && wrapped.Found && !details.BodyTruncated {
			details.ErrorType = projectCodexHandshakeErrorType(wrapped.ErrorType)
			details.ErrorCode = projectCodexHandshakeErrorCode(wrapped.Code)
		}
	case bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<!doctype html")) || bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<html")):
		details.BodyKind = "html"
	default:
		details.BodyKind = "text"
	}
	return details
}

func projectCodexHandshakeErrorType(value string) string {
	switch value {
	case "api_error", "authentication_error", "permission_error", "invalid_request_error", "rate_limit_error", "usage_limit_reached", "server_error", "access_denied":
		return value
	default:
		return ""
	}
}

func projectCodexHandshakeErrorCode(value string) string {
	switch value {
	case "access_denied", "permission_denied", "token_expired", "invalid_api_key", "invalid_token", "account_deactivated", "invalid_workspace_selected", "access_program_not_enabled", "unsupported_country_region_territory", "rate_limit_exceeded", "usage_limit_reached", "insufficient_quota", "content_policy_violation", "policy_violation", "model_not_found":
		return value
	default:
		return ""
	}
}

func safeCodexUpstreamErrorDetails(details *CodexUpstreamErrorDetails) bool {
	if details == nil {
		return true
	}
	switch details.BodyKind {
	case "empty", "json", "html", "text", "oversized", "undecodable":
	default:
		return false
	}
	return details.BodyBytes >= 0 &&
		details.ErrorType == projectCodexHandshakeErrorType(details.ErrorType) &&
		details.ErrorCode == projectCodexHandshakeErrorCode(details.ErrorCode) &&
		(details.RequestID == "" || codexHandshakeRequestID.MatchString(details.RequestID)) &&
		(details.CFRay == "" || codexHandshakeCFRay.MatchString(details.CFRay))
}
