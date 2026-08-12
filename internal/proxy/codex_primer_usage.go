package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func CodexPrimerUsageURL(codexUpstream string) (string, error) {
	parsed, err := url.Parse(codexUpstream)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Codex upstream URL")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(path, "/codex") {
		return "", fmt.Errorf("Codex upstream path must end in /codex")
	}
	parsed.Path = strings.TrimSuffix(path, "/codex") + "/wham/usage"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

const DefaultCodexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
const defaultCodexPrimerUsageTimeout = 30 * time.Second

type CodexPrimerUsageReader struct {
	Router   *CodexRequestRouter
	UsageURL string
	Timeout  time.Duration
}

func (r *CodexPrimerUsageReader) Read(ctx context.Context, account codex.AccountKey) (codex.UsageObservation, error) {
	if r == nil || r.Router == nil || r.UsageURL == "" || account == "" {
		return codex.UsageObservation{}, fmt.Errorf("Codex primer usage reader unavailable")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultCodexPrimerUsageTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.UsageURL, nil)
	if err != nil {
		return codex.UsageObservation{}, err
	}
	response, _, failure, err := r.Router.DoPinned(ctx, RouteChoice{AccountKey: account}, req)
	if err != nil {
		return codex.UsageObservation{}, fmt.Errorf("read Codex primer usage: %w", err)
	}
	if failure != CodexPinnedAccepted || response == nil || response.Body == nil {
		if response != nil {
			closeResponse(response)
		}
		return codex.UsageObservation{}, fmt.Errorf("Codex primer usage credential rejected")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, codexAttemptResponseLimit+1))
	if err != nil {
		return codex.UsageObservation{}, fmt.Errorf("read Codex primer usage response: %w", err)
	}
	if len(data) > codexAttemptResponseLimit {
		return codex.UsageObservation{}, fmt.Errorf("Codex primer usage response exceeds 1 MiB")
	}
	if response.StatusCode != http.StatusOK {
		return codex.UsageObservation{}, fmt.Errorf("Codex primer usage HTTP %d", response.StatusCode)
	}
	observation := codex.ParseUsageObservation(data, "", "")
	if !observation.Result.IsUsable() {
		return codex.UsageObservation{}, fmt.Errorf("Codex primer usage response invalid")
	}
	return observation, nil
}
