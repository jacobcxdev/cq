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
