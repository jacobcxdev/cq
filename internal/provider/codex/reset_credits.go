package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const (
	resetCreditsURL     = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	consumeResetURL     = resetCreditsURL + "/consume"
	resetListTimeout    = 5 * time.Second
	resetConsumeTimeout = 10 * time.Second
)

type ResetType string
type ResetCreditStatus string
type ConsumeResetOutcome string

const (
	ResetTypeCodexRateLimits ResetType = "codex_rate_limits"

	ResetCreditAvailable ResetCreditStatus = "available"
	ResetCreditRedeeming ResetCreditStatus = "redeeming"
	ResetCreditRedeemed  ResetCreditStatus = "redeemed"

	ConsumeReset           ConsumeResetOutcome = "reset"
	ConsumeAlreadyRedeemed ConsumeResetOutcome = "already_redeemed"
	ConsumeNothingToReset  ConsumeResetOutcome = "nothing_to_reset"
	ConsumeNoCredit        ConsumeResetOutcome = "no_credit"
)

type ResetCredit struct {
	ID          string            `json:"id"`
	ResetType   ResetType         `json:"reset_type"`
	Status      ResetCreditStatus `json:"status"`
	GrantedAt   time.Time         `json:"granted_at"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
}

type ResetCreditEntryError struct {
	Index int    `json:"index"`
	Code  string `json:"code"`
}

type ResetCreditInventory struct {
	Credits        []ResetCredit           `json:"credits"`
	AvailableCount int                     `json:"available_count"`
	EntryErrors    []ResetCreditEntryError `json:"entry_errors,omitempty"`
}

type ConsumeResetResult struct {
	Outcome      ConsumeResetOutcome `json:"outcome"`
	WindowsReset int64               `json:"windows_reset"`
}

type ResetHTTPError struct {
	Status int
}

func (e *ResetHTTPError) Error() string {
	return fmt.Sprintf("reset-credit request returned HTTP %d", e.Status)
}

type ResetCreditInventoryError struct {
	Code string
}

func (e *ResetCreditInventoryError) Error() string {
	return "reset-credit inventory is invalid: " + e.Code
}

type ResetCreditClient struct {
	HTTP httputil.Doer
}

type rawResetCredit struct {
	ID          string  `json:"id"`
	ResetType   string  `json:"reset_type"`
	Status      string  `json:"status"`
	GrantedAt   string  `json:"granted_at"`
	ExpiresAt   *string `json:"expires_at"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

type rawResetCreditInventory struct {
	Credits        *[]rawResetCredit `json:"credits"`
	AvailableCount *int64            `json:"available_count"`
}

func (c ResetCreditClient) List(ctx context.Context, material CredentialMaterial) (ResetCreditInventory, error) {
	if err := validateResetClient(c, material); err != nil {
		return ResetCreditInventory{}, err
	}
	if err := ctx.Err(); err != nil {
		return ResetCreditInventory{}, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, resetListTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, resetCreditsURL, nil)
	if err != nil {
		return ResetCreditInventory{}, err
	}
	addResetCreditHeaders(req, material)

	body, err := c.do(req)
	if err != nil {
		return ResetCreditInventory{}, err
	}
	var raw rawResetCreditInventory
	if err := decodeResetJSON(body, &raw); err != nil {
		return ResetCreditInventory{}, &ResetCreditInventoryError{Code: "invalid_response"}
	}
	if raw.Credits == nil || raw.AvailableCount == nil {
		return ResetCreditInventory{}, &ResetCreditInventoryError{Code: "missing_fields"}
	}
	if *raw.AvailableCount < 0 || int64(int(*raw.AvailableCount)) != *raw.AvailableCount {
		return ResetCreditInventory{}, &ResetCreditInventoryError{Code: "invalid_available_count"}
	}

	inventory := ResetCreditInventory{
		Credits:        make([]ResetCredit, 0, len(*raw.Credits)),
		AvailableCount: int(*raw.AvailableCount),
	}
	rawAvailable := 0
	for index, entry := range *raw.Credits {
		if entry.Status == string(ResetCreditAvailable) {
			rawAvailable++
		}
		credit, code := parseResetCredit(entry)
		if code != "" {
			inventory.EntryErrors = append(inventory.EntryErrors, ResetCreditEntryError{Index: index, Code: code})
			continue
		}
		inventory.Credits = append(inventory.Credits, credit)
	}
	if rawAvailable != inventory.AvailableCount {
		return inventory, &ResetCreditInventoryError{Code: "available_count_mismatch"}
	}
	if len(inventory.EntryErrors) > 0 {
		return inventory, &ResetCreditInventoryError{Code: "invalid_credit_entries"}
	}
	return inventory, nil
}

func (c ResetCreditClient) Consume(ctx context.Context, material CredentialMaterial, creditID, redeemRequestID string) (ConsumeResetResult, error) {
	if err := validateResetClient(c, material); err != nil {
		return ConsumeResetResult{}, err
	}
	if strings.TrimSpace(creditID) == "" || strings.TrimSpace(redeemRequestID) == "" {
		return ConsumeResetResult{}, errors.New("reset credit and redeem request IDs are required")
	}
	if err := ctx.Err(); err != nil {
		return ConsumeResetResult{}, err
	}

	payload, err := json.Marshal(struct {
		RedeemRequestID string `json:"redeem_request_id"`
		CreditID        string `json:"credit_id"`
	}{RedeemRequestID: redeemRequestID, CreditID: creditID})
	if err != nil {
		return ConsumeResetResult{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, resetConsumeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, consumeResetURL, bytes.NewReader(payload))
	if err != nil {
		return ConsumeResetResult{}, err
	}
	addResetCreditHeaders(req, material)
	req.Header.Set("Content-Type", "application/json")

	body, err := c.do(req)
	if err != nil {
		return ConsumeResetResult{}, err
	}
	var raw struct {
		Code         string `json:"code"`
		WindowsReset int64  `json:"windows_reset"`
	}
	if err := decodeResetJSON(body, &raw); err != nil {
		return ConsumeResetResult{}, errors.New("invalid reset-credit consume response")
	}
	outcome := ConsumeResetOutcome(raw.Code)
	switch outcome {
	case ConsumeReset, ConsumeAlreadyRedeemed, ConsumeNothingToReset, ConsumeNoCredit:
	default:
		return ConsumeResetResult{}, errors.New("unknown reset-credit consume outcome")
	}
	if raw.WindowsReset < 0 {
		return ConsumeResetResult{}, errors.New("invalid reset-credit windows count")
	}
	return ConsumeResetResult{Outcome: outcome, WindowsReset: raw.WindowsReset}, nil
}

func validateResetClient(client ResetCreditClient, material CredentialMaterial) error {
	if client.HTTP == nil {
		return errors.New("reset-credit HTTP client unavailable")
	}
	if material.AccessToken == "" || material.AccountID == "" {
		return errors.New("reset-credit credential unavailable")
	}
	return nil
}

func addResetCreditHeaders(req *http.Request, material CredentialMaterial) {
	req.Header.Set("Authorization", "Bearer "+material.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", material.AccountID)
}

func (c ResetCreditClient) do(req *http.Request) ([]byte, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("reset-credit request failed: %w", err)
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("reset-credit response unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &ResetHTTPError{Status: resp.StatusCode}
	}
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read reset-credit response: %w", err)
	}
	return body, nil
}

func decodeResetJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func parseResetCredit(raw rawResetCredit) (ResetCredit, string) {
	if raw.ID == "" || strings.TrimSpace(raw.ID) != raw.ID {
		return ResetCredit{}, "invalid_id"
	}
	grantedAt, err := time.Parse(time.RFC3339, raw.GrantedAt)
	if err != nil {
		return ResetCredit{}, "invalid_granted_at"
	}
	var expiresAt *time.Time
	if raw.ExpiresAt != nil {
		parsed, err := time.Parse(time.RFC3339, *raw.ExpiresAt)
		if err != nil {
			return ResetCredit{}, "invalid_expires_at"
		}
		expiresAt = &parsed
	}
	credit := ResetCredit{
		ID: raw.ID, ResetType: ResetType(raw.ResetType), Status: ResetCreditStatus(raw.Status),
		GrantedAt: grantedAt, ExpiresAt: expiresAt,
	}
	if raw.Title != nil {
		credit.Title = *raw.Title
	}
	if raw.Description != nil {
		credit.Description = *raw.Description
	}
	return credit, ""
}
