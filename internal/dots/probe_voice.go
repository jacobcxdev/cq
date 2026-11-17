package dots

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func (p *Probe) retryNativeVoice(r *http.Request, rejected *http.Response, offer []byte) *http.Response {
	if encoding := rejected.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil
	}
	if rejected.Header.Get("Location") != "" {
		return nil
	}
	const maxRejection = 64 << 10
	prefix, err := probeReadPrefix(rejected.Body, maxRejection)
	rejected.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(prefix), rejected.Body), rejected.Body}
	if err != nil || len(prefix) > maxRejection || !proxy.IsCodexNativeVoiceQuotaRejection(prefix) {
		return nil
	}
	if p.options.RequireNative(r.Context(), r.Header) != nil {
		return nil
	}

	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.options.VoiceProxy+r.URL.RequestURI(), bytes.NewReader(offer))
	if err != nil {
		return nil
	}
	request.Header = probeHeaders(r.Header)
	request.Header.Del("Cookie")
	request.Header.Del("Content-Length")
	response, err := p.options.HTTP.Do(request)
	if err != nil {
		return nil
	}
	// Return the original typed limit if the proxy cannot create a new call.
	if response.StatusCode < 200 || response.StatusCode >= 300 || response.Header.Get("Location") == "" {
		response.Body.Close()
		return nil
	}
	return response
}
