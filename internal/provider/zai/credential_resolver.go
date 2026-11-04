package zai

import (
	"context"
	"errors"

	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

// Credential resolves only the selected account; OAuth never falls back to a
// stored API key or another account's login.
func (a Account) Credential(ctx context.Context, tokens *zcode.ProjectTokenClient) (string, error) {
	if a.OAuth != nil {
		if tokens == nil || a.APIKey != "" {
			return "", errors.New("ZCode credential resolver unavailable")
		}
		return tokens.Resolve(ctx, *a.OAuth)
	}
	key, err := validateAPIKey(a.APIKey)
	if err != nil || key == "" {
		return "", errors.New("Z.ai credential unavailable")
	}
	return key, nil
}
