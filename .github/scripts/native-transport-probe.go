package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
	cqhttputil "github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const (
	localToken    = "cq-native-local"
	upstreamToken = "cq-native-upstream"
	accountID     = "cq-native-account"
	maxBodyBytes  = 1 << 20
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: native-transport-probe <serve|fixtures|probe>")
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "fixtures":
		err = fixtures(os.Args[2:])
	case "probe":
		err = probe(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command")
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addressFile := flags.String("address-file", "", "path receiving upstream URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *addressFile == "" || flags.NArg() != 0 {
		return errors.New("serve requires --address-file")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := writePrivateFile(*addressFile, []byte("http://"+listener.Addr().String()+"\n")); err != nil {
		return err
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(serveUpstream),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func serveUpstream(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+upstreamToken ||
		request.Header.Get("ChatGPT-Account-ID") != accountID ||
		!strings.HasSuffix(request.URL.Path, "/responses") {
		http.Error(writer, "invalid upstream authority", http.StatusUnauthorized)
		return
	}
	if websocket.IsWebSocketUpgrade(request) {
		serveUpstreamWebSocket(writer, request)
		return
	}
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBodyBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxBodyBytes || !bytes.Contains(body, []byte(`"response.create"`)) {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"cq-native-response\"}}\n\n")
	_, _ = io.WriteString(writer, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"PONG\"}]}}\n\n")
	_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"cq-native-response\",\"end_turn\":true,\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
}

func serveUpstreamWebSocket(writer http.ResponseWriter, request *http.Request) {
	connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	messageType, body, err := connection.ReadMessage()
	if err != nil || messageType != websocket.TextMessage || !bytes.Contains(body, []byte(`"response.create"`)) {
		return
	}
	_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"cq-native-ws-response"}}`))
	_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"cq-native-ws-response","end_turn":true,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`))
}

func fixtures(args []string) error {
	flags := flag.NewFlagSet("fixtures", flag.ContinueOnError)
	configPath := flags.String("config", "", "CQ proxy config path")
	authPath := flags.String("auth", "", "Codex auth path")
	stateRoot := flags.String("state-root", "", "CQ proxy resilience state root")
	upstream := flags.String("upstream", "", "synthetic upstream URL")
	port := flags.Int("port", 19280, "proxy port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	parsed, err := url.Parse(*upstream)
	if *configPath == "" || !filepath.IsAbs(*configPath) || *authPath == "" || !filepath.IsAbs(*authPath) {
		return errors.New("fixture output paths must be absolute")
	}
	if *stateRoot == "" || !filepath.IsAbs(*stateRoot) || filepath.Clean(*stateRoot) != *stateRoot {
		return errors.New("fixture state root must be a clean absolute path")
	}
	if err != nil || parsed == nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" {
		return errors.New("fixture upstream must be an HTTP origin")
	}
	if *port < 1 || *port > 65535 || flags.NArg() != 0 {
		return errors.New("fixture port or arguments are invalid")
	}
	config, err := json.Marshal(map[string]any{
		"port":                       *port,
		"claude_upstream":            *upstream,
		"codex_upstream":             strings.TrimRight(*upstream, "/") + "/codex",
		"local_token":                localToken,
		"proxy_resilience_state_dir": *stateRoot,
		"codex_turn_routing":         "off",
		"codex_ws_turn_routing":      "off",
	})
	if err != nil {
		return err
	}
	auth, err := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      syntheticJWT(),
			"access_token":  upstreamToken,
			"refresh_token": "cq-native-refresh",
			"account_id":    accountID,
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	if err := writePrivateFile(*configPath, append(config, '\n')); err != nil {
		return err
	}
	if err := os.MkdirAll(*stateRoot, 0o700); err != nil {
		return err
	}
	if err := proxy.InitialiseProxyResilienceState(context.Background(), proxy.ProxyResilienceStateOptions{
		FS: fsutil.OSFileSystem{}, Root: *stateRoot, Random: rand.Reader, Now: time.Now,
	}); err != nil {
		return err
	}
	return writePrivateFile(*authPath, append(auth, '\n'))
}

func syntheticJWT() string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"email": "cq-native@invalid.example",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "pro",
			"chatgpt_user_id":    "cq-native-user",
			"chatgpt_account_id": accountID,
		},
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func probe(args []string) error {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	address := flags.String("address", "http://127.0.0.1:19280", "installed proxy URL")
	token := flags.String("token", localToken, "local proxy token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	parsed, err := url.Parse(*address)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" || flags.NArg() != 0 {
		return errors.New("probe requires a loopback HTTP --address")
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil || host != "127.0.0.1" {
		return errors.New("probe address must be an explicit IPv4 loopback socket")
	}
	if err := probeHTTP(*address, *token); err != nil {
		return err
	}
	if err := probeWebSocket(*address, *token); err != nil {
		return err
	}
	_, err = fmt.Println(`{"http_sse":true,"websocket":true}`)
	return err
}

func probeHTTP(address, token string) error {
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":"ping","client_metadata":{"x-codex-turn-metadata":{"session_id":"native-http","thread_id":"native-http","turn_id":"native-http-1","request_kind":"turn"}}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address+"/responses", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := cqhttputil.ReadBody(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Contains(responseBody, []byte("PONG")) {
		return fmt.Errorf("installed HTTP/SSE probe failed: status=%d", response.StatusCode)
	}
	return nil
}

func probeWebSocket(address, token string) error {
	webSocketURL := "ws" + strings.TrimPrefix(address, "http") + "/responses"
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	connection, response, err := websocket.DefaultDialer.Dial(webSocketURL, header)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("installed WebSocket upgrade failed: %w", err)
	}
	defer connection.Close()
	deadline := time.Now().Add(15 * time.Second)
	_ = connection.SetReadDeadline(deadline)
	_ = connection.SetWriteDeadline(deadline)
	request := []byte(`{"type":"response.create","model":"gpt-5.4","input":[],"client_metadata":{"x-codex-turn-metadata":{"session_id":"native-ws","thread_id":"native-ws","turn_id":"native-ws-1","request_kind":"turn"}}}`)
	if err := connection.WriteMessage(websocket.TextMessage, request); err != nil {
		return err
	}
	for range 8 {
		messageType, body, err := connection.ReadMessage()
		if err != nil {
			return err
		}
		if messageType == websocket.TextMessage && bytes.Contains(body, []byte(`"response.completed"`)) {
			return nil
		}
	}
	return errors.New("installed WebSocket probe did not complete")
}

func writePrivateFile(path string, body []byte) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("output path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "native-transport-probe: "+format+"\n", args...)
	os.Exit(1)
}
