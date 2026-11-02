package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const resetListURL = "https://api.z.ai/api/biz/customer-package-reset/list?targetType=PERSONAL"
const resetUseURL = "https://api.z.ai/api/biz/customer-package-reset/use"

// ResetCredit describes one exact card from the personal Coding Plan inventory.
// Record identifiers retain their JSON scalar type when sent back to Z.ai.
type ResetCredit struct {
	ID         string     `json:"id"`
	Window     string     `json:"window"`
	Available  bool       `json:"available"`
	ExpireTime string     `json:"expire_time,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	recordID   json.RawMessage
	grantType  json.RawMessage
}

type ResetInventory struct {
	Credits []ResetCredit `json:"credits"`
}

// ResetError deliberately omits upstream messages and authentication data.
type ResetError struct {
	Code       string
	HTTPStatus int
	Ambiguous  bool
}

func (e *ResetError) Error() string {
	if e.Ambiguous {
		return "Z.ai reset outcome unresolved; no automatic retry"
	}
	return fmt.Sprintf("Z.ai reset request failed: %s", e.Code)
}

type ResetClient struct{ HTTP httputil.Doer }

// List reads the same personal reset inventory used by Z.ai's usage page.
func (c ResetClient) List(ctx context.Context, key string, now time.Time) (ResetInventory, error) {
	response, err := c.request(ctx, http.MethodGet, resetListURL, key, nil, false)
	if err != nil {
		return ResetInventory{}, err
	}
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Success bool            `json:"success"`
		Data    *struct {
			FiveHourResets []resetRecord `json:"fiveHourResets"`
			WeekResets     []resetRecord `json:"weekResets"`
		} `json:"data"`
	}
	if json.Unmarshal(response, &envelope) != nil || !successfulResetCode(envelope.Code) || !envelope.Success || envelope.Data == nil {
		return ResetInventory{}, &ResetError{Code: "invalid_inventory"}
	}
	inventory := ResetInventory{Credits: make([]ResetCredit, 0)}
	for _, group := range []struct {
		window  string
		records []resetRecord
	}{{"5h", envelope.Data.FiveHourResets}, {"7d", envelope.Data.WeekResets}} {
		for _, record := range group.records {
			if record.ExpireTime != "" {
				if _, ok := resetExpiry(record.ExpireTime); !ok {
					if _, err := time.Parse("2006-01-02 15:04:05", record.ExpireTime); err != nil {
						return ResetInventory{}, &ResetError{Code: "invalid_inventory"}
					}
				}
			}
			id := resetScalar(record.RecordID)
			credit := ResetCredit{Window: group.window, Available: record.Available, ExpireTime: record.ExpireTime, recordID: record.RecordID, grantType: record.GrantType}
			if id != "" {
				credit.ID = group.window + ":" + id
			}
			if expires, ok := resetExpiry(record.ExpireTime); ok {
				credit.ExpiresAt = &expires
				credit.Available = credit.Available && expires.After(now)
			}
			// A missing recordId must never become the vendor's implicit "first card" operation.
			credit.Available = credit.Available && credit.ID != ""
			inventory.Credits = append(inventory.Credits, credit)
		}
	}
	sort.SliceStable(inventory.Credits, func(i, j int) bool {
		a, b := inventory.Credits[i], inventory.Credits[j]
		if a.ExpiresAt != nil && b.ExpiresAt != nil {
			return a.ExpiresAt.Before(*b.ExpiresAt)
		}
		return a.ExpireTime < b.ExpireTime
	})
	return inventory, nil
}

type resetRecord struct {
	RecordID   json.RawMessage `json:"recordId"`
	GrantType  json.RawMessage `json:"grantType"`
	ExpireTime string          `json:"expireTime"`
	Available  bool            `json:"available"`
}

// Use submits one card once. Callers retain requestID across an unresolved outcome.
// Contract: Z.ai's usage-page client, retrieved 2026-11-02:
// https://static.bigmodel.cn/z-ai-website/_next/static/chunks/1bhh26gy0qltu.js
func (c ResetClient) Use(ctx context.Context, key string, credit ResetCredit, requestID string, now time.Time) error {
	if !credit.Available || resetScalar(credit.recordID) == "" || strings.TrimSpace(requestID) == "" || credit.ExpiresAt != nil && !credit.ExpiresAt.After(now) {
		return errors.New("Z.ai reset card unavailable")
	}
	resetType := "FIVE_HOUR"
	if credit.Window == "7d" {
		resetType = "WEEK"
	} else if credit.Window != "5h" {
		return errors.New("invalid Z.ai reset window")
	}
	body, err := json.Marshal(struct {
		TargetType string          `json:"targetType"`
		ResetType  string          `json:"resetType"`
		RecordID   json.RawMessage `json:"recordId"`
		RequestID  string          `json:"requestId"`
		GrantType  json.RawMessage `json:"grantType,omitempty"`
	}{"PERSONAL", resetType, credit.recordID, requestID, credit.grantType})
	if err != nil {
		return errors.New("invalid Z.ai reset card")
	}
	response, err := c.request(ctx, http.MethodPost, resetUseURL, key, body, true)
	if err != nil {
		return err
	}
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Success *bool           `json:"success"`
	}
	if json.Unmarshal(response, &envelope) != nil || envelope.Success == nil {
		return &ResetError{Code: "invalid_response", Ambiguous: true}
	}
	if !*envelope.Success {
		return &ResetError{Code: "rejected"}
	}
	if !successfulResetCode(envelope.Code) {
		return &ResetError{Code: "invalid_response", Ambiguous: true}
	}
	return nil
}

func (c ResetClient) request(ctx context.Context, method, endpoint, key string, body []byte, mutation bool) ([]byte, error) {
	key, err := validateAPIKey(key)
	if err != nil || key == "" || c.HTTP == nil {
		return nil, &ResetError{Code: "unavailable"}
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &ResetError{Code: "invalid_request"}
	}
	request.Header.Set("Authorization", key)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, &ResetError{Code: "request_failed", Ambiguous: mutation}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &ResetError{Code: "http_error", HTTPStatus: response.StatusCode, Ambiguous: mutation && (response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500)}
	}
	data, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil, &ResetError{Code: "read_failed", Ambiguous: mutation}
	}
	return data, nil
}

func successfulResetCode(code json.RawMessage) bool {
	return strings.Trim(string(code), `"`) == "200"
}

func resetScalar(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" || len(text) > 256 || strings.TrimSpace(text) != text {
			return ""
		}
		for _, r := range text {
			if unicode.IsControl(r) {
				return ""
			}
		}
		return text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		if _, err := strconv.ParseUint(string(number), 10, 64); err == nil {
			return string(number)
		}
	}
	return ""
}

func resetExpiry(value string) (time.Time, bool) {
	if expiry, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return expiry, true
	}
	// Unzoned vendor timestamps remain visible as supplied. Do not invent a timezone.
	return time.Time{}, false
}

// SelectResetCredit chooses an exact card, or the first known expiry in one window.
func SelectResetCredit(inventory ResetInventory, window, reference string, now time.Time) (ResetCredit, error) {
	var candidates []ResetCredit
	for _, credit := range inventory.Credits {
		if credit.Window != window || !credit.Available || credit.ExpiresAt != nil && !credit.ExpiresAt.After(now) {
			continue
		}
		if reference != "" && credit.ID != reference {
			continue
		}
		candidates = append(candidates, credit)
	}
	if len(candidates) == 0 {
		return ResetCredit{}, errors.New("Z.ai reset card not available for this window")
	}
	if reference != "" && len(candidates) != 1 {
		return ResetCredit{}, errors.New("ambiguous Z.ai reset card")
	}
	if reference == "" {
		for _, credit := range candidates {
			if credit.ExpiresAt == nil {
				return ResetCredit{}, errors.New("Z.ai reset expiry timezone unknown; select an exact card with --credit")
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].ExpiresAt.Before(*candidates[j].ExpiresAt) })
	}
	return candidates[0], nil
}
