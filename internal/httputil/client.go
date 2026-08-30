package httputil

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// MaxResponseBody is the upper bound on HTTP response body reads (1 MiB).
const MaxResponseBody = 1 << 20

// ErrBodyTooLarge reports a response body that exceeded its configured limit.
var ErrBodyTooLarge = errors.New("response body exceeds configured limit")

// ReadBody reads up to MaxResponseBody bytes from r.
// Returns an error if the body exceeds MaxResponseBody.
func ReadBody(r io.Reader) ([]byte, error) {
	return ReadBodyLimit(r, MaxResponseBody)
}

// ReadBodyLimit reads at most limit bytes without closing r.
func ReadBodyLimit(r io.Reader, limit int64) ([]byte, error) {
	if limit < 0 || limit == math.MaxInt64 {
		return nil, fmt.Errorf("invalid body limit %d", limit)
	}
	reader := &io.LimitedReader{R: r, N: limit + 1}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrBodyTooLarge
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
