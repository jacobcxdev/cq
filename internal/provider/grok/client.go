package grok

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// BaseURL is Grok Build's subscription endpoint. Public API billing is not a
// fallback for missing or exhausted subscription access.
const BaseURL = "https://cli-chat-proxy.grok.com/v1"

type subscriptionHTTPError struct{ status int }

func (e subscriptionHTTPError) Error() string {
	return fmt.Sprintf("Grok subscription request returned HTTP %d", e.status)
}

func readSubscriptionJSON(ctx context.Context, authority Authority, account AccountID, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if authority == nil {
		return nil, errors.New("Grok subscription authority unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+path, nil)
	if err != nil {
		return nil, errors.New("create Grok subscription request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := authority.Do(ctx, account, req)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("empty Grok subscription response")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, subscriptionHTTPError{status: resp.StatusCode}
	}
	body, err := httputil.ReadBody(resp.Body)
	if err != nil {
		return nil, errors.New("read Grok subscription response")
	}
	return body, nil
}
