package dots

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func cloudCredentialToken(expires time.Time) string {
	claims, _ := json.Marshal(map[string]any{"exp": expires.Unix()})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
}

func TestCloudCredentialsRotateForHTTPStatusAndNextDial(t *testing.T) {
	h := newCloudTestHarness(t)
	base := time.Now().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(base.Unix())
	h.deps.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	h.accounts[1].Token = cloudCredentialToken(base.Add(90 * time.Second))
	fresh := h.accounts[1]
	fresh.Token = cloudCredentialToken(base.Add(time.Hour))
	var resolutions atomic.Int64
	h.deps.resolveOwner = func(alias string) (Account, error) {
		resolutions.Add(1)
		if alias != fresh.Alias {
			return Account{}, errors.New("unexpected owner")
		}
		return fresh, nil
	}
	requests := make(chan string, 8)
	h.deps.http = cloudTestHTTP(func(r *http.Request) (*http.Response, error) {
		requests <- r.Header.Get("Authorization")
		return cloudResponse(200, `{"thread":{"id":"secondary-root","status":{"type":"idle"}}}`), nil
	})
	h.start()
	h.initialize()
	h.send(2, "thread/read", "secondary-root")
	if h.await(2)["error"] != nil || <-requests != "Bearer "+h.accounts[1].Token {
		t.Fatal("initial credential not used")
	}
	clock.Store(base.Add(91 * time.Second).Unix())
	h.send(3, "thread/read", "secondary-root")
	if h.await(3)["error"] != nil || <-requests != "Bearer "+fresh.Token {
		t.Fatal("HTTP read retained expired credential")
	}
	h.send(4, "thread/resume", "secondary-root")
	if h.await(4)["error"] != nil {
		t.Fatal("healthy socket interrupted by rotation")
	}
	select {
	case authorization := <-requests:
		if authorization != "Bearer "+fresh.Token {
			t.Fatal("status read retained expired credential")
		}
	case <-time.After(time.Second):
		t.Fatal("status refresh unavailable")
	}
	h.socketMu.Lock()
	first := h.sockets[0]
	h.socketMu.Unlock()
	select {
	case <-first.closed:
		t.Fatal("rotation closed a healthy socket")
	default:
	}
	first.mu.Lock()
	first.onSend = func(request cloudMessage) (bool, error) {
		if request.method() == "thread/resume" {
			first.output <- nil
			return true, nil
		}
		return false, nil
	}
	first.mu.Unlock()
	h.send(5, "thread/resume", "secondary-root")
	if h.await(5)["error"] == nil {
		t.Fatal("closed socket did not fail pending request")
	}
	h.send(6, "thread/resume", "secondary-root")
	if h.await(6)["error"] != nil {
		t.Fatal("later request could not reconnect")
	}
	h.socketMu.Lock()
	defer h.socketMu.Unlock()
	if len(h.dialOwners) != 2 || h.dialOwners[1].Token != fresh.Token || h.dialOwners[1].AccountID != fresh.AccountID {
		t.Fatal("new dial did not use rotated pinned owner")
	}
	if resolutions.Load() != 1 || h.accounts[1].Token == fresh.Token {
		t.Fatal("credential cache churned or initial owner mutated")
	}
}

func TestCloudCredentialRefreshRejectsUnavailableOrSwitchedOwner(t *testing.T) {
	for _, test := range []string{"alias", "account", "missing", "expired"} {
		t.Run(test, func(t *testing.T) {
			h := newCloudTestHarness(t)
			h.accounts[1].Token = cloudCredentialToken(time.Now().Add(-time.Second))
			fresh := h.accounts[1]
			fresh.Token = cloudTestToken()
			h.deps.resolveOwner = func(string) (Account, error) {
				switch test {
				case "alias":
					fresh.Alias = h.accounts[0].Alias
				case "account":
					fresh.AccountID = h.accounts[0].AccountID
				case "missing":
					return Account{}, errors.New("missing credential")
				case "expired":
					fresh.Token = h.accounts[1].Token
				}
				return fresh, nil
			}
			var calls atomic.Int64
			h.deps.http = cloudTestHTTP(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return cloudResponse(200, `{}`), nil
			})
			b := &cloudBridge{ctx: context.Background(), cfg: h.cfg, deps: h.deps, owners: map[string]Account{h.accounts[1].Alias: h.accounts[1]}, roots: map[string]string{"secondary-root": "22222222"}, output: h.output}
			if _, err := b.ownerThreadStatus("22222222", "secondary-root"); err == nil {
				t.Fatal("unverified status credential accepted")
			}
			b.httpRead("22222222", cloudMessage{"id": json.RawMessage(`2`), "method": json.RawMessage(`"thread/read"`)}, "secondary-root")
			if calls.Load() != 0 || (<-h.output.values)["error"] == nil {
				t.Fatal("unverified owner reached HTTP boundary")
			}
			h.start()
			h.initialize()
			h.send(3, "thread/resume", "secondary-root")
			if h.await(3)["error"] == nil || h.socketCount() != 0 {
				t.Fatal("unverified owner reached dial boundary")
			}
		})
	}
}

func TestCloudSendFailureDoesNotRetryAfterCredentialRotation(t *testing.T) {
	h := newCloudTestHarness(t)
	fresh := h.accounts[1]
	h.accounts[1].Token = cloudCredentialToken(time.Now().Add(-time.Second))
	h.deps.resolveOwner = func(string) (Account, error) { return fresh, nil }
	var sends atomic.Int64
	original := h.deps.dial
	h.deps.dial = func(ctx context.Context, owner Account) (cloudSocket, int, error) {
		if owner.Token != fresh.Token {
			t.Error("dial retained expired credential")
		}
		connection, code, err := original(ctx, owner)
		connection.(*cloudTestSocket).onSend = func(request cloudMessage) (bool, error) {
			if request.method() == "thread/resume" {
				sends.Add(1)
				return true, errors.New("synthetic write failure")
			}
			return false, nil
		}
		return connection, code, err
	}
	h.start()
	h.initialize()
	h.send(2, "thread/resume", "secondary-root")
	if h.await(2)["error"] == nil {
		t.Fatal("send failure not returned")
	}
	h.send(3, "thread/start", "")
	h.await(3)
	if h.socketCount() != 1 || sends.Load() != 1 || strings.Count(h.output.text(), `"id":2`) != 1 {
		t.Fatal("failed request retried or completed more than once")
	}
}

func TestCloudDefaultResolverRereadsConfiguredInventory(t *testing.T) {
	c := catalogueTestClient(t)
	c.Now = time.Now
	catalogueTestWrite(t, c.Settings.SystemAuth, catalogueTestDocument("system", time.Now().Add(time.Hour).Unix()))
	path := filepath.Join(c.Settings.CQAccounts, "secondary.auth.json")
	catalogueTestWrite(t, path, catalogueTestDocument("cloud-secondary", time.Now().Add(time.Hour).Unix()))
	inventory, err := c.Inventory()
	if err != nil || len(inventory.Accounts) != 2 {
		t.Fatalf("initial inventory: %v", err)
	}
	pinned := inventory.Accounts[1]
	pinned.Token = cloudCredentialToken(time.Now().Add(-time.Second))
	c.Settings.RuntimeRoot = t.TempDir()
	deps := cloudStdioDefaults(c.Settings)
	b := &cloudBridge{deps: deps, owners: map[string]Account{pinned.Alias: pinned}}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := b.ownerCredential(pinned.Alias)
	if err != nil || owner.Token != inventory.Accounts[1].Token || owner.AccountID != pinned.AccountID {
		t.Fatal("configured inventory was not reread")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("credential read changed configured file")
	}
	catalogueTestWrite(t, path, catalogueTestDocument("switched-owner", time.Now().Add(time.Hour).Unix()))
	if _, err := deps.resolveOwner(pinned.Alias); err == nil {
		t.Fatal("configured owner switch was accepted")
	}
}

type cloudClosingSocket struct{ *cloudTestSocket }

func (s cloudClosingSocket) ReadMessage() (int, []byte, error) {
	kind, raw, err := s.cloudTestSocket.ReadMessage()
	if err != nil {
		return kind, raw, fmt.Errorf("private wrapper: %w", &websocket.CloseError{Code: 1001, Text: "private-close-reason"})
	}
	return kind, raw, nil
}

func TestCloudReceiveClosureRecordsOnlySafeMetadata(t *testing.T) {
	h := newCloudTestHarness(t)
	original := h.deps.dial
	h.deps.dial = func(ctx context.Context, owner Account) (cloudSocket, int, error) {
		socket, code, err := original(ctx, owner)
		socket.(*cloudTestSocket).onSend = func(request cloudMessage) (bool, error) {
			if request.method() == "thread/resume" {
				socket.(*cloudTestSocket).output <- nil
				return true, nil
			}
			return false, nil
		}
		return cloudClosingSocket{socket.(*cloudTestSocket)}, code, err
	}
	h.start()
	h.initialize()
	h.send(2, "thread/resume", "secondary-root")
	h.await(2)
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	for _, event := range h.events {
		if event["receive_failure_type"] != "closed" {
			continue
		}
		if event["close_code"] != 1001 || event["reason_type"] != "ConnectionClosed" || event["lifetime_ms"].(int64) < 0 {
			t.Fatalf("closure metadata: %#v", event)
		}
		raw, _ := json.Marshal(event)
		if strings.Contains(string(raw), "private") {
			t.Fatal("raw error or close reason reached diagnostics")
		}
		return
	}
	t.Fatal("closure metadata unavailable")
}
