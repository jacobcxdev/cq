package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/quota"
)

// PrioritiseZAIAccounts keeps preferred order among healthy or unknown accounts,
// followed by subscriptions known to be unavailable. Callers supply fresh quota
// results. Every credential remains available for explicit rejection fallback.
func PrioritiseZAIAccounts(accounts []zaiprov.Account, results []quota.Result, now time.Time) []zaiprov.Account {
	unavailable := make(map[string]bool)
	for _, result := range results {
		if result.AccountID == "" {
			continue
		}
		if result.Status == quota.StatusError && result.Error != nil && result.Error.Code == "auth_expired" {
			unavailable[result.AccountID] = true
			continue
		}
		if !result.IsUsable() {
			continue
		}
		for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
			window, exists := result.Windows[name]
			if !exists || window.ResetAtUnix > 0 && window.ResetAtUnix <= now.Unix() {
				continue
			}
			remaining := float64(window.RemainingPct)
			if window.RemainingPctExact != nil {
				remaining = *window.RemainingPctExact
			}
			if remaining == 0 {
				unavailable[result.AccountID] = true
			}
		}
	}
	ordered := make([]zaiprov.Account, 0, len(accounts))
	for _, account := range accounts {
		if !unavailable[account.ID] {
			ordered = append(ordered, account)
		}
	}
	for _, account := range accounts {
		if unavailable[account.ID] {
			ordered = append(ordered, account)
		}
	}
	return ordered
}

// Read credentials for each request so account changes do not require a proxy
// restart. The legacy reader remains available for single-account callers.
func (s *Server) zaiAccounts() ([]zaiprov.Account, error) {
	var accounts []zaiprov.Account
	var err error
	if s.ZAIAccounts != nil {
		accounts, err = s.ZAIAccounts()
	} else if s.ZAIAPIKey != nil {
		var key string
		key, err = s.ZAIAPIKey()
		accounts = []zaiprov.Account{{APIKey: key}}
	}
	if err != nil {
		return nil, err
	}
	unique := make([]zaiprov.Account, 0, len(accounts))
	seen := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		account.APIKey = strings.TrimSpace(account.APIKey)
		if account.APIKey != "" && !seen[account.APIKey] {
			unique = append(unique, account)
			seen[account.APIKey] = true
		}
	}
	return unique, nil
}

// A rejected request may move to another subscription before any response is
// forwarded. Network errors and accepted streams are never replayed: the
// upstream may already have consumed them. Opaque response IDs stay on the
// preferred account; WebSocket requests instead replay their complete history.
func (s *Server) doZAIRequest(ctx context.Context, target string, header http.Header, body []byte, accounts []zaiprov.Account) (*http.Response, error) {
	var request struct {
		PreviousResponseID string `json:"previous_response_id"`
	}
	_ = json.Unmarshal(body, &request)
	if request.PreviousResponseID != "" {
		accounts = accounts[:1]
	} else if s.ZAIQuota != nil {
		accounts = PrioritiseZAIAccounts(accounts, s.ZAIQuota(), time.Now())
	}
	transport := s.ZAITransport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for i, account := range accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		setZAIHeaders(request.Header, header, account.APIKey)
		noteRouteAccount(ctx, redactedAccountHint("zai", account.ID), i > 0)
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		rejected := response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests
		if !rejected && i < len(accounts)-1 {
			rejected = zaiInitialSSERejection(response)
		}
		if !rejected || i == len(accounts)-1 {
			return response, nil
		}
		// Closing without draining keeps a rejected or unbounded upstream body
		// from delaying account failover. No rejected payload reaches diagnostics.
		_ = response.Body.Close()
		fmt.Fprintf(os.Stderr, "cq: Z.ai account=%q rejected status=%d; trying next account\n", account.ID, response.StatusCode)
	}
	return nil, fmt.Errorf("Z.ai API key unavailable")
}

const zaiSSEPreludeLimit = 64 << 10

// Z.ai may reject a request with HTTP 200 and an initial failure event. Inspect
// only that first data event, before anything reaches the client. Every byte
// and read error remains available when the stream cannot be safely retried.
func zaiInitialSSERejection(response *http.Response) bool {
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode < 200 || response.StatusCode >= 300 || mediaType != "text/event-stream" {
		return false
	}
	body := response.Body
	var prefix []byte
	var readErr error
	defer func() {
		response.Body = &zaiSSEPreludeBody{body: body, prefix: prefix, readErr: readErr}
	}()
	parser := NewCodexSSEParser(zaiSSEPreludeLimit)
	buffer := make([]byte, 4<<10)
	lineStart := 0
	commentOnly := true
	for len(prefix) < zaiSSEPreludeLimit {
		n, err := body.Read(buffer[:min(len(buffer), zaiSSEPreludeLimit-len(prefix))])
		readErr = err
		prefix = append(prefix, buffer[:n]...)
		events, parseErr := parser.Feed(buffer[:n])
		if parseErr != nil || err != nil && err != io.EOF {
			return false
		}
		if len(events) != 0 {
			return zaiRejectedInitialEvent(events[0])
		}
		// A complete heartbeat line must reach the client now, rather than waiting
		// indefinitely for a later model event. An event in this same read was
		// already handled above; otherwise inspect only complete prelude lines.
		for lineStart < len(prefix) {
			end := bytes.IndexByte(prefix[lineStart:], '\n')
			if end < 0 {
				break
			}
			end += lineStart
			line := bytes.TrimSuffix(prefix[lineStart:end], []byte{'\r'})
			lineStart = end + 1
			if len(line) == 0 {
				commentOnly = true
			} else if line[0] == ':' {
				if commentOnly {
					return false
				}
			} else {
				commentOnly = false
			}
		}
		if err != nil || n == 0 {
			return false
		}
	}
	return false
}

func zaiRejectedInitialEvent(observation CodexSSEObservation) bool {
	data := observation.Data
	if validateZAIRejectionAuthority(data) != nil || validateCodexLifecycleAuthority(data) != nil {
		return false
	}
	type rejection struct {
		Code    json.RawMessage `json:"code"`
		Type    *string         `json:"type"`
		Message *string         `json:"message"`
		Param   *string         `json:"param"`
	}
	var event struct {
		Type       string            `json:"type"`
		Code       json.RawMessage   `json:"code"`
		Error      rejection         `json:"error"`
		Status     *int              `json:"status"`
		StatusCode *int              `json:"status_code"`
		Output     []json.RawMessage `json:"output"`
		Response   struct {
			Status string            `json:"status"`
			Error  rejection         `json:"error"`
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil || len(event.Output) != 0 {
		return false
	}
	var raw json.RawMessage
	switch event.Type {
	case "response.failed":
		if event.Response.Status != "failed" || len(event.Response.Output) != 0 {
			return false
		}
		raw = event.Response.Error.Code
	case "error":
		raw = event.Code
		if len(raw) == 0 {
			raw = event.Error.Code
		}
	default:
		return false
	}
	// The Codex grammar requires string error codes. Z.ai also uses numeric
	// codes, so only that typed, unambiguous vendor difference may override a
	// generic observation parse error.
	if observation.ParseError != nil && (len(raw) == 0 || raw[0] == '"') {
		return false
	}
	// Numeric authentication and quota meanings are documented at
	// https://docs.z.ai/api-reference/api-code. Unknown and global service
	// failures deliberately remain on the original account.
	switch strings.Trim(string(raw), `"`) {
	case "rate_limit_exceeded", "insufficient_quota",
		"1000", "1001", "1003", "1005", "1113", "1302", "1308", "1310",
		"1316", "1317", "1318", "1319", "1320", "1321":
		return true
	}
	return false
}

func validateZAIRejectionAuthority(data []byte) error {
	return validateCodexJSONAuthority(data, "Z.ai initial event", func(path []string, name string) bool {
		fields := ""
		switch {
		case len(path) == 0:
			fields = " type code error response status status_code output "
		case len(path) == 1 && path[0] == "response":
			fields = " status error output "
		case len(path) == 1 && path[0] == "error", len(path) == 2 && path[0] == "response" && path[1] == "error":
			fields = " type code message param "
		}
		for _, field := range strings.Fields(fields) {
			if codexJSONNameEqual(name, field) {
				return true
			}
		}
		return false
	})
}

type zaiSSEPreludeBody struct {
	body    io.ReadCloser
	prefix  []byte
	readErr error
}

func (body *zaiSSEPreludeBody) Read(buffer []byte) (int, error) {
	if len(body.prefix) != 0 {
		n := copy(buffer, body.prefix)
		body.prefix = body.prefix[n:]
		if len(body.prefix) != 0 {
			return n, nil
		}
		err := body.readErr
		body.readErr = nil
		return n, err
	}
	if body.readErr != nil {
		err := body.readErr
		body.readErr = nil
		return 0, err
	}
	return body.body.Read(buffer)
}

func (body *zaiSSEPreludeBody) Close() error { return body.body.Close() }
