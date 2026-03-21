package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// fakeJWT builds a JWT with the given payload (no signature verification needed).
func fakeJWT(payload any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	body, _ := json.Marshal(payload)
	encoded := base64.RawURLEncoding.EncodeToString(body)
	return header + "." + encoded + ".fakesig"
}

func TestDecodeCodexClaims(t *testing.T) {
	token := fakeJWT(map[string]any{
		"email": "User@Example.COM",
		"exp":   1774076490.0,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-uuid-123",
			"chatgpt_user_id":    "user-uuid-456",
			"chatgpt_plan_type":  "plus",
		},
	})
	c := DecodeCodexClaims(token)
	if c.Email != "user@example.com" {
		t.Errorf("Email = %q, want user@example.com", c.Email)
	}
	if c.AccountID != "acct-uuid-123" {
		t.Errorf("AccountID = %q, want acct-uuid-123", c.AccountID)
	}
	if c.UserID != "user-uuid-456" {
		t.Errorf("UserID = %q, want user-uuid-456", c.UserID)
	}
	if c.PlanType != "plus" {
		t.Errorf("PlanType = %q, want plus", c.PlanType)
	}
	if c.ExpiresAt != 1774076490 {
		t.Errorf("ExpiresAt = %d, want 1774076490", c.ExpiresAt)
	}
}

func TestDecodeCodexClaimsRecordKey(t *testing.T) {
	c := CodexClaims{UserID: "user-abc", AccountID: "acct-def"}
	if got := c.RecordKey(); got != "user-abc::acct-def" {
		t.Errorf("RecordKey() = %q, want user-abc::acct-def", got)
	}
}

func TestDecodeCodexClaimsMissingAuth(t *testing.T) {
	token := fakeJWT(map[string]any{
		"email": "user@test.com",
		"exp":   1700000000.0,
	})
	c := DecodeCodexClaims(token)
	if c.Email != "user@test.com" {
		t.Errorf("Email = %q, want user@test.com", c.Email)
	}
	if c.AccountID != "" {
		t.Errorf("AccountID = %q, want empty", c.AccountID)
	}
	if c.UserID != "" {
		t.Errorf("UserID = %q, want empty", c.UserID)
	}
	if c.PlanType != "" {
		t.Errorf("PlanType = %q, want empty", c.PlanType)
	}
}

func TestDecodeCodexClaimsInvalidToken(t *testing.T) {
	for _, input := range []string{"", "x", "x.!!!.z", "a.b"} {
		c := DecodeCodexClaims(input)
		if c.Email != "" || c.AccountID != "" {
			t.Errorf("DecodeCodexClaims(%q) should return zero claims", input)
		}
	}
}
