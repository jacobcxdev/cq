package proxy

import (
	"errors"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"net/http"
	"time"
)

func reserveDispatchError(reserve *CodexReserve, account codex.AccountKey) error {
	if reserve != nil {
		if blocked, reset := reserve.Blocked(account); blocked {
			limit := &CachedUsageLimitError{}
			if reset > 0 {
				limit.ResetAt = time.Unix(reset, 0)
			}
			return limit
		}
	}
	return nil
}

func writeCodexCapacityError(writer http.ResponseWriter, err error) bool {
	var limit *CachedUsageLimitError
	if !errors.As(err, &limit) {
		return false
	}
	writeError(writer, http.StatusTooManyRequests, "usage_limit_reached", "The usage limit has been reached")
	return true
}

var codexReserveWSLimitFrame = []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`)
