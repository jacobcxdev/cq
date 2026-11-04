// Package zcode implements the inspected ZCode authentication and model protocols.
package zcode

import "errors"

var ErrNotConfigured = errors.New("ZCode Z.ai login is not configured")

// Session carries account credentials in memory; tokens never enter public JSON.
type Session struct {
	Family               string `json:"family"`
	ZCodeJWT             string `json:"-"`
	BusinessJWT          string `json:"-"`
	PlaintextCredentials bool   `json:"plaintext_credentials,omitempty"`
}

func validHeaderValue(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r >= 127 {
			return false
		}
	}
	return true
}
