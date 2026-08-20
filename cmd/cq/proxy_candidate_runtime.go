package main

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const candidateRuntimeControlHeader = "X-CQ-Candidate-Control"

var candidateRuntimeExecutable = os.Executable
var candidateRuntimeCommand = exec.Command

type candidateRuntimeHealthV1 struct {
	SchemaVersion   int    `json:"schema_version"`
	Kind            string `json:"kind"`
	ProxyInstanceID string `json:"proxy_instance_id"`
	ValidationRunID string `json:"validation_run_id"`
	Generation      uint64 `json:"generation"`
}

func candidateRuntimeArguments(state proxy.CandidateLifecycleStateV1) []string {
	return []string{
		"proxy", "candidate", "__runtime",
		"--instance", state.ProxyInstanceID,
		"--validation-run", state.ValidationRunID,
		"--generation", strconv.FormatUint(state.Generation, 10),
		"--port", strconv.Itoa(state.Port),
		"--token-fd", "3",
	}
}

func isCandidateRuntimeCommand(args []string) bool {
	return len(args) >= 3 && args[0] == "proxy" && args[1] == "candidate" && args[2] == "__runtime"
}

func startCandidateRuntime(ctx context.Context, _ string, state proxy.CandidateLifecycleStateV1, token []byte) ([]byte, error) {
	if len(token) != 32 || state.Port == proxy.DefaultPort {
		return nil, proxy.ErrCandidateLifecycleInvalid
	}
	if health, err := inspectCandidateRuntime(ctx, state.Port, token); err == nil {
		if health.ProxyInstanceID != state.ProxyInstanceID || health.ValidationRunID != state.ValidationRunID {
			return nil, errors.New("candidate port is owned by another runtime")
		}
		return candidateRuntimeReceipt("running", health), nil
	}
	executable, err := candidateRuntimeExecutable()
	if err != nil {
		return nil, err
	}
	tokenReader, tokenWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer tokenReader.Close()
	command := candidateRuntimeCommand(executable, candidateRuntimeArguments(state)...)
	command.ExtraFiles = []*os.File{tokenReader}
	if err := command.Start(); err != nil {
		_ = tokenWriter.Close()
		return nil, err
	}
	kill := true
	defer func() {
		if kill {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	}()
	if _, err := tokenWriter.Write(token); err != nil {
		_ = tokenWriter.Close()
		return nil, err
	}
	if err := tokenWriter.Close(); err != nil {
		return nil, err
	}
	_ = tokenReader.Close()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		health, healthErr := inspectCandidateRuntime(ctx, state.Port, token)
		if healthErr == nil {
			if health.ProxyInstanceID != state.ProxyInstanceID || health.ValidationRunID != state.ValidationRunID {
				return nil, errors.New("candidate runtime identity mismatch")
			}
			if err := command.Process.Release(); err != nil {
				return nil, err
			}
			kill = false
			return candidateRuntimeReceipt("running", health), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func stopCandidateRuntime(ctx context.Context, _ string, state proxy.CandidateLifecycleStateV1, token []byte) ([]byte, error) {
	health, err := inspectCandidateRuntime(ctx, state.Port, token)
	if err != nil {
		return nil, fmt.Errorf("candidate runtime unavailable: %w", err)
	}
	if health.ProxyInstanceID != state.ProxyInstanceID || health.ValidationRunID != state.ValidationRunID {
		return nil, errors.New("candidate runtime identity mismatch")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, candidateRuntimeURL(state.Port, "/__cq_candidate_control/stop"), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set(candidateRuntimeControlHeader, hex.EncodeToString(token))
	response, err := candidateRuntimeHTTPClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("candidate runtime stop returned %s", response.Status)
	}
	var stopped candidateRuntimeHealthV1
	if err := json.Unmarshal(body, &stopped); err != nil || stopped != health {
		return nil, errors.New("candidate runtime stop identity mismatch")
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, inspectErr := inspectCandidateRuntime(ctx, state.Port, token); inspectErr != nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
	return candidateRuntimeReceipt("stopped", stopped), nil
}

func inspectCandidateRuntime(ctx context.Context, port int, token []byte) (candidateRuntimeHealthV1, error) {
	var health candidateRuntimeHealthV1
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, candidateRuntimeURL(port, "/health"), nil)
	if err != nil {
		return health, err
	}
	request.Header.Set(candidateRuntimeControlHeader, hex.EncodeToString(token))
	response, err := candidateRuntimeHTTPClient().Do(request)
	if err != nil {
		return health, err
	}
	defer response.Body.Close()
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return health, err
	}
	if response.StatusCode != http.StatusOK {
		return health, fmt.Errorf("candidate runtime health returned %s", response.Status)
	}
	if err := json.Unmarshal(body, &health); err != nil || health.SchemaVersion != 1 || health.Kind != "candidate_runtime_health_v1" {
		return candidateRuntimeHealthV1{}, errors.New("candidate runtime health invalid")
	}
	return health, nil
}

func candidateRuntimeHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DisableCompression: true, DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil || host != "127.0.0.1" {
			return nil, errors.New("candidate runtime address invalid")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}}}
}

func candidateRuntimeURL(port int, path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + path
}

func runCandidateRuntimeChild(ctx context.Context, args []string) error {
	health, tokenFD, err := parseCandidateRuntimeArguments(args)
	if err != nil {
		return err
	}
	tokenFile := os.NewFile(uintptr(tokenFD), "candidate-runtime-token")
	if tokenFile == nil {
		return errors.New("candidate runtime token descriptor unavailable")
	}
	token := make([]byte, 32)
	defer zeroCandidateBytes(token)
	if _, err := io.ReadFull(tokenFile, token); err != nil {
		_ = tokenFile.Close()
		return err
	}
	extra := make([]byte, 1)
	if count, err := tokenFile.Read(extra); count != 0 || !errors.Is(err, io.EOF) {
		_ = tokenFile.Close()
		return errors.New("candidate runtime token framing invalid")
	}
	if err := tokenFile.Close(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(healthPort(args)))
	if err != nil {
		return err
	}
	defer listener.Close()
	stop := make(chan struct{}, 1)
	authenticated := func(request *http.Request) bool {
		provided, err := hex.DecodeString(request.Header.Get(candidateRuntimeControlHeader))
		return err == nil && hmac.Equal(provided, token)
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !authenticated(request) {
			http.Error(writer, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/health":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(health)
		case request.Method == http.MethodPost && request.URL.Path == "/__cq_candidate_control/stop":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(health)
			select {
			case stop <- struct{}{}:
			default:
			}
		default:
			http.NotFound(writer, request)
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case <-stop:
	case serveErr := <-serveDone:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	serveErr := <-serveDone
	if !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

func parseCandidateRuntimeArguments(args []string) (candidateRuntimeHealthV1, int, error) {
	var health candidateRuntimeHealthV1
	if len(args) != 11 || args[0] != "__runtime" || args[1] != "--instance" || args[3] != "--validation-run" || args[5] != "--generation" || args[7] != "--port" || args[9] != "--token-fd" {
		return health, 0, errors.New("candidate runtime arguments invalid")
	}
	generation, generationErr := strconv.ParseUint(args[6], 10, 64)
	port, portErr := strconv.Atoi(args[8])
	tokenFD, tokenErr := strconv.Atoi(args[10])
	if !lowerHexArgument(args[2], 32) || !lowerHexArgument(args[4], 64) || generationErr != nil || generation == 0 || portErr != nil || port < 1 || port > 65535 || port == proxy.DefaultPort || tokenErr != nil || tokenFD != 3 {
		return health, 0, errors.New("candidate runtime arguments invalid")
	}
	health = candidateRuntimeHealthV1{SchemaVersion: 1, Kind: "candidate_runtime_health_v1", ProxyInstanceID: args[2], ValidationRunID: args[4], Generation: generation}
	return health, tokenFD, nil
}

func healthPort(args []string) int {
	port, _ := strconv.Atoi(args[8])
	return port
}

func candidateRuntimeReceipt(state string, health candidateRuntimeHealthV1) []byte {
	return []byte(state + "\x00" + health.ProxyInstanceID + "\x00" + health.ValidationRunID + "\x00" + strconv.FormatUint(health.Generation, 10))
}
