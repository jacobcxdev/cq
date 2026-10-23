package dots

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
)

type nativeEvaluator struct {
	settings        *Settings
	verification    sync.Mutex
	verified        bool
	archiveRevision fileRevision
}

func NewNativeEvaluator(s *Settings) NativeEvaluator { return &nativeEvaluator{settings: s} }

func (e *nativeEvaluator) check() error {
	e.verification.Lock()
	defer e.verification.Unlock()
	info, err := os.Stat(filepath.Join(e.settings.AppPath, "Contents", "Resources", "app.asar"))
	if err != nil {
		return err
	}
	current := revision(info)
	if e.verified && current == e.archiveRevision {
		return nil
	}
	if err = AppCheck(e.settings); err != nil {
		return err
	}
	e.archiveRevision = current
	e.verified = true
	return nil
}
func (e *nativeEvaluator) EvaluateAll(ctx context.Context, expression string) ([]any, error) {
	s := e.settings
	if s == nil || expression == "" {
		return nil, errors.New("native debugger expression invalid")
	}
	if len(expression) > nativeMaxRequest-1024 {
		return nil, errors.New("native debugger expression too large")
	}
	if err := e.check(); err != nil {
		return nil, err
	}
	if s.IntegrationMode == "native" {
		pid, err := nativeProcessPID(ctx, s, 0)
		if err != nil {
			return nil, err
		}
		response, err := nativeRequest(ctx, s, map[string]any{"operation": "evaluate_all", "app_pid": pid, "expression": expression})
		if err != nil {
			return nil, err
		}
		if response["app_pid"] != float64(pid) {
			return nil, errors.New("native debugger app identity changed")
		}
		values, ok := response["value"].([]any)
		if !ok {
			return nil, errors.New("native renderer result invalid")
		}
		return values, nil
	}
	value, err := e.evaluateIsolated(ctx, expression)
	if err != nil {
		return nil, err
	}
	return []any{value}, nil
}
func debuggerURL(value string, port int) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "ws" || u.Hostname() != "127.0.0.1" || u.Port() != strconv.Itoa(port) || u.User != nil || u.Fragment != "" || len(u.Path) <= len("/devtools/page/") || u.Path[:len("/devtools/page/")] != "/devtools/page/" {
		return "", errors.New("isolated debugger URL invalid")
	}
	return value, nil
}
func (e *nativeEvaluator) evaluateIsolated(ctx context.Context, expression string) (any, error) {
	data, err := ReadOwned(filepath.Join(e.settings.RuntimeRoot, "launch.json"), e.settings.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	var launch struct {
		Port int `json:"debug_port"`
	}
	if json.Unmarshal(data, &launch) != nil || launch.Port <= 1024 || launch.Port >= 65536 {
		return nil, errors.New("invalid isolated debugger port")
	}
	if _, err = nativeProcessPID(ctx, e.settings, launch.Port); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, nativeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(launch.Port)+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("isolated debugger target unavailable")
	}
	defer response.Body.Close()
	body, err := httputil.ReadBody(response.Body)
	if err != nil || response.StatusCode != 200 {
		return nil, errors.New("isolated debugger target invalid")
	}
	var targets []struct {
		Type      string `json:"type"`
		URL       string `json:"url"`
		ID        string `json:"id"`
		WebSocket string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(body, &targets) != nil {
		return nil, errors.New("isolated debugger target invalid")
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	var matches []string
	for _, target := range targets {
		if target.Type == "page" && target.URL == "app://-/index.html" {
			matches = append(matches, target.WebSocket)
		}
	}
	if len(matches) != 1 {
		return nil, errors.New("native renderer selection ambiguous")
	}
	endpoint, err := debuggerURL(matches[0], launch.Port)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Proxy: nil}
	ws, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, errors.New("isolated debugger unavailable")
	}
	defer ws.Close()
	ws.SetReadLimit(nativeMaxFrame)
	deadline, _ := ctx.Deadline()
	_ = ws.SetReadDeadline(deadline)
	_ = ws.SetWriteDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	if err = ws.WriteJSON(map[string]any{"id": 1, "method": "Runtime.evaluate", "params": map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}}); err != nil {
		return nil, errors.New("native API invocation failed")
	}
	for {
		var value map[string]any
		if err = ws.ReadJSON(&value); err != nil {
			return nil, errors.New("native API invocation failed")
		}
		if value["id"] != float64(1) {
			continue
		}
		result, ok := value["result"].(map[string]any)
		if !ok || value["error"] != nil || result["exceptionDetails"] != nil {
			return nil, errors.New("native API invocation failed")
		}
		remote, ok := result["result"].(map[string]any)
		if !ok {
			return nil, errors.New("native API invocation failed")
		}
		return remote["value"], nil
	}
}
