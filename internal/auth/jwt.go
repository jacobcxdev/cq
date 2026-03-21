package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// DecodeEmail extracts the email claim from a JWT without verifying the signature.
// WARNING: The JWT signature is NOT verified. Do not use the result
// for authentication or authorization decisions.
func DecodeEmail(token string) string {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Email
}

// CodexClaims holds decoded claims from a Codex/ChatGPT JWT id_token.
type CodexClaims struct {
	Email     string
	AccountID string // chatgpt_account_id from "https://api.openai.com/auth"
	UserID    string // chatgpt_user_id from "https://api.openai.com/auth"
	PlanType  string // chatgpt_plan_type from "https://api.openai.com/auth"
	ExpiresAt int64  // top-level "exp" claim
}

// RecordKey returns the codex-auth-compatible account key: "{UserID}::{AccountID}".
func (c CodexClaims) RecordKey() string {
	return c.UserID + "::" + c.AccountID
}

// DecodeCodexClaims extracts Codex-specific claims from a JWT id_token
// without verifying the signature. Account info is nested under the
// "https://api.openai.com/auth" claim object.
func DecodeCodexClaims(token string) CodexClaims {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) < 2 {
		return CodexClaims{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return CodexClaims{}
	}
	var raw struct {
		Email string  `json:"email"`
		Exp   float64 `json:"exp"`
		Auth  *struct {
			AccountID string `json:"chatgpt_account_id"`
			UserID    string `json:"chatgpt_user_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &raw) != nil {
		return CodexClaims{}
	}
	c := CodexClaims{
		Email:     strings.ToLower(raw.Email),
		ExpiresAt: int64(raw.Exp),
	}
	if raw.Auth != nil {
		c.AccountID = raw.Auth.AccountID
		c.UserID = raw.Auth.UserID
		c.PlanType = raw.Auth.PlanType
	}
	return c
}
