package httputil

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxResponseBody is the upper bound on HTTP response body reads (1 MiB).
const MaxResponseBody = 1 << 20

// ReadBody reads up to MaxResponseBody bytes from r.
// Returns an error if the body exceeds MaxResponseBody.
func ReadBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxResponseBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResponseBody {
		return nil, fmt.Errorf("response body exceeds %d bytes", MaxResponseBody)
	}
	return data, nil
}

// MaxErrorBodyLen is the maximum length of an error body snippet.
const MaxErrorBodyLen = 200

// TruncateBody returns up to max bytes of b as a string.
func TruncateBody(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max])
}

// Doer abstracts HTTP client operations for testability.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

type uaTransport struct {
	base http.RoundTripper
	ua   string
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	return t.base.RoundTrip(req)
}

// NewClient creates an HTTP client with the given timeout and a default User-Agent.
// The version string is included in the User-Agent header.
func NewClient(timeout time.Duration, version string) *http.Client {
	ua := "cq/" + version
	return &http.Client{
		Timeout:   timeout,
		Transport: &uaTransport{base: http.DefaultTransport, ua: ua},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
}
