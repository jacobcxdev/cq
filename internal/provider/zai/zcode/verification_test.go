package zcode

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

// These tests fake only browser callbacks. They do not run the vendor SDK or
// establish that a fixture string is a valid human-verification result.
func TestVerifyReturnsOneCallbackAndClosesListener(t *testing.T) {
	var opened string
	proof, err := Verify(context.Background(), fixtureVerificationConfig(), func(ctx context.Context, target string) error {
		opened = target
		u, err := url.Parse(target)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.RawQuery != "" {
			t.Fatal("verification did not use an isolated loopback page")
		}
		r, err := http.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := httputil.ReadBody(r.Body)
		r.Body.Close()
		if readErr != nil || r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-store" || strings.Contains(string(body), "zcode-fixture-token") {
			t.Fatal("verification page failed or contained an account credential")
		}
		if got := postVerification(t, ctx, target, "", `{"param":"fixture-sdk-result"}`); got != 200 {
			t.Fatalf("callback status = %d", got)
		}
		if got := postVerification(t, ctx, target, "", `{"param":"other-result"}`); got != 409 {
			t.Fatalf("second callback status = %d", got)
		}
		return nil
	})
	if err != nil || proof.Param != "fixture-sdk-result" || proof.Region != "sgp" {
		t.Fatalf("proof fields missing, err = %v", err)
	}
	client := &http.Client{Timeout: time.Second}
	if r, err := client.Get(opened); err == nil {
		r.Body.Close()
		t.Fatal("verification listener survived completion")
	}
}

func TestVerifyRejectsCrossOriginBadStateAndMalformedCallbacks(t *testing.T) {
	proof, err := Verify(context.Background(), fixtureVerificationConfig(), func(ctx context.Context, target string) error {
		for _, test := range []struct{ kind, body string }{
			{"origin", `{"param":"fixture"}`},
			{"state", `{"param":"fixture"}`},
			{"host", `{"param":"fixture"}`},
			{"fetch-site", `{"param":"fixture"}`},
			{"method", `{"param":"fixture"}`},
			{"type", `{"param":"fixture"}`},
			{"", `{"param":""}`},
			{"", `{"param":"fixture","error":"failed"}`},
			{"", `{"param":"fixture","extra":1}`},
			{"", `{"param":"bad\nproof"}`},
			{"", `{"param":"` + strings.Repeat("p", 66000) + `"}`},
		} {
			if got := postVerification(t, ctx, target, test.kind, test.body); got < 400 {
				t.Fatalf("invalid %s callback accepted", test.kind)
			}
		}
		if got := postVerification(t, ctx, target, "", `{"param":"fixture-after-invalid"}`); got != 200 {
			t.Fatal("invalid callback consumed the valid pending result")
		}
		return nil
	})
	if err != nil || proof.Param != "fixture-after-invalid" {
		t.Fatalf("valid callback after invalid input failed: %v", err)
	}
}

func TestVerifyStopsOnSDKFailureCancelAndOpenerFailure(t *testing.T) {
	for _, failure := range []string{"failed", "cancelled"} {
		_, err := Verify(context.Background(), fixtureVerificationConfig(), func(ctx context.Context, target string) error {
			postVerification(t, ctx, target, "", `{"error":"`+failure+`"}`)
			return nil
		})
		if err == nil || strings.Contains(err.Error(), "fixture") {
			t.Fatal("failed verification returned proof or unredacted error")
		}
	}
	for _, opener := range []func(context.Context, string) error{
		func(context.Context, string) error { return errors.New("private opener details") },
		func(context.Context, string) error { panic("private opener details") },
	} {
		if _, err := Verify(context.Background(), fixtureVerificationConfig(), opener); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("opener failure not safely returned")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := Verify(ctx, fixtureVerificationConfig(), func(context.Context, string) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestVerifyRejectsInvalidConfigurationBeforeOpening(t *testing.T) {
	for _, cfg := range []VerificationConfig{{}, {Enabled: true, Region: "cn", Prefix: "fixture", SceneID: "bad\"scene"}, {Enabled: true, Region: "unrecognised", Prefix: "fixture", SceneID: "scene"}} {
		if _, err := Verify(context.Background(), cfg, func(context.Context, string) error { t.Fatal("invalid config opened browser"); return nil }); err == nil {
			t.Fatal("invalid verification config accepted")
		}
	}
}

func TestVerifyDeadlineClosesPendingListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var opened string
	_, err := Verify(ctx, fixtureVerificationConfig(), func(_ context.Context, target string) error { opened = target; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || opened == "" {
		t.Fatalf("pending verification did not honor deadline: %v", err)
	}
	client := &http.Client{Timeout: time.Second}
	if response, err := client.Get(opened); err == nil {
		response.Body.Close()
		t.Fatal("timed-out verification listener survived")
	}
}

func fixtureVerificationConfig() VerificationConfig {
	return VerificationConfig{Enabled: true, Prefix: "fixture-prefix", Region: "sgp", SceneID: "fixture-scene"}
}

func postVerification(t *testing.T, ctx context.Context, target, wrong, body string) int {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	state := strings.TrimPrefix(u.Path, "/verify/")
	u.Path = "/result"
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", u.Scheme+"://"+u.Host)
	r.Header.Set("X-CQ-Verification-State", state)
	switch wrong {
	case "origin":
		r.Header.Set("Origin", "https://example.invalid")
	case "state":
		r.Header.Set("X-CQ-Verification-State", "wrong-state")
	case "host":
		r.Host = "example.invalid"
	case "fetch-site":
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	case "method":
		r.Method = http.MethodGet
	case "type":
		r.Header.Set("Content-Type", "text/plain")
	}
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}
