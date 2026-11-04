package zcode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// Explicit opt-in only. This test issues at most five harmless model requests:
// 128 paid + 128 Start + 512 tool + 512 continuation + 128 SSE = 1,408 output
// tokens requested. No claims, reset operations, UI actions or credential writes.
// Individual subtests may be selected with Go's -run expression.
func TestZCodeLiveGateway(t *testing.T) {
	if os.Getenv("CQ_ZCODE_LIVE") != "1" {
		t.Skip("set CQ_ZCODE_LIVE=1 to qualify the real ZCode gateway")
	}
	login, device := zcodeLiveLogin(t)
	observer := &zcodeLiveObserver{base: http.DefaultTransport, secrets: []string{login.ZCodeJWT, login.BusinessJWT, login.RawOAuth, login.RefreshToken}}
	client := httputil.NewClientWithTransport(50*time.Second, "zcode-qualification", observer)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	tokens := &ProjectTokenClient{HTTP: client, Version: "3.17.2"}
	defer tokens.Clear()
	gateway := &Gateway{HTTP: client, Signer: &Signer{HTTP: client, Version: ClientVersion}, Version: ClientVersion, DeviceMid: device, Catalogue: &ModelCatalogue{HTTP: client, Version: ClientVersion}}
	auth := GatewayAuth{ZCodeJWT: login.ZCodeJWT}
	sessionID := "zcode-qualification-" + rand.Text()
	request := func(t *testing.T, body map[string]any, selected GatewayAuth, limit int) zcodeLiveResponse {
		t.Helper()
		body["model"], body["max_output_tokens"], body["store"] = "glm-5.3-flash", limit, false
		body["reasoning"] = map[string]string{"effort": "low"}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal("construct qualification request")
		}
		observer.reset()
		callCtx, stop := context.WithTimeout(ctx, 45*time.Second)
		defer stop()
		if !selected.StartPlan {
			credential, err := tokens.Resolve(callCtx, login)
			if err != nil {
				var failure *ProjectTokenError
				status := 0
				if errors.As(err, &failure) {
					status = failure.HTTPStatus
				}
				t.Logf("status=%d", status)
				t.Fatal("own OAuth session could not resolve a project token; no saved API-key fallback")
			}
			selected.Credential = credential
			observer.mu.Lock()
			observer.secrets = append(observer.secrets, credential)
			observer.mu.Unlock()
		}
		response, err := gateway.Do(callCtx, raw, selected, sessionID)
		seen := observer.snapshot()
		if err != nil {
			var failure *Error
			if errors.As(err, &failure) && failure.HTTPStatus != 0 {
				seen.Status = failure.HTTPStatus
			}
			t.Logf("status=%d vendor_code=%d request_id=%q", seen.Status, seen.Code, seen.RequestID)
			t.Fatal("gateway request failed before a complete response")
		}
		defer response.Body.Close()
		t.Logf("status=%d vendor_code=%d request_id=%q", response.StatusCode, seen.Code, seen.RequestID)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if selected.StartPlan && (seen.Code == 3012 || response.StatusCode == http.StatusMethodNotAllowed) {
				t.Fatal("Start gateway rejected source-matched authentication; no retry performed")
			}
			t.Fatal("gateway rejected qualification request; no retry performed")
		}
		data, err := httputil.ReadBody(response.Body)
		if err != nil {
			t.Fatal("gateway stream or response did not finish within bounds")
		}
		var result zcodeLiveResponse
		if response.Header.Get("Content-Type") == "text/event-stream" {
			result = zcodeLiveTerminal(t, data)
		} else if json.Unmarshal(data, &result) != nil {
			t.Fatal("gateway returned malformed native response")
		}
		if result.Usage != nil {
			t.Logf("input_tokens=%d output_tokens=%d total_tokens=%d", result.Usage.Input, result.Usage.Output, result.Usage.Total)
		}
		if result.Object != "response" || result.Status != "completed" || result.Usage == nil || result.Usage.Input < 0 || result.Usage.Output < 0 || result.Usage.Output > int64(limit) || result.Usage.Total != result.Usage.Input+result.Usage.Output {
			t.Fatal("model did not complete with valid actual usage within requested output cap")
		}
		return result
	}
	if !t.Run("paid", func(t *testing.T) {
		result := request(t, map[string]any{"input": "Reply exactly ZCODE_PAID_OK. Do not use tools."}, auth, 128)
		if zcodeLiveText(result) != "ZCODE_PAID_OK" {
			t.Fatal("paid marker response did not match")
		}
	}) {
		return
	}
	if !t.Run("start", func(t *testing.T) {
		selected := auth
		selected.StartPlan = true
		result := request(t, map[string]any{"input": "Reply exactly ZCODE_START_OK. Do not use tools."}, selected, 128)
		if zcodeLiveText(result) != "ZCODE_START_OK" {
			t.Fatal("Start marker response did not match")
		}
	}) {
		return
	}
	if !t.Run("tools", func(t *testing.T) {
		tools := []any{map[string]any{"type": "function", "name": "zcode_probe", "namespace": "functions", "description": "Return a harmless qualification marker without accessing any external resources.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"marker": map[string]string{"type": "string"}}, "required": []string{"marker"}, "additionalProperties": false}}}
		initial, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": "Call functions.zcode_probe once with marker ZCODE_TOOL. After its result, reply exactly ZCODE_TOOL_DONE."})
		history := []json.RawMessage{initial}
		first := request(t, map[string]any{"input": history, "tools": tools, "tool_choice": map[string]string{"type": "function", "name": "zcode_probe", "namespace": "functions"}}, auth, 512)
		callID := ""
		for _, item := range first.Output {
			var call struct {
				Type, Name, Namespace, Arguments string
				CallID                           string `json:"call_id"`
			}
			if json.Unmarshal(item, &call) != nil {
				t.Fatal("malformed tool response item")
			}
			if call.Type == "function_call" {
				var args struct {
					Marker string `json:"marker"`
				}
				if callID != "" || call.Name != "zcode_probe" || call.Namespace != "functions" || call.CallID == "" || json.Unmarshal([]byte(call.Arguments), &args) != nil || args.Marker != "ZCODE_TOOL" {
					t.Fatal("tool call did not match harmless qualification function")
				}
				callID = call.CallID
			}
			// Preserve every real reasoning/signature and tool item byte-for-byte.
			history = append(history, item)
		}
		if callID == "" {
			t.Fatal("model did not issue the requested tool call")
		}
		result, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": callID, "output": "ZCODE_TOOL_RESULT"})
		history = append(history, result)
		follow := request(t, map[string]any{"input": history, "tools": tools, "tool_choice": "none"}, auth, 512)
		if zcodeLiveText(follow) != "ZCODE_TOOL_DONE" {
			t.Fatal("real tool-result continuation did not finish the marker task")
		}
	}) {
		return
	}
	t.Run("sse", func(t *testing.T) {
		result := request(t, map[string]any{"input": "Reply exactly ZCODE_SSE_OK. Do not use tools.", "stream": true}, auth, 128)
		if zcodeLiveText(result) != "ZCODE_SSE_OK" {
			t.Fatal("SSE marker response did not match")
		}
	})
}

type zcodeLiveResponse struct {
	Object, Status string
	Output         []json.RawMessage `json:"output"`
	Usage          *struct {
		Input  int64 `json:"input_tokens"`
		Output int64 `json:"output_tokens"`
		Total  int64 `json:"total_tokens"`
	} `json:"usage"`
}

func zcodeLiveText(response zcodeLiveResponse) string {
	var text strings.Builder
	for _, item := range response.Output {
		var message struct {
			Type    string
			Content []struct{ Type, Text string }
		}
		if json.Unmarshal(item, &message) != nil || message.Type != "message" {
			continue
		}
		for _, content := range message.Content {
			if content.Type == "output_text" {
				text.WriteString(content.Text)
			}
		}
	}
	return strings.TrimSpace(text.String())
}

func zcodeLiveTerminal(t *testing.T, raw []byte) zcodeLiveResponse {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 32<<10), httputil.MaxResponseBody)
	var result zcodeLiveResponse
	completed := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type     string
			Response zcodeLiveResponse
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			t.Fatal("malformed native SSE event")
		}
		if event.Type == "response.failed" || event.Type == "response.incomplete" {
			t.Fatal("native SSE did not complete")
		}
		if event.Type == "response.completed" {
			if completed {
				t.Fatal("duplicate native completion")
			}
			completed, result = true, event.Response
		}
	}
	if scanner.Err() != nil || !completed {
		t.Fatal("native SSE terminal event missing")
	}
	return result
}

func zcodeLiveLogin(t *testing.T) (LoginSession, string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("resolve own home directory")
	}
	state, err := os.ReadFile(filepath.Join(home, ".zcode", "v2", "telemetry-state.json"))
	var telemetry struct {
		DeviceMid string `json:"deviceMid"`
	}
	if err != nil || len(state) > httputil.MaxResponseBody || json.Unmarshal(state, &telemetry) != nil || !validHeaderValue(telemetry.DeviceMid, 256) {
		t.Fatal("existing ZCode device identity unavailable")
	}
	if config := os.Getenv("CQ_ZCODE_LIVE_CONFIG"); config != "" {
		store, err := OpenSessionStore(fsutil.OSFileSystem{}, config, false)
		if err != nil {
			t.Fatal("existing CQ OAuth store unavailable")
		}
		accounts, err := store.Accounts()
		if err != nil || len(accounts) == 0 {
			t.Fatal("existing CQ OAuth login unavailable")
		}
		return accounts[0], telemetry.DeviceMid
	}
	secret := os.Getenv("ZCODE_CREDENTIAL_SECRET")
	if secret == "" {
		username := "unknown"
		if current, err := user.Current(); err == nil {
			username = current.Username
		}
		platform := runtime.GOOS
		if platform == "windows" {
			platform = "win32"
		}
		secret = "zcode-credential-fallback:" + platform + ":" + home + ":" + username
	}
	session, err := (SessionReader{FS: fsutil.OSFileSystem{}, Path: filepath.Join(home, ".zcode", "v2", "credentials.json"), CipherSecret: secret}).Read()
	if err != nil {
		t.Fatal("existing installed ZCode Z.ai login unavailable")
	}
	if session.PlaintextCredentials {
		t.Log("status=plaintext_credential_store")
	}
	parts := strings.Split(session.ZCodeJWT, ".")
	if len(parts) != 3 {
		t.Fatal("ZCode login identity unavailable")
	}
	profile, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		t.Fatal("ZCode login identity malformed")
	}
	defer clear(profile)
	var claims struct {
		UserID string `json:"user_id"`
	}
	if json.Unmarshal(profile, &claims) != nil || !validHeaderValue(claims.UserID, 512) {
		t.Fatal("ZCode login user identity unavailable")
	}
	return LoginSession{ID: loginAccountID(claims.UserID), UserID: claims.UserID, ZCodeJWT: session.ZCodeJWT, BusinessJWT: session.BusinessJWT}, telemetry.DeviceMid
}

type zcodeLiveObservation struct {
	Status, Code int
	RequestID    string
}
type zcodeLiveObserver struct {
	base    http.RoundTripper
	mu      sync.Mutex
	last    zcodeLiveObservation
	secrets []string
}

func (o *zcodeLiveObserver) reset() { o.mu.Lock(); o.last = zcodeLiveObservation{}; o.mu.Unlock() }
func (o *zcodeLiveObserver) snapshot() zcodeLiveObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.last
}
func (o *zcodeLiveObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	rsp, err := o.base.RoundTrip(req)
	if err != nil || rsp == nil || req.URL.Host != "zcode.z.ai" || !strings.HasSuffix(req.URL.Path, "/anthropic/v1/messages") {
		return rsp, err
	}
	seen := zcodeLiveObservation{Status: rsp.StatusCode, RequestID: rsp.Header.Get("X-Request-Id")}
	if len(seen.RequestID) > 128 || !zcodeLiveRequestID(seen.RequestID) {
		seen.RequestID = ""
	}
	o.mu.Lock()
	for _, secret := range o.secrets {
		if secret != "" && strings.Contains(seen.RequestID, secret) {
			seen.RequestID = ""
		}
	}
	o.mu.Unlock()
	if (rsp.StatusCode < 200 || rsp.StatusCode >= 300) && rsp.Body != nil {
		raw, readErr := httputil.ReadBody(rsp.Body)
		_ = rsp.Body.Close()
		if readErr != nil {
			raw = nil
		}
		rsp.Body = io.NopCloser(bytes.NewReader(raw))
		var failure struct {
			Code  json.RawMessage
			Error struct{ Code json.RawMessage }
		}
		if json.Unmarshal(raw, &failure) == nil {
			code := failure.Code
			if len(code) == 0 {
				code = failure.Error.Code
			}
			var text string
			if json.Unmarshal(code, &text) != nil {
				text = string(code)
			}
			if number, err := strconv.Atoi(text); err == nil {
				seen.Code = number
			}
		}
	}
	o.mu.Lock()
	o.last = seen
	o.mu.Unlock()
	return rsp, err
}
func zcodeLiveRequestID(value string) bool {
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}
