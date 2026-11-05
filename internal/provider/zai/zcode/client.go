package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const balanceURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"
const resetStatusURL = "https://zcode.z.ai/api/v1/coding-plan/reset/status"

// Client exposes ZCode account operations. Version identifies
// the ZCode protocol version used by the caller's integration.
type Client struct {
	HTTP      httputil.Doer
	Version   string
	DeviceMid string
	Now       func() time.Time
}

// Error excludes remote messages and transport errors, which may contain tokens.
type Error struct {
	Code       string
	HTTPStatus int
	VendorCode int
}

func (e *Error) Error() string { return fmt.Sprintf("ZCode status request failed: %s", e.Code) }

// Balance returns the provider's raw quantities and expiry metadata. Missing,
// null and string-valued quantities do not become fabricated percentages.
func (c Client) Balance(ctx context.Context, session Session) (Balance, error) {
	endpoint := balanceURL + "?" + url.Values{"app_version": {c.Version}}.Encode()
	data, headers, err := c.request(ctx, endpoint, session, false)
	if err != nil {
		return Balance{}, err
	}
	var result Balance
	if json.Unmarshal(data, &result) != nil || !result.validScalars() {
		return Balance{}, &Error{Code: "invalid_balance"}
	}
	if at, ok := unixSeconds(result.ServerTime); ok {
		result.ReferenceTime = at
	} else if at, err := http.ParseTime(headers.Get("Date")); err == nil {
		result.ReferenceTime = at
	} else if c.Now != nil {
		result.ReferenceTime = c.Now()
	} else {
		result.ReferenceTime = time.Now()
	}
	return result, nil
}

func (c Client) ResetStatus(ctx context.Context, session Session) (ResetStatus, error) {
	data, _, err := c.request(ctx, resetStatusURL, session, true)
	if err != nil {
		return ResetStatus{}, err
	}
	var decoded struct {
		FiveHour *[]ResetOpportunity `json:"available_five_hour_resets"`
		Week     *[]ResetOpportunity `json:"available_week_resets"`
		FiveLast json.RawMessage     `json:"latest_five_hour_reset_history"`
		WeekLast json.RawMessage     `json:"latest_week_reset_history"`
		Unread   *bool               `json:"has_unread_history"`
	}
	if json.Unmarshal(data, &decoded) != nil || decoded.FiveHour == nil || decoded.Week == nil || decoded.Unread == nil {
		return ResetStatus{}, &Error{Code: "invalid_reset_status"}
	}
	result := ResetStatus{AvailableFiveHourResets: *decoded.FiveHour, AvailableWeekResets: *decoded.Week, HasUnreadHistory: *decoded.Unread}
	for _, group := range [][]ResetOpportunity{result.AvailableFiveHourResets, result.AvailableWeekResets} {
		for _, opportunity := range group {
			if !positiveNumber(opportunity.ExpireAt) {
				return ResetStatus{}, &Error{Code: "invalid_reset_status"}
			}
		}
	}
	for _, history := range []struct {
		raw json.RawMessage
		dst **ResetHistory
	}{{decoded.FiveLast, &result.LatestFiveHourResetHistory}, {decoded.WeekLast, &result.LatestWeekResetHistory}} {
		if len(history.raw) == 0 || json.Unmarshal(history.raw, history.dst) != nil || *history.dst != nil && !positiveNumber((*history.dst).UsedAt) {
			return ResetStatus{}, &Error{Code: "invalid_reset_status"}
		}
	}
	return result, nil
}

func (c Client) request(ctx context.Context, endpoint string, session Session, reset bool) ([]byte, http.Header, error) {
	if c.HTTP == nil || !validHeaderValue(c.Version, 80) || !validHeaderValue(c.DeviceMid, 256) {
		return nil, nil, &Error{Code: "client_unavailable"}
	}
	if session.Family != "zai" || !validHeaderValue(session.ZCodeJWT, 16<<10) || reset && !validHeaderValue(session.BusinessJWT, 16<<10) {
		return nil, nil, &Error{Code: "session_unavailable"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, &Error{Code: "invalid_request"}
	}
	req.Header.Set("Authorization", "Bearer "+session.ZCodeJWT)
	req.Header.Set("Accept", "application/json")
	SetSourceHeaders(req.Header, c.Version)
	req.Header.Set("X-Device-Mid", c.DeviceMid)
	if reset {
		req.Header.Set("X-Bigmodel-Authorization", session.BusinessJWT)
		req.Header.Set("Bigmodel-Target-Type", "PERSONAL")
	}
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
		return nil, nil, &Error{Code: "request_failed"}
	}
	if response == nil || response.Body == nil {
		return nil, nil, &Error{Code: "invalid_response"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, &Error{Code: "http_error", HTTPStatus: response.StatusCode}
	}
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil, nil, &Error{Code: "read_failed"}
	}
	var envelope struct {
		Code    *int            `json:"code"`
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code == nil || len(envelope.Data) == 0 || envelope.Data[0] != '{' {
		return nil, nil, &Error{Code: "invalid_envelope"}
	}
	if *envelope.Code != 0 || envelope.Success != nil && !*envelope.Success {
		return nil, nil, &Error{Code: "api_error", VendorCode: *envelope.Code}
	}
	return envelope.Data, response.Header, nil
}

func positiveNumber(number json.Number) bool {
	value, err := number.Float64()
	return err == nil && value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func unixSeconds(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || raw[0] == '"' || string(raw) == "null" {
		return time.Time{}, false
	}
	var number json.Number
	if json.Unmarshal(raw, &number) != nil {
		return time.Time{}, false
	}
	seconds, err := number.Float64()
	if err != nil || seconds < 0 || seconds > 253402300799 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return time.Time{}, false
	}
	whole, fraction := math.Modf(seconds)
	return time.Unix(int64(whole), int64(fraction*1e9)), true
}

func scalar(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	return raw[0] == '"' || raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9' || strings.TrimSpace(string(raw)) == "null"
}

// SetSourceHeaders applies the ZCode source identity used by its account APIs.
// Authentication and scope headers remain separate at each request boundary.
func SetSourceHeaders(header http.Header, version string) {
	header.Set("User-Agent", "ZCode/"+version)
	header.Set("X-Title", "Z Code@electron")
	header.Set("HTTP-Referer", "https://zcode.z.ai")
	header.Set("X-ZCode-App-Version", version)
	header.Set("X-Client-Language", "unknown")
	header.Set("X-Client-Timezone", "unknown")
	platform, arch := runtime.GOOS, runtime.GOARCH
	if platform == "windows" {
		platform = "win32"
	}
	switch arch {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "ia32"
	}
	header.Set("X-Platform", platform+"-"+arch)
	category := runtime.GOOS
	if category == "darwin" {
		category = "macos"
	}
	header.Set("X-Os-Category", category)
}
