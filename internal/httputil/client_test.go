package httputil

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewClientTimeout(t *testing.T) {
	// Use a very short timeout (1ms) and a very long server delay (10s) to make
	// the test deterministic even under heavy load.
	c := NewClient(1*time.Millisecond, "test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	_, err := c.Do(req)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// Verify it is a network timeout error rather than some other failure.
	var netErr interface{ Timeout() bool }
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("expected net.Error with Timeout()=true, got %T: %v", err, err)
	}
}

func TestNewClientSetsUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	c := NewClient(5*time.Second, "1.2.3")
	req, _ := http.NewRequest("GET", srv.URL, nil)
	c.Do(req)
	if gotUA != "cq/1.2.3" {
		t.Errorf("User-Agent = %q, want cq/1.2.3", gotUA)
	}
}

func TestReadBody(t *testing.T) {
	content := []byte("hello, world")
	got, err := ReadBody(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("ReadBody error: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("ReadBody = %q, want %q", got, content)
	}
}

func TestReadBodyTruncation(t *testing.T) {
	// Build a reader with MaxResponseBody+1 bytes.
	data := bytes.Repeat([]byte("x"), MaxResponseBody+1)
	_, err := ReadBody(bytes.NewReader(data))
	if err == nil {
		t.Fatal("ReadBody should return error for oversized body")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("ReadBody error = %q, want message containing 'exceeds'", err)
	}
}

func TestReadBodyEmpty(t *testing.T) {
	got, err := ReadBody(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ReadBody error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadBody len = %d, want 0", len(got))
	}
}

func TestTruncateBody(t *testing.T) {
	tests := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{"below max", "hello", 10, "hello"},
		{"at max", "hello", 5, "hello"},
		{"above max", "hello world", 5, "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateBody([]byte(tt.input), tt.max)
			if got != tt.want {
				t.Errorf("TruncateBody(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
		})
	}
}

func TestCheckRedirectStripAuth(t *testing.T) {
	c := NewClient(5*time.Second, "test")

	req, _ := http.NewRequest("GET", "http://other.example.com/path", nil)
	req.Header.Set("Authorization", "Bearer token")

	via := []*http.Request{{}}
	via[0].URL, _ = url.Parse("http://original.example.com/start")

	err := c.CheckRedirect(req, via)
	if err != nil {
		t.Fatalf("CheckRedirect error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty (stripped on cross-host redirect)", got)
	}
}

func TestCheckRedirectSameHost(t *testing.T) {
	c := NewClient(5*time.Second, "test")

	req, _ := http.NewRequest("GET", "http://example.com/page2", nil)
	req.Header.Set("Authorization", "Bearer token")

	via := []*http.Request{{}}
	via[0].URL, _ = url.Parse("http://example.com/page1")

	err := c.CheckRedirect(req, via)
	if err != nil {
		t.Fatalf("CheckRedirect error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer token" {
		t.Errorf("Authorization = %q, want %q (same-host should preserve)", got, "Bearer token")
	}
}

func TestCheckRedirectLimit(t *testing.T) {
	c := NewClient(5*time.Second, "test")

	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	// Build 10 via entries to trigger the limit.
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = &http.Request{}
		via[i].URL, _ = url.Parse("http://example.com/")
	}

	err := c.CheckRedirect(req, via)
	if err == nil {
		t.Fatal("expected error for >10 redirects, got nil")
	}
	if !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "too many redirects")
	}
}
