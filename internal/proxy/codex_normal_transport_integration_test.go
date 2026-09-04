//go:build !windows

package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/klauspost/compress/zstd"
)

type normalTransportGateScenario uint8

const (
	normalTransportGateHTTPSuccess normalTransportGateScenario = iota + 1
	normalTransportGateHTTPAuthRetry
	normalTransportGateHTTPForbiddenRetry
	normalTransportGateHTTPAdmittedAuthRetry
	normalTransportGateHTTPPortableUnauthorized
	normalTransportGateHTTPPortableForbidden
	normalTransportGateHTTPHardLimit
	normalTransportGateHTTPSoftLimit
	normalTransportGateHTTPAllHardLimit
	normalTransportGateWSApplicationHardLimit
	normalTransportGateWSApplicationUnauthorized
	normalTransportGateWSApplicationForbidden
	normalTransportGateWSHandshakeHardLimit
	normalTransportGateWSHandshakeUnauthorized
	normalTransportGateWSHandshakeForbidden
	normalTransportGateWSNonPortableApplicationHardLimit
	normalTransportGateWSNonPortableHandshakeHardLimit
	normalTransportGateWSAdmittedHardLimit
	normalTransportGateWSAdmittedUnauthorized
	normalTransportGateWSAdmittedForbidden
	normalTransportGateWSSequentialAdmittedHardLimit
	normalTransportGateWSSequentialAdmittedUnauthorized
	normalTransportGateWSSequentialAdmittedForbidden
	normalTransportGateWSSequentialHandshakeUnauthorized
	normalTransportGateWSSequentialHandshakeForbidden
	normalTransportGateWSAllAdmittedUnauthorized
	normalTransportGateWSAllAdmittedForbidden
	normalTransportGateWSAllHandshakeUnauthorized
	normalTransportGateWSAllHandshakeForbidden
	normalTransportGateWSAllApplicationHardLimit
	normalTransportGateWSAllHandshakeHardLimit
	normalTransportGateWSNonPortableAllApplicationHardLimit
	normalTransportGateWSNonPortableAllHandshakeHardLimit
	normalTransportGateWSNonPortableTwoApplicationHardLimit
	normalTransportGateWSNonPortableTwoHandshakeHardLimit
	normalTransportGateWSRefreshSuccess
	normalTransportGateWSRefreshFailure
	normalTransportGateWSDirectAuthRefreshSuccess
	normalTransportGateWSDirectAuthRefreshFailure
	normalTransportGateWSDirectAuthRefreshAllFailure
	normalTransportGateWSCredentialAuthThenSuccess
	normalTransportGateWSCredentialAuthThenHardLimit
	normalTransportGateWSCredentialAllUnauthorized
	normalTransportGateWSCredentialAllForbidden
	normalTransportGateWSIdleEOF
	normalTransportGateWSIdleHeldOpen
	normalTransportGateWSIdleCloseAfterPrecheck
	normalTransportGateWSResponseAnchorSocketBound
	normalTransportGateWSResponseAnchorSocketClosed
	normalTransportGateWSPrewarmStall
	normalTransportGateAccessTokenOutlivesIDToken
)

type normalTransportGateReceipt struct {
	transport  string
	accountID  string
	candidate  string
	status     int
	payload    string
	connection int
}

type normalTransportGateBackend struct {
	scenario normalTransportGateScenario

	mu                sync.Mutex
	receipts          []normalTransportGateReceipt
	authorizations    []string
	httpAuthRecovered bool
	httpAuthActive    bool
	httpAuthStatus    int
	wsConnections     int
	failures          chan error
	firstWSClosed     chan struct{}
	firstWSCloseOnce  sync.Once
	closeFirstWS      chan struct{}
	closeFirstWSOnce  sync.Once
	wsAuthChainDone   bool
	wsHandshakeBody   []byte
	wsHandshakeCoding string
}

func (backend *normalTransportGateBackend) recordAuthorization(authorization string) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.authorizations = append(backend.authorizations, authorization)
}

func (backend *normalTransportGateBackend) authorizationSnapshot() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]string(nil), backend.authorizations...)
}

func newNormalTransportGateBackend(scenario normalTransportGateScenario) *normalTransportGateBackend {
	return &normalTransportGateBackend{
		scenario:      scenario,
		failures:      make(chan error, 8),
		firstWSClosed: make(chan struct{}),
		closeFirstWS:  make(chan struct{}),
	}
}

func normalTransportGateCandidateLabel(authorization string) string {
	switch authorization {
	case "Bearer validation-token-a-one":
		return "validation-candidate-a-one"
	case "Bearer validation-token-a-two":
		return "validation-candidate-a-two"
	case "Bearer validation-token-a-stale":
		return "validation-candidate-a-stale"
	case "Bearer validation-token-a-refreshed":
		return "validation-candidate-a-refreshed"
	default:
		return "validation-candidate"
	}
}

func (backend *normalTransportGateBackend) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.EscapedPath() != legacyCodexResponsesPath {
		backend.fail(writer, http.StatusNotFound, fmt.Errorf("provider path = %q", request.URL.EscapedPath()))
		return
	}
	accountID := request.Header.Get("ChatGPT-Account-ID")
	authorization := request.Header.Get("Authorization")
	backend.recordAuthorization(authorization)
	if accountID == "" || !strings.HasPrefix(authorization, "Bearer validation-token-") {
		backend.fail(writer, http.StatusUnauthorized, errors.New("provider authority unavailable"))
		return
	}
	if backend.scenario == normalTransportGateWSRefreshSuccess && accountID == "validation-upstream-a" && request.Header.Get("Authorization") != "Bearer validation-token-a-refreshed" {
		backend.fail(writer, http.StatusUnauthorized, errors.New("stale refresh-only credential reached provider"))
		return
	}
	if backend.scenario == normalTransportGateWSRefreshFailure && accountID == "validation-upstream-a" {
		backend.fail(writer, http.StatusUnauthorized, errors.New("refresh-failed credential reached provider"))
		return
	}
	if websocket.IsWebSocketUpgrade(request) {
		backend.serveWebSocket(writer, request, accountID, normalTransportGateCandidateLabel(authorization))
		return
	}
	backend.serveHTTP(writer, request, accountID)
}

func (backend *normalTransportGateBackend) serveHTTP(writer http.ResponseWriter, request *http.Request, accountID string) {
	if request.Method != http.MethodPost {
		backend.fail(writer, http.StatusMethodNotAllowed, fmt.Errorf("provider method = %q", request.Method))
		return
	}
	payload, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody+1))
	if err != nil || len(payload) > maxRequestBody {
		backend.fail(writer, http.StatusBadRequest, errors.New("provider request unreadable"))
		return
	}
	defer clearBytes(payload)

	status := http.StatusOK
	backend.mu.Lock()
	authRecovered := backend.httpAuthRecovered
	backend.mu.Unlock()
	switch backend.scenario {
	case normalTransportGateHTTPAuthRetry:
		if !authRecovered {
			status = http.StatusUnauthorized
		}
	case normalTransportGateHTTPForbiddenRetry:
		if !authRecovered {
			status = http.StatusForbidden
		}
	case normalTransportGateHTTPAdmittedAuthRetry:
		backend.mu.Lock()
		if backend.httpAuthActive {
			status = backend.httpAuthStatus
		}
		backend.mu.Unlock()
	case normalTransportGateHTTPPortableUnauthorized:
		if accountID == "validation-upstream-a" {
			status = http.StatusUnauthorized
		}
	case normalTransportGateHTTPPortableForbidden:
		if accountID == "validation-upstream-a" {
			status = http.StatusForbidden
		}
	case normalTransportGateHTTPHardLimit:
		if accountID == "validation-upstream-a" {
			status = http.StatusTooManyRequests
		}
	case normalTransportGateHTTPSoftLimit:
		if accountID == "validation-upstream-a" {
			status = http.StatusTooManyRequests
		}
	case normalTransportGateHTTPAllHardLimit:
		status = http.StatusTooManyRequests
	}
	backend.record(normalTransportGateReceipt{
		transport: "http",
		accountID: accountID,
		status:    status,
		payload:   string(payload),
	})

	writer.Header().Set("Content-Type", "application/json")
	switch status {
	case http.StatusUnauthorized:
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, `{"error":{"type":"authentication_error"}}`)
	case http.StatusForbidden:
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, `{"error":{"type":"authentication_error"}}`)
	case http.StatusTooManyRequests:
		writer.WriteHeader(status)
		if backend.scenario == normalTransportGateHTTPSoftLimit {
			_, _ = io.WriteString(writer, `{"error":{"type":"rate_limit_exceeded"}}`)
		} else {
			_, _ = io.WriteString(writer, `{"error":{"type":"usage_limit_reached"}}`)
		}
	default:
		writer.Header().Set("Content-Type", "text/event-stream")
		encryptedState := ""
		if normalTransportGateNonPortableWebSocketScenario(backend.scenario) {
			encryptedState = `,"encrypted_content":"opaque-normal-transport-state"`
		}
		_, _ = fmt.Fprintf(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"normal-transport-http\"%s}}\n\n", encryptedState)
		_, _ = fmt.Fprintf(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"normal-transport-http\",\"end_turn\":true%s}}\n\n", encryptedState)
	}
}

func (backend *normalTransportGateBackend) serveWebSocket(writer http.ResponseWriter, request *http.Request, accountID, candidate string) {
	if status := backend.webSocketHandshakeFailureStatus(accountID, candidate); status != 0 {
		backend.record(normalTransportGateReceipt{transport: "websocket_handshake", accountID: accountID, candidate: candidate, status: status})
		errorType := "authentication_error"
		if status == http.StatusTooManyRequests {
			errorType = "usage_limit_reached"
		}
		body := []byte(fmt.Sprintf(`{"error":{"type":%q}}`, errorType))
		if len(backend.wsHandshakeBody) != 0 {
			body = backend.wsHandshakeBody
		}
		writer.Header().Set("Content-Type", "application/json")
		if backend.wsHandshakeCoding != "" {
			writer.Header().Set("Content-Encoding", backend.wsHandshakeCoding)
		}
		writer.WriteHeader(status)
		switch backend.wsHandshakeCoding {
		case "gzip":
			encoded := gzip.NewWriter(writer)
			_, writeErr := encoded.Write(body)
			if err := errors.Join(writeErr, encoded.Close()); err != nil {
				backend.recordFailure(err)
			}
		case "zstd":
			encoded, encodeErr := zstd.NewWriter(writer)
			if encodeErr != nil {
				backend.recordFailure(encodeErr)
				break
			}
			_, writeErr := encoded.Write(body)
			if err := errors.Join(writeErr, encoded.Close()); err != nil {
				backend.recordFailure(err)
			}
		default:
			if _, err := writer.Write(body); err != nil {
				backend.recordFailure(err)
			}
		}
		backend.finishWebSocketAuthFailureCycle(accountID)
		return
	}
	upgrader := websocket.Upgrader{
		CheckOrigin:       func(*http.Request) bool { return true },
		Subprotocols:      websocket.Subprotocols(request),
		EnableCompression: true,
	}
	connection, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		backend.recordFailure(fmt.Errorf("provider WebSocket upgrade: %w", err))
		return
	}
	defer connection.Close()
	backend.mu.Lock()
	backend.wsConnections++
	connectionIndex := backend.wsConnections
	backend.mu.Unlock()
	backend.record(normalTransportGateReceipt{
		transport:  "websocket_handshake",
		accountID:  accountID,
		candidate:  candidate,
		status:     http.StatusSwitchingProtocols,
		connection: connectionIndex,
	})

	messageType, payload, err := connection.ReadMessage()
	if err != nil {
		backend.recordFailure(fmt.Errorf("provider WebSocket read: %w", err))
		return
	}
	if messageType != websocket.TextMessage {
		backend.recordFailure(fmt.Errorf("provider WebSocket message type = %d", messageType))
		return
	}
	backend.record(normalTransportGateReceipt{
		transport:  "websocket_frame",
		accountID:  accountID,
		candidate:  candidate,
		payload:    string(payload),
		connection: connectionIndex,
	})
	if backend.scenario == normalTransportGateWSPrewarmStall && connectionIndex == 1 {
		var request struct {
			Generate *bool `json:"generate"`
		}
		if err := json.Unmarshal(payload, &request); err != nil || request.Generate == nil || *request.Generate {
			backend.recordFailure(fmt.Errorf("provider stalled WebSocket request was not prewarm"))
			return
		}
		_, _, _ = connection.ReadMessage()
		return
	}
	if backend.scenario == normalTransportGateWSResponseAnchorSocketBound {
		backend.serveWebSocketResponseAnchorSocketBound(connection, connectionIndex, payload)
		return
	}
	if backend.scenario == normalTransportGateWSResponseAnchorSocketClosed {
		backend.serveWebSocketResponseAnchorSocketClosed(connection, connectionIndex, payload)
		return
	}
	if status := backend.preAdmissionWebSocketFailureStatus(accountID); status != 0 {
		if err := connection.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"error","status":%d,"error":{"type":"authentication_error"}}`, status))); err != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket pre-admission failure write: %w", err))
		}
		return
	}
	if status := backend.admittedWebSocketFailureStatus(accountID); status != 0 {
		if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"normal-transport-admitted"}}`)); err != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket admitted response write: %w", err))
			return
		}
		errorType := "authentication_error"
		if status == http.StatusTooManyRequests {
			errorType = "usage_limit_reached"
		}
		if err := connection.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"error","status":%d,"error":{"type":%q}}`, status, errorType))); err != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket admitted failure write: %w", err))
		}
		backend.finishWebSocketAuthFailureCycle(accountID)
		return
	}

	if backend.rejectWebSocketApplication(accountID) {
		if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`)); err != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket hard-limit write: %w", err))
		}
		return
	}
	if backend.scenario == normalTransportGateWSIdleEOF && connectionIndex == 1 {
		backend.writeWebSocketCompletion(connection, "normal-transport-idle-one")
		_ = connection.Close()
		backend.firstWSCloseOnce.Do(func() { close(backend.firstWSClosed) })
		return
	}
	if backend.scenario == normalTransportGateWSIdleCloseAfterPrecheck && connectionIndex == 1 {
		backend.writeWebSocketCompletion(connection, "normal-transport-idle-race-one")
		<-backend.closeFirstWS
		_ = connection.Close()
		backend.firstWSCloseOnce.Do(func() { close(backend.firstWSClosed) })
		return
	}
	if backend.scenario == normalTransportGateWSIdleHeldOpen && connectionIndex == 1 {
		backend.writeWebSocketCompletion(connection, "normal-transport-idle-held-one")
		messageType, successor, readErr := connection.ReadMessage()
		if readErr != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket held-idle successor read: %w", readErr))
			return
		}
		if messageType != websocket.TextMessage {
			backend.recordFailure(fmt.Errorf("provider WebSocket held-idle successor message type = %d", messageType))
			return
		}
		backend.record(normalTransportGateReceipt{
			transport:  "websocket_frame",
			accountID:  accountID,
			candidate:  candidate,
			payload:    string(successor),
			connection: connectionIndex,
		})
		backend.writeWebSocketCompletion(connection, "normal-transport-idle-held-two")
		return
	}
	if backend.scenario == normalTransportGateWSSequentialAdmittedHardLimit && accountID == "validation-upstream-default" {
		backend.writeWebSocketCompletion(connection, "normal-transport-websocket")
		messageType, successor, readErr := connection.ReadMessage()
		if readErr != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket sequential successor read: %w", readErr))
			return
		}
		if messageType != websocket.TextMessage {
			backend.recordFailure(fmt.Errorf("provider WebSocket sequential successor message type = %d", messageType))
			return
		}
		backend.record(normalTransportGateReceipt{
			transport:  "websocket_frame",
			accountID:  accountID,
			candidate:  candidate,
			payload:    string(successor),
			connection: connectionIndex,
		})
		backend.writeWebSocketCompletion(connection, "normal-transport-websocket")
		return
	}
	responseID := "normal-transport-websocket"
	if backend.scenario == normalTransportGateWSIdleEOF {
		responseID = "normal-transport-idle-two"
	} else if backend.scenario == normalTransportGateWSIdleHeldOpen {
		responseID = "normal-transport-idle-held-two"
	} else if backend.scenario == normalTransportGateWSIdleCloseAfterPrecheck {
		responseID = "normal-transport-idle-race-two"
	}
	backend.completeWebSocketAuthChain(accountID)
	backend.writeWebSocketCompletion(connection, responseID)
}

func (backend *normalTransportGateBackend) serveWebSocketResponseAnchorSocketBound(connection *websocket.Conn, connectionIndex int, firstPayload []byte) {
	const firstResponseID = "normal-transport-socket-bound-one"
	const secondResponseID = "normal-transport-socket-bound-two"
	previousResponseID := func(payload []byte) string {
		var request struct {
			PreviousResponseID string `json:"previous_response_id"`
		}
		if err := json.Unmarshal(payload, &request); err != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket socket-bound request decode: %w", err))
			return ""
		}
		return request.PreviousResponseID
	}
	writeInvalidAnchor := func() {
		payload := []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"previous_response_id belongs to another WebSocket connection"}}`)
		if err := connection.WriteMessage(websocket.TextMessage, payload); err != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket socket-bound rejection write: %w", err))
		}
	}

	if connectionIndex != 1 {
		if previousResponseID(firstPayload) == firstResponseID {
			writeInvalidAnchor()
			return
		}
		backend.recordFailure(fmt.Errorf("provider WebSocket socket-bound successor missing anchor on connection %d", connectionIndex))
		writeInvalidAnchor()
		return
	}
	if previous := previousResponseID(firstPayload); previous != "" {
		backend.recordFailure(fmt.Errorf("provider WebSocket socket-bound first request previous_response_id = %q", previous))
		writeInvalidAnchor()
		return
	}
	backend.writeWebSocketCompletion(connection, firstResponseID)

	messageType, successorPayload, err := connection.ReadMessage()
	if err != nil {
		// Broken brokers close completed upstream sockets. Successor then reaches
		// another connection and receives deterministic invalid_request_error.
		return
	}
	if messageType != websocket.TextMessage {
		backend.recordFailure(fmt.Errorf("provider WebSocket socket-bound successor message type = %d", messageType))
		return
	}
	backend.record(normalTransportGateReceipt{
		transport:  "websocket_frame",
		payload:    string(successorPayload),
		connection: connectionIndex,
	})
	if previous := previousResponseID(successorPayload); previous != firstResponseID {
		backend.recordFailure(fmt.Errorf("provider WebSocket socket-bound successor previous_response_id = %q, want %q", previous, firstResponseID))
		writeInvalidAnchor()
		return
	}
	backend.writeWebSocketCompletion(connection, secondResponseID)
}

func (backend *normalTransportGateBackend) serveWebSocketResponseAnchorSocketClosed(connection *websocket.Conn, connectionIndex int, payload []byte) {
	const firstResponseID = "normal-transport-socket-closed-one"
	const secondResponseID = "normal-transport-socket-closed-two"
	var request struct {
		PreviousResponseID string `json:"previous_response_id"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		backend.recordFailure(fmt.Errorf("provider WebSocket socket-closed request decode: %w", err))
		return
	}
	if connectionIndex == 1 {
		if request.PreviousResponseID != "" {
			backend.recordFailure(fmt.Errorf("provider WebSocket socket-closed first request previous_response_id = %q", request.PreviousResponseID))
			return
		}
		backend.writeWebSocketCompletion(connection, firstResponseID)
		_ = connection.Close()
		backend.firstWSCloseOnce.Do(func() { close(backend.firstWSClosed) })
		return
	}
	if request.PreviousResponseID != "" {
		failure := []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"previous_response_id belongs to a closed WebSocket connection"}}`)
		if err := connection.WriteMessage(websocket.TextMessage, failure); err != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket socket-closed rejection write: %w", err))
		}
		return
	}
	backend.writeWebSocketCompletion(connection, secondResponseID)
}

func (backend *normalTransportGateBackend) rejectWebSocketHandshake(accountID string) bool {
	if backend.scenario == normalTransportGateWSAllHandshakeHardLimit || backend.scenario == normalTransportGateWSNonPortableAllHandshakeHardLimit || backend.scenario == normalTransportGateWSNonPortableTwoHandshakeHardLimit {
		return accountID == "validation-upstream-a" || accountID == "validation-upstream-b"
	}
	return (backend.scenario == normalTransportGateWSHandshakeHardLimit || backend.scenario == normalTransportGateWSNonPortableHandshakeHardLimit) && accountID == "validation-upstream-a"
}

func (backend *normalTransportGateBackend) webSocketHandshakeFailureStatus(accountID, candidate string) int {
	switch backend.scenario {
	case normalTransportGateWSDirectAuthRefreshSuccess,
		normalTransportGateWSDirectAuthRefreshFailure,
		normalTransportGateWSDirectAuthRefreshAllFailure:
		if accountID == "validation-upstream-a" && candidate == "validation-candidate-a-stale" {
			return http.StatusUnauthorized
		}
		return 0
	case normalTransportGateWSCredentialAuthThenSuccess:
		if candidate == "validation-candidate-a-one" {
			return http.StatusUnauthorized
		}
		return 0
	case normalTransportGateWSCredentialAuthThenHardLimit:
		if candidate == "validation-candidate-a-one" {
			return http.StatusUnauthorized
		}
		return http.StatusTooManyRequests
	case normalTransportGateWSCredentialAllUnauthorized:
		return http.StatusUnauthorized
	case normalTransportGateWSCredentialAllForbidden:
		return http.StatusForbidden
	}
	if backend.rejectWebSocketHandshake(accountID) {
		return http.StatusTooManyRequests
	}
	switch backend.scenario {
	case normalTransportGateWSAllHandshakeUnauthorized:
		if !backend.webSocketAuthChainCompleted() {
			return http.StatusUnauthorized
		}
		return 0
	case normalTransportGateWSAllHandshakeForbidden:
		if !backend.webSocketAuthChainCompleted() {
			return http.StatusForbidden
		}
		return 0
	case normalTransportGateWSSequentialHandshakeUnauthorized:
		if !backend.webSocketAuthChainCompleted() && (accountID == "validation-upstream-a" || accountID == "validation-upstream-b") {
			return http.StatusUnauthorized
		}
		return 0
	case normalTransportGateWSSequentialHandshakeForbidden:
		if !backend.webSocketAuthChainCompleted() && (accountID == "validation-upstream-a" || accountID == "validation-upstream-b") {
			return http.StatusForbidden
		}
		return 0
	}
	if accountID != "validation-upstream-a" {
		return 0
	}
	switch backend.scenario {
	case normalTransportGateWSHandshakeUnauthorized:
		return http.StatusUnauthorized
	case normalTransportGateWSHandshakeForbidden:
		return http.StatusForbidden
	default:
		return 0
	}
}

func (backend *normalTransportGateBackend) rejectWebSocketApplication(accountID string) bool {
	if backend.scenario == normalTransportGateWSAllApplicationHardLimit || backend.scenario == normalTransportGateWSNonPortableAllApplicationHardLimit || backend.scenario == normalTransportGateWSNonPortableTwoApplicationHardLimit {
		return accountID == "validation-upstream-a" || accountID == "validation-upstream-b"
	}
	return (backend.scenario == normalTransportGateWSApplicationHardLimit || backend.scenario == normalTransportGateWSNonPortableApplicationHardLimit) && accountID == "validation-upstream-a"
}

func (backend *normalTransportGateBackend) admittedWebSocketFailureStatus(accountID string) int {
	switch backend.scenario {
	case normalTransportGateWSAllAdmittedUnauthorized:
		if !backend.webSocketAuthChainCompleted() {
			return http.StatusUnauthorized
		}
		return 0
	case normalTransportGateWSAllAdmittedForbidden:
		if !backend.webSocketAuthChainCompleted() {
			return http.StatusForbidden
		}
		return 0
	}
	sequential := backend.scenario == normalTransportGateWSSequentialAdmittedHardLimit || backend.scenario == normalTransportGateWSSequentialAdmittedUnauthorized || backend.scenario == normalTransportGateWSSequentialAdmittedForbidden
	if accountID != "validation-upstream-a" && (!sequential || accountID != "validation-upstream-b") {
		return 0
	}
	switch backend.scenario {
	case normalTransportGateWSAdmittedHardLimit, normalTransportGateWSSequentialAdmittedHardLimit:
		return http.StatusTooManyRequests
	case normalTransportGateWSAdmittedUnauthorized, normalTransportGateWSSequentialAdmittedUnauthorized:
		if backend.webSocketAuthChainCompleted() {
			return 0
		}
		return http.StatusUnauthorized
	case normalTransportGateWSAdmittedForbidden, normalTransportGateWSSequentialAdmittedForbidden:
		if backend.webSocketAuthChainCompleted() {
			return 0
		}
		return http.StatusForbidden
	default:
		return 0
	}
}

func (backend *normalTransportGateBackend) webSocketAuthChainCompleted() bool {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.wsAuthChainDone
}

func (backend *normalTransportGateBackend) completeWebSocketAuthChain(accountID string) {
	if accountID != "validation-upstream-default" {
		return
	}
	switch backend.scenario {
	case normalTransportGateWSSequentialAdmittedUnauthorized,
		normalTransportGateWSSequentialAdmittedForbidden,
		normalTransportGateWSSequentialHandshakeUnauthorized,
		normalTransportGateWSSequentialHandshakeForbidden:
		backend.mu.Lock()
		backend.wsAuthChainDone = true
		backend.mu.Unlock()
	}
}

func (backend *normalTransportGateBackend) finishWebSocketAuthFailureCycle(accountID string) {
	if accountID != "validation-upstream-default" {
		return
	}
	switch backend.scenario {
	case normalTransportGateWSAllAdmittedUnauthorized,
		normalTransportGateWSAllAdmittedForbidden,
		normalTransportGateWSAllHandshakeUnauthorized,
		normalTransportGateWSAllHandshakeForbidden:
		backend.mu.Lock()
		backend.wsAuthChainDone = true
		backend.mu.Unlock()
	}
}

func (backend *normalTransportGateBackend) preAdmissionWebSocketFailureStatus(accountID string) int {
	if accountID != "validation-upstream-a" {
		return 0
	}
	switch backend.scenario {
	case normalTransportGateWSApplicationUnauthorized:
		return http.StatusUnauthorized
	case normalTransportGateWSApplicationForbidden:
		return http.StatusForbidden
	default:
		return 0
	}
}

func (backend *normalTransportGateBackend) writeWebSocketCompletion(connection *websocket.Conn, responseID string) {
	for _, payload := range [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"` + responseID + `"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"` + responseID + `","end_turn":true}}`),
	} {
		if err := connection.WriteMessage(websocket.TextMessage, payload); err != nil {
			backend.recordFailure(fmt.Errorf("provider WebSocket completion write: %w", err))
			return
		}
	}
}

func (backend *normalTransportGateBackend) fail(writer http.ResponseWriter, status int, err error) {
	backend.recordFailure(err)
	http.Error(writer, "provider request rejected", status)
}

func (backend *normalTransportGateBackend) record(receipt normalTransportGateReceipt) {
	backend.mu.Lock()
	backend.receipts = append(backend.receipts, receipt)
	backend.mu.Unlock()
}

func (backend *normalTransportGateBackend) snapshot() []normalTransportGateReceipt {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]normalTransportGateReceipt(nil), backend.receipts...)
}

func (backend *normalTransportGateBackend) allowHTTPAuthRecovery() {
	backend.mu.Lock()
	backend.httpAuthRecovered = true
	backend.httpAuthActive = false
	backend.mu.Unlock()
}

func (backend *normalTransportGateBackend) armHTTPAuthFailure(status int) {
	backend.mu.Lock()
	backend.httpAuthStatus = status
	backend.httpAuthActive = true
	backend.mu.Unlock()
}

func (backend *normalTransportGateBackend) recordFailure(err error) {
	select {
	case backend.failures <- err:
	default:
	}
}

func (backend *normalTransportGateBackend) assertNoFailure(t *testing.T) {
	t.Helper()
	select {
	case err := <-backend.failures:
		t.Fatal(err)
	default:
	}
}

func (backend *normalTransportGateBackend) triggerFirstWebSocketClose() {
	backend.closeFirstWSOnce.Do(func() { close(backend.closeFirstWS) })
}

type normalTransportGateCloseAfterPrecheckPlanner struct {
	inner   CodexNativeHTTPRequestPlanner
	backend *normalTransportGateBackend
}

func (planner normalTransportGateCloseAfterPrecheckPlanner) Build(ctx context.Context, input CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	if bytes.Contains(input.Encoded, []byte("normal-transport-turn-idle-race-two")) {
		planner.backend.triggerFirstWebSocketClose()
		select {
		case <-planner.backend.firstWSClosed:
		case <-ctx.Done():
			return CodexPreparedHTTPRequest{}, ctx.Err()
		}
	}
	return planner.inner.Build(ctx, input)
}

type normalTransportGateHarness struct {
	backend          *normalTransportGateBackend
	proxy            *httptest.Server
	continuity       *CodexContinuityCoordinator
	httpPlanner      *CodexHTTPRequestPlanFactory
	webSocketPlanner *CodexHTTPRequestPlanFactory
	inventory        *codexInstalledHTTPValidationInventory
	callerToken      string
	refresh          *normalTransportGateRefreshInventory
}

type normalTransportGateMultiCredentialInventory struct {
	inventory codex.Inventory
	material  map[codex.CandidateRef]codex.CredentialMaterial
}

func newNormalTransportGateMultiCredentialInventory(now time.Time) *normalTransportGateMultiCredentialInventory {
	identity := codex.AccountIdentity{
		AccountID: "validation-upstream-a",
		UserID:    "validation-user-a",
		PlanType:  "pro",
	}
	result := &normalTransportGateMultiCredentialInventory{
		material: make(map[codex.CandidateRef]codex.CredentialMaterial, 2),
	}
	account := codex.LogicalAccount{
		Key:      codexInstalledHTTPValidationAccountA,
		Identity: identity,
		Routable: true,
	}
	for index, fixture := range []struct {
		candidate codex.CandidateID
		token     string
	}{
		{candidate: "validation-candidate-a-one", token: "validation-token-a-one"},
		{candidate: "validation-candidate-a-two", token: "validation-token-a-two"},
	} {
		ref := codex.CandidateRef{AccountKey: account.Key, CandidateID: fixture.candidate}
		account.Candidates = append(account.Candidates, codex.CredentialCandidate{
			Ref:             ref,
			Revision:        codex.Revision(fmt.Sprintf("validation-revision-%d", index+1)),
			Source:          codex.SourceExternal,
			AccessExpiresAt: now.Add(24 * time.Hour),
			Routable:        true,
		})
		result.material[ref] = codex.CredentialMaterial{
			AccessToken: fixture.token,
			IDToken:     codexInstalledHTTPValidationIDToken(identity.AccountID, identity.UserID),
			AccountID:   identity.AccountID,
		}
	}
	result.inventory.Accounts = []codex.LogicalAccount{account}
	return result
}

func (inventory *normalTransportGateMultiCredentialInventory) List(ctx context.Context) (codex.Inventory, error) {
	if ctx == nil {
		return codex.Inventory{}, errors.New("multi-credential inventory context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return codex.Inventory{}, err
	}
	result := codex.Inventory{Accounts: make([]codex.LogicalAccount, len(inventory.inventory.Accounts))}
	for index, account := range inventory.inventory.Accounts {
		result.Accounts[index] = account
		result.Accounts[index].Candidates = append([]codex.CredentialCandidate(nil), account.Candidates...)
	}
	return result, nil
}

func (inventory *normalTransportGateMultiCredentialInventory) ResolveExact(ctx context.Context, planned codex.PlannedCandidate) (codex.CredentialMaterial, error) {
	view, err := inventory.List(ctx)
	if err != nil {
		return codex.CredentialMaterial{}, err
	}
	for _, account := range view.Accounts {
		if account.Key != planned.Ref.AccountKey || account.Identity.AccountID != planned.Identity.AccountID || account.Identity.UserID != planned.Identity.UserID {
			continue
		}
		for _, candidate := range account.Candidates {
			if candidate.Ref != planned.Ref || candidate.Revision != planned.Revision || candidate.Source != planned.Source || !candidate.Routable || candidate.DispatchBlocked {
				continue
			}
			if material, ok := inventory.material[candidate.Ref]; ok {
				return material, nil
			}
		}
	}
	return codex.CredentialMaterial{}, codex.ErrStaleRevision
}

func (inventory *normalTransportGateMultiCredentialInventory) RefreshReference(ctx context.Context, _ codex.CandidateRef, _ codex.Revision) (codex.CandidateRef, codex.Revision, error) {
	if ctx == nil {
		return codex.CandidateRef{}, "", errors.New("multi-credential refresh context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return codex.CandidateRef{}, "", err
	}
	return codex.CandidateRef{}, "", codex.ErrRefreshUnavailable
}

func (inventory *normalTransportGateMultiCredentialInventory) revalidate(ctx context.Context, key codex.AccountKey) error {
	view, err := inventory.List(ctx)
	if err != nil {
		return err
	}
	for _, account := range view.Accounts {
		if account.Key == key && account.Routable && !account.Unstable {
			return nil
		}
	}
	return codex.ErrStaleRevision
}

type normalTransportGateRefreshInventory struct {
	mu          sync.Mutex
	inventory   codex.Inventory
	material    map[codex.AccountKey]codex.CredentialMaterial
	failRefresh bool
	refreshes   int
}

func newNormalTransportGateRefreshInventory(t *testing.T, source *codexInstalledHTTPValidationInventory, failRefresh, direct bool) *normalTransportGateRefreshInventory {
	t.Helper()
	inventory, err := source.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := &normalTransportGateRefreshInventory{
		inventory:   inventory,
		material:    make(map[codex.AccountKey]codex.CredentialMaterial, len(source.material)),
		failRefresh: failRefresh,
	}
	for key, material := range source.material {
		result.material[key] = material
	}
	found := false
	for accountIndex := range result.inventory.Accounts {
		account := &result.inventory.Accounts[accountIndex]
		if account.Key != codexInstalledHTTPValidationAccountA || len(account.Candidates) != 1 {
			continue
		}
		candidate := &account.Candidates[0]
		candidate.Source = codex.SourceManaged
		candidate.CQAuthored = true
		candidate.RefreshEligible = true
		candidate.AccessExpiresAt = time.Now().Add(-time.Hour)
		if direct {
			candidate.AccessExpiresAt = time.Now().Add(time.Hour)
		}
		candidate.Revision = "validation-revision-expired"
		material := result.material[account.Key]
		material.AccessToken = "validation-token-a-stale"
		result.material[account.Key] = material
		found = true
	}
	if !found {
		t.Fatal("refresh-only account fixture unavailable")
	}
	return result
}

func (inventory *normalTransportGateRefreshInventory) List(ctx context.Context) (codex.Inventory, error) {
	if ctx == nil {
		return codex.Inventory{}, errors.New("refresh inventory context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return codex.Inventory{}, err
	}
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	result := codex.Inventory{Accounts: make([]codex.LogicalAccount, len(inventory.inventory.Accounts))}
	for index, account := range inventory.inventory.Accounts {
		result.Accounts[index] = account
		result.Accounts[index].Candidates = append([]codex.CredentialCandidate(nil), account.Candidates...)
	}
	return result, nil
}

func (inventory *normalTransportGateRefreshInventory) ResolveExact(ctx context.Context, planned codex.PlannedCandidate) (codex.CredentialMaterial, error) {
	if ctx == nil {
		return codex.CredentialMaterial{}, errors.New("refresh inventory context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return codex.CredentialMaterial{}, err
	}
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	for _, account := range inventory.inventory.Accounts {
		if account.Key != planned.Ref.AccountKey || account.Identity.AccountID != planned.Identity.AccountID || account.Identity.UserID != planned.Identity.UserID {
			continue
		}
		for _, candidate := range account.Candidates {
			if candidate.Ref == planned.Ref && candidate.Revision == planned.Revision && candidate.Source == planned.Source && candidate.Routable && !candidate.DispatchBlocked {
				if material, ok := inventory.material[account.Key]; ok {
					return material, nil
				}
			}
		}
	}
	return codex.CredentialMaterial{}, codex.ErrStaleRevision
}

func (inventory *normalTransportGateRefreshInventory) RefreshReference(ctx context.Context, ref codex.CandidateRef, revision codex.Revision) (codex.CandidateRef, codex.Revision, error) {
	if ctx == nil {
		return codex.CandidateRef{}, "", errors.New("refresh inventory context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return codex.CandidateRef{}, "", err
	}
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	inventory.refreshes++
	if inventory.failRefresh {
		return codex.CandidateRef{}, "", codex.ErrRefreshUnavailable
	}
	for accountIndex := range inventory.inventory.Accounts {
		account := &inventory.inventory.Accounts[accountIndex]
		for candidateIndex := range account.Candidates {
			candidate := &account.Candidates[candidateIndex]
			if candidate.Ref != ref || candidate.Revision != revision {
				continue
			}
			candidate.Revision = "validation-revision-refreshed"
			candidate.AccessExpiresAt = time.Now().Add(time.Hour)
			material := inventory.material[account.Key]
			material.AccessToken = "validation-token-a-refreshed"
			inventory.material[account.Key] = material
			return candidate.Ref, candidate.Revision, nil
		}
	}
	return codex.CandidateRef{}, "", codex.ErrStaleRevision
}

func (inventory *normalTransportGateRefreshInventory) revalidate(ctx context.Context, key codex.AccountKey) error {
	view, err := inventory.List(ctx)
	if err != nil {
		return err
	}
	for _, account := range view.Accounts {
		if account.Key == key && account.Routable && !account.Unstable {
			return nil
		}
	}
	return codex.ErrStaleRevision
}

func (inventory *normalTransportGateRefreshInventory) refreshCount() int {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	return inventory.refreshes
}

func normalTransportGateJWT(t *testing.T, header string, claims any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func normalTransportGateAccessExpiryFixture(t *testing.T) (codex.Inventory, codex.CredentialMaterial, NormalCallerCredentialV1) {
	t.Helper()
	now := time.Now().UTC()
	const accountID = "validation-upstream-a"
	const userID = "validation-user-a"
	idToken := normalTransportGateJWT(t, "e30", map[string]any{
		"email": "validation@example.test",
		"exp":   now.Add(-time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    userID,
			"chatgpt_plan_type":  "pro",
		},
	})
	accessExpiresAt := time.Unix(now.Add(24*time.Hour).Unix(), 0)
	accessToken := normalTransportGateJWT(t, "validation-token-a", map[string]any{
		"exp": accessExpiresAt.Unix(),
	})
	authData, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": "validation-refresh-token",
			"id_token":      idToken,
			"account_id":    accountID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	managedHome := filepath.Join(root, "managed-codex-homes", "validation-record")
	if err := os.MkdirAll(managedHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedHome, "auth.json"), authData, 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(authData)
	manifest, err := json.Marshal(map[string]any{
		"version": 3,
		"accounts": []any{map[string]any{
			"id":                 "validation-record",
			"managedHomePath":    managedHome,
			"providerAccountID":  accountID,
			"workspaceAccountID": accountID,
			"authFingerprint":    hex.EncodeToString(fingerprint[:]),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "managed-codex-accounts.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	inventory := codex.DiscoverInventoryWithSources(context.Background(), fsutil.NewMemFS(), codex.NewCodexBarSource(root))
	if len(inventory.Accounts) != 1 || len(inventory.Accounts[0].Candidates) != 1 {
		t.Fatalf("parsed CodexBar inventory = %#v, want one account and candidate", inventory)
	}
	// Installed CodexBar accounts have already been associated with a stable
	// logical account by the account catalogue. Preserve that independent
	// routing fact while keeping candidate metadata sourced from the parser.
	inventory.Accounts[0].Unstable = false
	account := inventory.Accounts[0]
	candidate := account.Candidates[0]
	material := codex.CredentialMaterial{
		AccessToken:  accessToken,
		RefreshToken: "validation-refresh-token",
		IDToken:      idToken,
		AccountID:    accountID,
	}
	caller := NormalCallerCredentialV1{
		Domain:     NormalCallerCodex,
		Bearer:     accessToken,
		SubjectID:  string(account.Key) + "\x00" + string(candidate.Ref.CandidateID) + "\x00" + string(candidate.Revision),
		ValidUntil: accessExpiresAt,
	}
	return inventory, material, caller
}

func newNormalTransportGateHarness(t *testing.T, scenario normalTransportGateScenario) *normalTransportGateHarness {
	return newNormalTransportGateHarnessWithCaller(t, scenario, false)
}

func newNormalTransportGateCodexCallerHarness(t *testing.T, scenario normalTransportGateScenario) *normalTransportGateHarness {
	return newNormalTransportGateHarnessWithCaller(t, scenario, true)
}

func newNormalTransportGateHarnessWithCaller(t *testing.T, scenario normalTransportGateScenario, authenticatedCodexCaller bool) *normalTransportGateHarness {
	t.Helper()
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Errorf("close normal transport gate core: %v", err)
		}
	})
	var codexCaller *NormalCallerCredentialV1
	if scenario == normalTransportGateAccessTokenOutlivesIDToken {
		inventory, material, caller := normalTransportGateAccessExpiryFixture(t)
		core.inventory.inventory = inventory
		core.inventory.material = map[codex.AccountKey]codex.CredentialMaterial{
			inventory.Accounts[0].Key: material,
		}
		codexCaller = &caller
	} else if authenticatedCodexCaller {
		account := core.inventory.inventory.Accounts[0]
		candidate := account.Candidates[0]
		material := core.inventory.material[account.Key]
		codexCaller = &NormalCallerCredentialV1{
			Domain:     NormalCallerCodex,
			Bearer:     material.AccessToken,
			SubjectID:  string(account.Key) + "\x00" + string(candidate.Ref.CandidateID) + "\x00" + string(candidate.Revision),
			ValidUntil: candidate.AccessExpiresAt,
		}
	}
	if normalTransportGateAllAccountsLimited(scenario) {
		accounts := core.inventory.inventory.Accounts[:0]
		for _, account := range core.inventory.inventory.Accounts {
			if account.Key != codexInstalledHTTPValidationDefault {
				accounts = append(accounts, account)
			}
		}
		core.inventory.inventory.Accounts = accounts
		delete(core.inventory.material, codexInstalledHTTPValidationDefault)
	}
	if scenario == normalTransportGateWSDirectAuthRefreshAllFailure {
		for key := range core.inventory.material {
			if key != codexInstalledHTTPValidationAccountA {
				delete(core.inventory.material, key)
			}
		}
		for _, account := range core.inventory.inventory.Accounts {
			if account.Key == codexInstalledHTTPValidationAccountA {
				core.inventory.inventory.Accounts = []codex.LogicalAccount{account}
				break
			}
		}
	}
	defaultAccountKey := codexInstalledHTTPValidationDefault
	if normalTransportGateAllAccountsLimited(scenario) {
		defaultAccountKey = codexInstalledHTTPValidationAccountB
	}
	if scenario == normalTransportGateWSDirectAuthRefreshAllFailure {
		defaultAccountKey = codexInstalledHTTPValidationAccountA
	}
	if scenario == normalTransportGateAccessTokenOutlivesIDToken {
		defaultAccountKey = ""
	}
	var credentialInventory codex.CredentialInventory = core.inventory
	var exactSecrets codex.ExactSecretResolver = core.inventory
	var credentialRefresher codex.CredentialReferenceRefresher = core.inventory
	leaseRuntime := core.leaseRuntime
	var refreshInventory *normalTransportGateRefreshInventory
	if scenario == normalTransportGateWSRefreshSuccess || scenario == normalTransportGateWSRefreshFailure || normalTransportGateDirectRefreshScenario(scenario) {
		failRefresh := scenario == normalTransportGateWSRefreshFailure || scenario == normalTransportGateWSDirectAuthRefreshFailure || scenario == normalTransportGateWSDirectAuthRefreshAllFailure
		refreshInventory = newNormalTransportGateRefreshInventory(t, core.inventory, failRefresh, normalTransportGateDirectRefreshScenario(scenario))
		credentialInventory = refreshInventory
		exactSecrets = refreshInventory
		credentialRefresher = refreshInventory
		leaseRuntime, err = newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(core.continuity, refreshInventory.revalidate, core.admissions)
		if err != nil {
			t.Fatal(err)
		}
	}
	if normalTransportGateMultiCredentialScenario(scenario) {
		multiCredentialInventory := newNormalTransportGateMultiCredentialInventory(time.Now())
		credentialInventory = multiCredentialInventory
		exactSecrets = multiCredentialInventory
		credentialRefresher = multiCredentialInventory
		defaultAccountKey = codexInstalledHTTPValidationAccountA
		leaseRuntime, err = newCodexLeaseRuntimeWithNativeHTTPAdmissionSink(core.continuity, multiCredentialInventory.revalidate, core.admissions)
		if err != nil {
			t.Fatal(err)
		}
	}

	backend := newNormalTransportGateBackend(scenario)
	provider := httptest.NewServer(backend)
	t.Cleanup(func() {
		backend.triggerFirstWebSocketClose()
		provider.CloseClientConnections()
		provider.Close()
	})
	providerURL, err := url.Parse(provider.URL)
	if err != nil || providerURL.Host == "" {
		t.Fatal("normal transport gate provider URL unavailable")
	}
	httpTransport := newCodexInstalledHTTPValidationRoundTripper(providerURL.Host)
	t.Cleanup(httpTransport.transport.CloseIdleConnections)

	httpPlanner := &CodexHTTPRequestPlanFactory{
		Inventory: credentialInventory, Capacity: core.capacity, Routes: core.continuity, Runtime: leaseRuntime,
		DefaultAccountKey: defaultAccountKey,
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		TransportKind:     "http",
		Now:               time.Now,
	}
	httpHandler, err := NewCodexNativeHTTPHandler(httpPlanner, &CodexHTTPRequestSession{
		Executor: &CodexAttemptExecutor{
			Inventory: credentialInventory,
			Secrets:   exactSecrets,
			Transport: &CodexTokenTransport{Inner: httpTransport},
		},
		Refresher: credentialRefresher,
		Capacity:  core.capacity,
	}, provider.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := httpHandler.CloseAndDrain(ctx); err != nil {
			t.Errorf("close normal transport HTTP handler: %v", err)
		}
	})

	webSocketPlanner := &CodexHTTPRequestPlanFactory{
		Inventory: credentialInventory, Capacity: core.capacity, Routes: core.continuity, Runtime: leaseRuntime,
		DefaultAccountKey: defaultAccountKey,
		Authority:         CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
		TransportKind:     "websocket",
		Now:               time.Now,
	}
	var webSocketPlans CodexNativeHTTPRequestPlanner = webSocketPlanner
	if scenario == normalTransportGateWSIdleCloseAfterPrecheck {
		webSocketPlans = normalTransportGateCloseAfterPrecheckPlanner{inner: webSocketPlanner, backend: backend}
	}
	webSocketExecutor := NewCodexWebSocketAttemptExecutor(credentialInventory, exactSecrets)
	webSocketExecutor.Dialer.Proxy = nil
	webSocketBroker, err := NewCodexTerminatingWebSocketHandler(webSocketPlans, webSocketExecutor, credentialRefresher, core.capacity, provider.URL)
	if err != nil {
		t.Fatal(err)
	}
	if scenario == normalTransportGateWSPrewarmStall {
		handler, ok := webSocketBroker.(*codexTerminatingWebSocketHandler)
		if !ok {
			t.Fatal("normal transport gate WebSocket handler unavailable")
		}
		handler.prewarmTimeout = 20 * time.Millisecond
	}
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Config: &Config{LocalToken: localToken, ClaudeUpstream: provider.URL, CodexUpstream: provider.URL},
		CodexRouting: &CodexRoutingRuntime{
			HTTP:      CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 1, AuthoritativeEpoch: 1},
			WebSocket: CodexModeStatus{Configured: CodexRoutingEnforce, Effective: CodexRoutingEnforce, ModeEpoch: 1, AuthoritativeEpoch: 1},
		},
		CodexNativeHTTP:      httpHandler,
		CodexWebSocketBroker: webSocketBroker,
		Catalog:              modelregistry.NewCatalog(modelregistry.Snapshot{}),
	}
	handler, err := server.RuntimeHandler()
	if err != nil {
		t.Fatal(err)
	}
	callerToken := localToken
	var proxyServer *httptest.Server
	if codexCaller != nil {
		proxyServer, _ = newCodexRuntimeSupervisorAcceptanceServerWithCredentials(t, handler, []NormalCallerCredentialV1{
			{Domain: NormalCallerLocal, Bearer: localToken, SubjectID: "validation-local"},
			*codexCaller,
		})
		callerToken = codexCaller.Bearer
	} else {
		proxyServer, _ = newCodexRuntimeSupervisorAcceptanceServer(t, handler, localToken)
	}
	return &normalTransportGateHarness{
		backend:          backend,
		proxy:            proxyServer,
		continuity:       core.continuity,
		httpPlanner:      httpPlanner,
		webSocketPlanner: webSocketPlanner,
		inventory:        core.inventory,
		callerToken:      callerToken,
		refresh:          refreshInventory,
	}
}

func normalTransportGateAllAccountsLimited(scenario normalTransportGateScenario) bool {
	return scenario == normalTransportGateHTTPAllHardLimit || scenario == normalTransportGateWSAllApplicationHardLimit || scenario == normalTransportGateWSAllHandshakeHardLimit ||
		scenario == normalTransportGateWSNonPortableAllApplicationHardLimit || scenario == normalTransportGateWSNonPortableAllHandshakeHardLimit
}

func normalTransportGateMultiCredentialScenario(scenario normalTransportGateScenario) bool {
	return scenario == normalTransportGateWSCredentialAuthThenSuccess ||
		scenario == normalTransportGateWSCredentialAuthThenHardLimit ||
		scenario == normalTransportGateWSCredentialAllUnauthorized ||
		scenario == normalTransportGateWSCredentialAllForbidden
}

func normalTransportGateDirectRefreshScenario(scenario normalTransportGateScenario) bool {
	return scenario == normalTransportGateWSDirectAuthRefreshSuccess ||
		scenario == normalTransportGateWSDirectAuthRefreshFailure ||
		scenario == normalTransportGateWSDirectAuthRefreshAllFailure
}

func normalTransportGateNonPortableWebSocketScenario(scenario normalTransportGateScenario) bool {
	return scenario == normalTransportGateWSNonPortableApplicationHardLimit || scenario == normalTransportGateWSNonPortableHandshakeHardLimit ||
		scenario == normalTransportGateWSNonPortableAllApplicationHardLimit || scenario == normalTransportGateWSNonPortableAllHandshakeHardLimit ||
		scenario == normalTransportGateWSNonPortableTwoApplicationHardLimit || scenario == normalTransportGateWSNonPortableTwoHandshakeHardLimit
}

func TestNormalProxyTransportRoutesWhenAccessTokenOutlivesIDToken(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, normalTransportGateAccessTokenOutlivesIDToken)
			metadata := CodexTurnMetadata{
				SessionID:   "normal-transport-session-access-expiry-" + transport,
				ThreadID:    "normal-transport-thread-access-expiry-" + transport,
				TurnID:      "normal-transport-turn-access-expiry-" + transport,
				RequestKind: CodexRequestTurn,
			}

			switch transport {
			case "http":
				status, responseBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata))
				if status != http.StatusOK || !bytes.Contains(responseBody, []byte(`"type":"response.completed"`)) {
					t.Fatalf("normal HTTP response = %d %q, want completed 200", status, responseBody)
				}
				receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
				if len(receipts) != 1 || receipts[0].accountID != "validation-upstream-a" || receipts[0].status != http.StatusOK {
					t.Fatalf("provider HTTP receipts = %#v, want one completed account-a request", receipts)
				}
			case "websocket":
				connection := normalTransportGateWebSocket(t, harness)
				if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame(metadata.TurnID, "")); err != nil {
					t.Fatal(err)
				}
				normalTransportGateReadWSCompletion(t, connection)
				if err := connection.Close(); err != nil {
					t.Fatal(err)
				}
				handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
				frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
				if len(handshakes) != 1 || handshakes[0].accountID != "validation-upstream-a" || handshakes[0].status != http.StatusSwitchingProtocols || len(frames) != 1 {
					t.Fatalf("provider WebSocket receipts = handshakes %#v frames %#v, want one completed account-a turn", handshakes, frames)
				}
			}

			authorizations := harness.backend.authorizationSnapshot()
			if len(authorizations) != 1 || authorizations[0] != "Bearer "+harness.callerToken {
				t.Fatalf("provider authorizations = %#v, want exact parsed access bearer", authorizations)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportHTTPSuccessorRoutesAfterAbandonedPredecessor(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateHTTPSuccess)
	predecessor := CodexTurnMetadata{
		SessionID:   "normal-transport-session-abandoned",
		ThreadID:    "normal-transport-thread-abandoned",
		TurnID:      "normal-transport-turn-abandoned-predecessor",
		RequestKind: CodexRequestTurn,
	}
	prepared, err := harness.httpPlanner.Build(context.Background(), CodexHTTPRequestPlanInput{
		Encoded: normalTransportGateHTTPBody(t, predecessor),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background()); err != nil {
		prepared.Frozen.Release()
		t.Fatal(err)
	}
	prepared.Frozen.Release()

	successor := predecessor
	successor.TurnID = "normal-transport-turn-abandoned-successor"
	status, responseBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, successor))
	if status != http.StatusOK || !bytes.Contains(responseBody, []byte(`"type":"response.completed"`)) {
		t.Fatalf("successor HTTP response = %d %q, want completed 200", status, responseBody)
	}
	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 1 || receipts[0].status != http.StatusOK {
		t.Fatalf("successor provider receipts = %#v, want one completed request", receipts)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportHTTPRejectedTurnCanRetry(t *testing.T) {
	for _, test := range []struct {
		name       string
		scenario   normalTransportGateScenario
		authStatus int
	}{
		{name: "unauthorized", scenario: normalTransportGateHTTPAuthRetry, authStatus: http.StatusUnauthorized},
		{name: "forbidden", scenario: normalTransportGateHTTPForbiddenRetry, authStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			metadata := CodexTurnMetadata{
				SessionID: "normal-transport-session-auth", ThreadID: "normal-transport-thread-auth", TurnID: "normal-transport-turn-auth", RequestKind: CodexRequestTurn,
			}
			body := normalTransportGateHTTPBody(t, metadata)

			firstStatus, _ := normalTransportGateHTTPCall(t, harness, body)
			if firstStatus != test.authStatus {
				t.Fatalf("first status = %d, want %d", firstStatus, test.authStatus)
			}
			harness.backend.allowHTTPAuthRecovery()
			secondStatus, secondBody := normalTransportGateHTTPCall(t, harness, body)
			if secondStatus != http.StatusOK || !bytes.Contains(secondBody, []byte(`"type":"response.completed"`)) {
				t.Fatalf("same-turn retry = %d %q, want completed 200", secondStatus, secondBody)
			}

			receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
			if len(receipts) < 2 || receipts[len(receipts)-1].status != http.StatusOK || receipts[len(receipts)-1].payload != receipts[0].payload {
				t.Fatalf("provider receipts = %#v, want same request to reach provider after rejected round", receipts)
			}
			for _, receipt := range receipts[:len(receipts)-1] {
				if receipt.status != test.authStatus {
					t.Fatalf("pre-recovery provider status = %d, want %d", receipt.status, test.authStatus)
				}
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportHTTPHardLimitMigratesBeforeLeak(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateHTTPHardLimit)
	metadata := CodexTurnMetadata{
		SessionID: "normal-transport-session-http-429", ThreadID: "normal-transport-thread-http-429", TurnID: "normal-transport-turn-http-429", RequestKind: CodexRequestTurn,
	}
	status, responseBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata))
	if status != http.StatusOK || bytes.Contains(responseBody, []byte("usage_limit_reached")) || !bytes.Contains(responseBody, []byte(`"type":"response.completed"`)) {
		t.Fatalf("hard-limit downstream response = %d %q, want completed 200 without 429", status, responseBody)
	}
	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 2 ||
		receipts[0].accountID != "validation-upstream-a" || receipts[0].status != http.StatusTooManyRequests ||
		receipts[1].accountID != "validation-upstream-b" || receipts[1].status != http.StatusOK ||
		receipts[0].payload != receipts[1].payload {
		t.Fatalf("hard-limit provider receipts = %#v, want A/429 then byte-identical B/200", receipts)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportRequiredAffinityHardLimitReplaysPortableRequest(t *testing.T) {
	for _, test := range []struct {
		name      string
		transport string
		scenario  normalTransportGateScenario
	}{
		{name: "http", transport: "http", scenario: normalTransportGateHTTPHardLimit},
		{name: "websocket", transport: "websocket", scenario: normalTransportGateWSApplicationHardLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			snapshotter := &codexHTTPRequestPlanTestSnapshotter{snapshot: CodexLeaseRouteSnapshot{
				Classification:          CodexRestoredLaneUnseen,
				AffinityPresent:         true,
				AffinityAccountKey:      codexInstalledHTTPValidationAccountA,
				AffinityCacheAdmittedAt: time.Now(),
				AffinityEffectiveModel:  "gpt-5.6-sol",
				AffinityRequiresAccount: true,
				JournalGeneration:       harness.continuity.Store().Generation(),
			}}
			harness.httpPlanner.Routes = snapshotter
			harness.webSocketPlanner.Routes = snapshotter

			switch test.transport {
			case "http":
				metadata := CodexTurnMetadata{
					SessionID: "normal-transport-session-required-affinity-http", ThreadID: "normal-transport-thread-required-affinity-http", TurnID: "normal-transport-turn-required-affinity-http", RequestKind: CodexRequestTurn,
				}
				status, body := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata))
				if status != http.StatusOK || bytes.Contains(body, []byte("usage_limit_reached")) || !bytes.Contains(body, []byte(`"type":"response.completed"`)) {
					t.Fatalf("required-affinity hard-limit response = %d %q, want replayed 200", status, body)
				}
				receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
				if len(receipts) != 2 || receipts[0].accountID != "validation-upstream-a" || receipts[0].status != http.StatusTooManyRequests || receipts[1].accountID != "validation-upstream-b" || receipts[1].status != http.StatusOK || receipts[0].payload != receipts[1].payload {
					t.Fatalf("required-affinity HTTP receipts = %#v, want byte-identical A/429 then B/200", receipts)
				}
			case "websocket":
				frame := normalTransportGateWSFrameForLane("normal-transport-session-required-affinity-ws", "normal-transport-thread-required-affinity-ws", "normal-transport-turn-required-affinity-ws", "")
				connection := normalTransportGateWebSocket(t, harness)
				if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
					t.Fatal(err)
				}
				replies := normalTransportGateReadWSCompletion(t, connection)
				_ = connection.Close()
				if bytes.Contains(bytes.Join(replies, nil), []byte("usage_limit_reached")) {
					t.Fatalf("required-affinity hard-limit leaked downstream: %q", replies)
				}
				receipts := harness.backend.snapshot()
				handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
				frames := normalTransportGateReceipts(receipts, "websocket_frame")
				if len(handshakes) != 2 || handshakes[0].accountID != "validation-upstream-a" || handshakes[1].accountID != "validation-upstream-b" || len(frames) != 2 || frames[0].accountID != "validation-upstream-a" || frames[1].accountID != "validation-upstream-b" || frames[0].payload != frames[1].payload {
					t.Fatalf("required-affinity WebSocket receipts = handshakes %#v frames %#v, want byte-identical A then B", handshakes, frames)
				}
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportHTTPPortableSuccessorReplaysAfterEncryptedPredecessorHardLimit(t *testing.T) {
	harness := newNormalTransportGateCodexCallerHarness(t, normalTransportGateWSNonPortableApplicationHardLimit)
	predecessor := CodexTurnMetadata{
		SessionID: "normal-transport-session-http-encrypted-429", ThreadID: "normal-transport-thread-http-encrypted-429", TurnID: "normal-transport-turn-http-encrypted-predecessor", RequestKind: CodexRequestTurn,
	}
	status, body := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, predecessor))
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"encrypted_content":"opaque-normal-transport-state"`)) {
		t.Fatalf("encrypted predecessor = %d %q, want A/200", status, body)
	}

	harness.backend.scenario = normalTransportGateHTTPHardLimit
	successor := predecessor
	successor.TurnID = "normal-transport-turn-http-encrypted-successor"
	var request map[string]any
	if err := json.Unmarshal(normalTransportGateHTTPBody(t, successor), &request); err != nil {
		t.Fatal(err)
	}
	request["input"] = []any{map[string]any{"encrypted_content": "opaque-normal-transport-state"}}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	status, body = normalTransportGateHTTPCall(t, harness, encoded)
	if status != http.StatusOK || bytes.Contains(body, []byte("usage_limit_reached")) || !bytes.Contains(body, []byte(`"type":"response.completed"`)) {
		t.Fatalf("encrypted successor = %d %q, want replayed B/200", status, body)
	}

	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 3 ||
		receipts[0].accountID != "validation-upstream-a" || receipts[0].status != http.StatusOK ||
		receipts[1].accountID != "validation-upstream-a" || receipts[1].status != http.StatusTooManyRequests ||
		receipts[2].accountID != "validation-upstream-b" || receipts[2].status != http.StatusOK ||
		receipts[1].payload != receipts[2].payload {
		t.Fatalf("encrypted successor receipts = %#v, want A/200 then byte-identical A/429 and B/200", receipts)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketPortableSuccessorMigratesAfterExhaustedEncryptedPredecessor(t *testing.T) {
	harness := newNormalTransportGateCodexCallerHarness(t, normalTransportGateWSNonPortableApplicationHardLimit)
	allAccounts := append([]codex.LogicalAccount(nil), harness.inventory.inventory.Accounts...)
	if len(allAccounts) < 2 || allAccounts[0].Key != codexInstalledHTTPValidationAccountA || allAccounts[1].Key != codexInstalledHTTPValidationAccountB {
		t.Fatalf("validation inventory = %#v, want A then B", allAccounts)
	}
	harness.inventory.inventory.Accounts = append([]codex.LogicalAccount(nil), allAccounts[:1]...)

	predecessor := CodexTurnMetadata{
		SessionID: "normal-transport-session-ws-successor", ThreadID: "normal-transport-thread-ws-successor", TurnID: "normal-transport-turn-ws-predecessor", RequestKind: CodexRequestTurn,
	}
	seedStatus, seedBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, predecessor))
	if seedStatus != http.StatusOK || !bytes.Contains(seedBody, []byte(`"encrypted_content":"opaque-normal-transport-state"`)) {
		t.Fatalf("predecessor seed = %d %q, want encrypted A/200", seedStatus, seedBody)
	}
	var encryptedContinuation map[string]any
	if err := json.Unmarshal(normalTransportGateHTTPContinuationBody(t, predecessor, "normal-transport-http"), &encryptedContinuation); err != nil {
		t.Fatal(err)
	}
	encryptedContinuation["input"] = []any{map[string]any{"encrypted_content": "opaque-normal-transport-state"}}
	encryptedBody, err := json.Marshal(encryptedContinuation)
	if err != nil {
		t.Fatal(err)
	}
	continuationStatus, continuationBody := normalTransportGateHTTPCall(t, harness, encryptedBody)
	if continuationStatus != http.StatusOK || !bytes.Contains(continuationBody, []byte(`"type":"response.completed"`)) {
		t.Fatalf("encrypted predecessor continuation = %d %q, want A/200", continuationStatus, continuationBody)
	}
	predecessorSnapshot, err := harness.continuity.LoadRouteSnapshot(context.Background(), NewCodexLeaseKey(predecessor), []codex.AccountKey{codexInstalledHTTPValidationAccountA}, CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true})
	if err != nil {
		t.Fatal(err)
	}
	if predecessorSnapshot.AffinityRequiresAccount || predecessorSnapshot.BoundRequiresAccount {
		t.Fatalf("encrypted predecessor snapshot = %#v, want portable account affinity", predecessorSnapshot)
	}

	exhausted := normalTransportGateWebSocket(t, harness)
	if err := exhausted.WriteMessage(websocket.TextMessage, normalTransportGateWSFrameForLane(predecessor.SessionID, predecessor.ThreadID, predecessor.TurnID, "")); err != nil {
		t.Fatal(err)
	}
	if err := exhausted.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := exhausted.ReadMessage()
	if err != nil || messageType != websocket.TextMessage {
		t.Fatalf("exhausted predecessor = type %d payload %q err %v, want final 429", messageType, payload, err)
	}
	normalTransportGateRequireSemanticJSON(t, payload, []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`), "exhausted predecessor")
	_ = exhausted.Close()
	harness.inventory.inventory.Accounts = append([]codex.LogicalAccount(nil), allAccounts...)

	successor := normalTransportGateWebSocket(t, harness)
	successorFrame := bytes.Replace(
		normalTransportGateWSFrameForLane(predecessor.SessionID, predecessor.ThreadID, "normal-transport-turn-ws-successor", ""),
		[]byte(`"input":[]`),
		[]byte(`"input":[{"encrypted_content":"opaque-normal-transport-state"}]`),
		1,
	)
	if err := successor.WriteMessage(websocket.TextMessage, successorFrame); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSCompletion(t, successor)
	_ = successor.Close()

	receipts := harness.backend.snapshot()
	handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
	frames := normalTransportGateReceipts(receipts, "websocket_frame")
	if len(handshakes) != 2 || len(frames) != 2 || handshakes[0].accountID != "validation-upstream-a" || handshakes[1].accountID != "validation-upstream-b" || frames[0].accountID != "validation-upstream-a" || frames[1].accountID != "validation-upstream-b" {
		t.Fatalf("successor migration receipts = handshakes %#v frames %#v, want A then B", handshakes, frames)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportLeaseInvalidationReselectsCurrentPortableTurn(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			harness := newNormalTransportGateCodexCallerHarness(t, normalTransportGateWSNonPortableApplicationHardLimit)
			predecessor := CodexTurnMetadata{
				SessionID:   "normal-transport-session-invalidate-" + transport,
				ThreadID:    "normal-transport-thread-invalidate-" + transport,
				TurnID:      "normal-transport-turn-invalidate-predecessor-" + transport,
				RequestKind: CodexRequestTurn,
			}
			status, body := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, predecessor))
			if status != http.StatusOK || !bytes.Contains(body, []byte(`"encrypted_content":"opaque-normal-transport-state"`)) {
				t.Fatalf("continuity seed = %d %q, want encrypted A/200", status, body)
			}

			result, err := harness.continuity.InvalidateTaskAffinities(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.InvalidatedLeases != 1 {
				t.Fatalf("invalidated leases = %d, want 1", result.InvalidatedLeases)
			}
			harness.httpPlanner.PinnedAccountKey = codexInstalledHTTPValidationAccountB
			harness.webSocketPlanner.PinnedAccountKey = codexInstalledHTTPValidationAccountB
			successor := predecessor
			successor.TurnID = "normal-transport-turn-invalidate-successor-" + transport

			switch transport {
			case "http":
				status, body := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, successor))
				if status != http.StatusOK || !bytes.Contains(body, []byte(`"type":"response.completed"`)) {
					t.Fatalf("post-invalidation response = %d %q, want completed B/200", status, body)
				}
				receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
				if len(receipts) != 2 || receipts[0].accountID != "validation-upstream-a" || receipts[1].accountID != "validation-upstream-b" {
					t.Fatalf("post-invalidation HTTP receipts = %#v, want A then B", receipts)
				}
			case "websocket":
				connection := normalTransportGateWebSocket(t, harness)
				if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrameForLane(successor.SessionID, successor.ThreadID, successor.TurnID, "")); err != nil {
					t.Fatal(err)
				}
				normalTransportGateReadWSCompletion(t, connection)
				_ = connection.Close()
				handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
				frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
				if len(handshakes) != 1 || len(frames) != 1 || frames[0].accountID != "validation-upstream-b" {
					t.Fatalf("post-invalidation WebSocket receipts = handshakes %#v frames %#v, want B only", handshakes, frames)
				}
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportHTTPSoftLimitDoesNotMigrate(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateHTTPSoftLimit)
	for _, turnID := range []string{"normal-transport-turn-http-soft-429", "normal-transport-turn-http-soft-429-next"} {
		metadata := CodexTurnMetadata{
			SessionID: "normal-transport-session-http-soft-429", ThreadID: "normal-transport-thread-http-soft-429", TurnID: turnID, RequestKind: CodexRequestTurn,
		}
		status, responseBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata))
		if status != http.StatusTooManyRequests || string(bytes.TrimSpace(responseBody)) != `{"error":{"type":"rate_limit_exceeded"}}` {
			t.Fatalf("soft-limit downstream response = %d %q, want exact provider 429", status, responseBody)
		}
	}
	receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
	if len(receipts) != 2 || receipts[0].accountID != "validation-upstream-a" || receipts[1].accountID != "validation-upstream-a" {
		t.Fatalf("soft-limit provider receipts = %#v, want A/429 then A/429 without account migration", receipts)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportHTTPPortableAuthFailureMigratesBeforeLeak(t *testing.T) {
	for _, test := range []struct {
		name       string
		scenario   normalTransportGateScenario
		authStatus int
	}{
		{name: "unauthorized", scenario: normalTransportGateHTTPPortableUnauthorized, authStatus: http.StatusUnauthorized},
		{name: "forbidden", scenario: normalTransportGateHTTPPortableForbidden, authStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			metadata := CodexTurnMetadata{
				SessionID: "normal-transport-session-http-auth-migrate", ThreadID: "normal-transport-thread-http-auth-migrate", TurnID: "normal-transport-turn-http-auth-migrate", RequestKind: CodexRequestTurn,
			}
			status, responseBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata))
			if status != http.StatusOK || bytes.Contains(responseBody, []byte("authentication_error")) || !bytes.Contains(responseBody, []byte(`"type":"response.completed"`)) {
				t.Fatalf("auth-failover downstream response = %d %q, want completed 200 without auth failure", status, responseBody)
			}
			receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
			if len(receipts) != 2 ||
				receipts[0].accountID != "validation-upstream-a" || receipts[0].status != test.authStatus ||
				receipts[1].accountID != "validation-upstream-b" || receipts[1].status != http.StatusOK ||
				receipts[0].payload != receipts[1].payload {
				t.Fatalf("auth-failover provider receipts = %#v, want byte-identical A/%d then B/200", receipts, test.authStatus)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportHTTPHardLimitSurfacesOnlyAfterPoolExhaustion(t *testing.T) {
	for _, retry := range []struct {
		name   string
		turnID string
	}{
		{name: "identical same-turn retry", turnID: "normal-transport-turn-http-all-429"},
		{name: "successor turn", turnID: "normal-transport-turn-http-all-429-successor"},
	} {
		t.Run(retry.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, normalTransportGateHTTPAllHardLimit)
			const finalHardLimit = `{"error":{"type":"usage_limit_reached"}}`
			metadata := CodexTurnMetadata{
				SessionID: "normal-transport-session-http-all-429", ThreadID: "normal-transport-thread-http-all-429", TurnID: "normal-transport-turn-http-all-429", RequestKind: CodexRequestTurn,
			}
			firstBody := normalTransportGateHTTPBody(t, metadata)
			status, responseBody := normalTransportGateHTTPCall(t, harness, firstBody)
			if status != http.StatusTooManyRequests || string(bytes.TrimSpace(responseBody)) != finalHardLimit {
				t.Fatalf("exhausted-pool downstream response = %d %q, want exact final usage-limit 429", status, responseBody)
			}
			receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
			if len(receipts) != 2 ||
				receipts[0].accountID != "validation-upstream-a" || receipts[0].status != http.StatusTooManyRequests ||
				receipts[1].accountID != "validation-upstream-b" || receipts[1].status != http.StatusTooManyRequests ||
				receipts[0].payload != receipts[1].payload {
				t.Fatalf("exhausted-pool provider receipts = %#v, want byte-identical A/429 then B/429 before downstream 429", receipts)
			}

			retryMetadata := metadata
			retryMetadata.TurnID = retry.turnID
			retryBody := firstBody
			if retry.turnID != metadata.TurnID {
				retryBody = normalTransportGateHTTPBody(t, retryMetadata)
			}
			retryStatus, retryResponse := normalTransportGateHTTPCall(t, harness, retryBody)
			if retryStatus != http.StatusTooManyRequests || string(bytes.TrimSpace(retryResponse)) != finalHardLimit {
				t.Fatalf("exhausted-pool retry = %d %q, want exact provider 429 rather than routing failure", retryStatus, retryResponse)
			}
			receipts = normalTransportGateReceipts(harness.backend.snapshot(), "http")
			if len(receipts) != 3 || receipts[2].accountID != "validation-upstream-b" || receipts[2].status != http.StatusTooManyRequests || receipts[2].payload != string(retryBody) {
				t.Fatalf("exhausted-pool retry receipts = %#v, want one deterministic B/429 probe", receipts)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportHTTPAdmittedAuthRetryKeepsBinding(t *testing.T) {
	for _, authStatus := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(authStatus), func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, normalTransportGateHTTPAdmittedAuthRetry)
			metadata := CodexTurnMetadata{
				SessionID: "normal-transport-session-admitted-auth", ThreadID: "normal-transport-thread-admitted-auth", TurnID: "normal-transport-turn-admitted-auth", RequestKind: CodexRequestTurn,
			}
			initialStatus, initialBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, metadata))
			if initialStatus != http.StatusOK || !bytes.Contains(initialBody, []byte(`"type":"response.completed"`)) {
				t.Fatalf("initial admitted request = %d %q", initialStatus, initialBody)
			}

			continuation := normalTransportGateHTTPContinuationBody(t, metadata, "normal-transport-http")
			harness.backend.armHTTPAuthFailure(authStatus)
			rejectedStatus, _ := normalTransportGateHTTPCall(t, harness, continuation)
			if rejectedStatus != authStatus {
				t.Fatalf("admitted auth rejection = %d, want %d", rejectedStatus, authStatus)
			}
			harness.backend.allowHTTPAuthRecovery()
			retryStatus, retryBody := normalTransportGateHTTPCall(t, harness, continuation)
			if retryStatus != http.StatusOK || !bytes.Contains(retryBody, []byte(`"type":"response.completed"`)) {
				t.Fatalf("admitted auth retry = %d %q receipts=%#v, want completed 200", retryStatus, retryBody, harness.backend.snapshot())
			}

			receipts := normalTransportGateReceipts(harness.backend.snapshot(), "http")
			if len(receipts) != 3 ||
				receipts[0].accountID != "validation-upstream-a" || receipts[0].status != http.StatusOK ||
				receipts[1].accountID != "validation-upstream-a" || receipts[1].status != authStatus ||
				receipts[2].accountID != "validation-upstream-a" || receipts[2].status != http.StatusOK ||
				receipts[1].payload != receipts[2].payload {
				t.Fatalf("admitted auth provider receipts = %#v, want A/200 A/%d A/200 with byte-identical continuation", receipts, authStatus)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketHardLimitMigratesBeforeLeak(t *testing.T) {
	for _, test := range []struct {
		name       string
		scenario   normalTransportGateScenario
		contentEnc string
		encrypted  bool
	}{
		{name: "application frame", scenario: normalTransportGateWSApplicationHardLimit},
		{name: "encrypted application frame", scenario: normalTransportGateWSApplicationHardLimit, encrypted: true},
		{name: "handshake", scenario: normalTransportGateWSHandshakeHardLimit},
		{name: "gzip handshake", scenario: normalTransportGateWSHandshakeHardLimit, contentEnc: "gzip"},
		{name: "zstd handshake", scenario: normalTransportGateWSHandshakeHardLimit, contentEnc: "zstd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			harness.backend.wsHandshakeCoding = test.contentEnc
			seedMetadata := CodexTurnMetadata{
				SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: "normal-transport-turn-ws-history", RequestKind: CodexRequestTurn,
			}
			seedStatus, seedBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, seedMetadata))
			if seedStatus != http.StatusOK || !bytes.Contains(seedBody, []byte(`"type":"response.completed"`)) {
				t.Fatalf("historical admission seed = %d %q, want completed A/200", seedStatus, seedBody)
			}
			connection := normalTransportGateWebSocket(t, harness)
			frame := normalTransportGateWSFrame("normal-transport-turn-ws-429", "")
			if test.encrypted {
				frame = bytes.Replace(frame, []byte(`"input":[]`), []byte(`"input":[{"encrypted_content":"opaque-normal-transport-state"}]`), 1)
			}
			if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			replies := normalTransportGateReadWSCompletion(t, connection)
			_ = connection.Close()
			if bytes.Contains(bytes.Join(replies, nil), []byte("usage_limit_reached")) {
				t.Fatalf("hard-limit frame leaked downstream: %q", replies)
			}

			receipts := harness.backend.snapshot()
			handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
			frames := normalTransportGateReceipts(receipts, "websocket_frame")
			if len(handshakes) != 2 || handshakes[0].accountID != "validation-upstream-a" || handshakes[1].accountID != "validation-upstream-b" || handshakes[1].status != http.StatusSwitchingProtocols {
				t.Fatalf("hard-limit handshakes = %#v, want A then B/101", handshakes)
			}
			if test.scenario == normalTransportGateWSApplicationHardLimit {
				if handshakes[0].status != http.StatusSwitchingProtocols || len(frames) != 2 || frames[0].accountID != "validation-upstream-a" || frames[1].accountID != "validation-upstream-b" || frames[0].payload != frames[1].payload {
					t.Fatalf("application hard-limit frames = %#v, want byte-identical A then B", frames)
				}
			} else if handshakes[0].status != http.StatusTooManyRequests || len(frames) != 1 || frames[0].accountID != "validation-upstream-b" || frames[0].payload != string(frame) {
				t.Fatalf("handshake hard-limit receipts = handshakes %#v frames %#v", handshakes, frames)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketStalledPrewarmFailsOpen(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSPrewarmStall)
	prewarm := []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"normal-transport-session-ws\",\"thread_id\":\"normal-transport-thread-ws\",\"turn_id\":\"\",\"request_kind\":\"prewarm\"}"},"input":[]}`)
	connection := normalTransportGateWebSocket(t, harness)
	if err := connection.WriteMessage(websocket.TextMessage, prewarm); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	_ = connection.Close()
	if err != nil || messageType != websocket.TextMessage {
		t.Fatalf("stalled prewarm fail-open = type %d payload %q err %v", messageType, payload, err)
	}
	normalTransportGateRequireSemanticJSON(t, payload, []byte(`{"type":"error","status":504,"error":{"type":"api_error"}}`), "stalled prewarm fail-open")

	retry := normalTransportGateWebSocket(t, harness)
	turn := normalTransportGateWSFrame("normal-transport-turn-after-stalled-prewarm", "")
	if err := retry.WriteMessage(websocket.TextMessage, turn); err != nil {
		t.Fatal(err)
	}
	replies := normalTransportGateReadWSCompletion(t, retry)
	_ = retry.Close()
	if !bytes.Contains(bytes.Join(replies, nil), []byte("normal-transport-websocket")) {
		t.Fatalf("turn after stalled prewarm = %q", replies)
	}

	frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
	if len(frames) != 2 || frames[0].connection != 1 || frames[0].payload != string(prewarm) || frames[1].connection != 2 || frames[1].payload != string(turn) {
		t.Fatalf("stalled prewarm transport receipts = %#v, want prewarm then fresh turn", frames)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketHandshakeHardLimitPreservesFinalProviderBody(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSAllHandshakeHardLimit)
	providerBody := []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached","code":"usage_limit_reached","message":"quota exhausted until reset","reset_at":"2026-09-01T00:00:00Z"}}`)
	harness.backend.wsHandshakeBody = providerBody

	connection := normalTransportGateWebSocket(t, harness)
	if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame("normal-transport-turn-ws-final-body", "")); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	_ = connection.Close()
	if err != nil || messageType != websocket.TextMessage {
		t.Fatalf("final handshake failure = type %d payload %q err %v, want exact provider body %q", messageType, payload, err, providerBody)
	}
	normalTransportGateRequireSemanticJSON(t, payload, providerBody, "final handshake failure")
	handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
	if len(handshakes) != 2 || handshakes[0].status != http.StatusTooManyRequests || handshakes[1].status != http.StatusTooManyRequests {
		t.Fatalf("final handshake receipts = %#v, want two provider 429 responses", handshakes)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketPolicyForbiddenDoesNotExpireAccount(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSHandshakeForbidden)
	providerBody := []byte(`{"type":"error","status":403,"error":{"type":"permission_denied","code":"safety_policy","message":"request forbidden by policy"}}`)
	harness.backend.wsHandshakeBody = providerBody

	connection := normalTransportGateWebSocket(t, harness)
	if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame("normal-transport-turn-ws-policy-403", "")); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	_ = connection.Close()
	if err != nil || messageType != websocket.TextMessage {
		t.Fatalf("policy failure = type %d payload %q err %v, want exact provider 403 %q", messageType, payload, err, providerBody)
	}
	normalTransportGateRequireSemanticJSON(t, payload, providerBody, "policy failure")
	handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
	if len(handshakes) != 1 || handshakes[0].accountID != "validation-upstream-a" || handshakes[0].status != http.StatusForbidden {
		t.Fatalf("policy forbidden handshakes = %#v, want only A/403", handshakes)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketHardLimitCapacityCarriesAcrossLanes(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSApplicationHardLimit)
	firstFrame := normalTransportGateWSFrameForLane("normal-transport-session-ws-capacity-one", "normal-transport-thread-ws-capacity-one", "normal-transport-turn-ws-capacity-one", "")
	first := normalTransportGateWebSocket(t, harness)
	if err := first.WriteMessage(websocket.TextMessage, firstFrame); err != nil {
		t.Fatal(err)
	}
	firstReplies := normalTransportGateReadWSCompletion(t, first)
	_ = first.Close()
	if bytes.Contains(bytes.Join(firstReplies, nil), []byte("usage_limit_reached")) {
		t.Fatalf("first-lane hard limit leaked downstream: %q", firstReplies)
	}

	secondFrame := normalTransportGateWSFrameForLane("normal-transport-session-ws-capacity-two", "normal-transport-thread-ws-capacity-two", "normal-transport-turn-ws-capacity-two", "")
	second := normalTransportGateWebSocket(t, harness)
	if err := second.WriteMessage(websocket.TextMessage, secondFrame); err != nil {
		t.Fatal(err)
	}
	secondReplies := normalTransportGateReadWSCompletion(t, second)
	_ = second.Close()
	if bytes.Contains(bytes.Join(secondReplies, nil), []byte("usage_limit_reached")) {
		t.Fatalf("second-lane hard limit leaked downstream: %q", secondReplies)
	}

	handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
	frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
	if len(handshakes) != 3 || len(frames) != 3 || handshakes[0].accountID != "validation-upstream-a" || frames[0].accountID != "validation-upstream-a" || frames[1].accountID == "validation-upstream-a" || frames[2].accountID == "validation-upstream-a" || frames[0].payload != string(firstFrame) || frames[1].payload != string(firstFrame) || frames[2].payload != string(secondFrame) {
		t.Fatalf("cross-lane capacity receipts = handshakes %#v frames %#v, want A rejected once and never retried on lane 2", handshakes, frames)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketApplicationAuthFailureMigratesBeforeLeak(t *testing.T) {
	for _, test := range []struct {
		name       string
		scenario   normalTransportGateScenario
		authStatus int
	}{
		{name: "unauthorized", scenario: normalTransportGateWSApplicationUnauthorized, authStatus: http.StatusUnauthorized},
		{name: "forbidden", scenario: normalTransportGateWSApplicationForbidden, authStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			connection := normalTransportGateWebSocket(t, harness)
			frame := normalTransportGateWSFrame("normal-transport-turn-ws-auth-application", "")
			if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			replies := normalTransportGateReadWSCompletion(t, connection)
			_ = connection.Close()
			if bytes.Contains(bytes.Join(replies, nil), []byte("authentication_error")) {
				t.Fatalf("application auth failure leaked downstream: %q", replies)
			}
			handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
			frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
			if len(handshakes) != 2 || handshakes[0].accountID != "validation-upstream-a" || handshakes[0].status != http.StatusSwitchingProtocols || handshakes[1].accountID != "validation-upstream-b" || handshakes[1].status != http.StatusSwitchingProtocols ||
				len(frames) != 2 || frames[0].accountID != "validation-upstream-a" || frames[1].accountID != "validation-upstream-b" || frames[0].payload != string(frame) || frames[1].payload != string(frame) {
				t.Fatalf("application auth receipts = handshakes %#v frames %#v, want byte-identical A/%d then B/completion", handshakes, frames, test.authStatus)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketHandshakeAuthFailureMigratesBeforeLeak(t *testing.T) {
	for _, test := range []struct {
		name       string
		scenario   normalTransportGateScenario
		authStatus int
	}{
		{name: "unauthorized", scenario: normalTransportGateWSHandshakeUnauthorized, authStatus: http.StatusUnauthorized},
		{name: "forbidden", scenario: normalTransportGateWSHandshakeForbidden, authStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			connection := normalTransportGateWebSocket(t, harness)
			frame := normalTransportGateWSFrame("normal-transport-turn-ws-auth-handshake", "")
			if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			replies := normalTransportGateReadWSCompletion(t, connection)
			_ = connection.Close()
			if bytes.Contains(bytes.Join(replies, nil), []byte("authentication_error")) {
				t.Fatalf("handshake auth failure leaked downstream: %q", replies)
			}
			receipts := harness.backend.snapshot()
			handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
			frames := normalTransportGateReceipts(receipts, "websocket_frame")
			if len(handshakes) != 2 || handshakes[0].accountID != "validation-upstream-a" || handshakes[0].status != test.authStatus || handshakes[1].accountID != "validation-upstream-b" || handshakes[1].status != http.StatusSwitchingProtocols ||
				len(frames) != 1 || frames[0].accountID != "validation-upstream-b" || frames[0].payload != string(frame) {
				t.Fatalf("handshake auth receipts = handshakes %#v frames %#v, want A/%d then B/101/completion", handshakes, frames, test.authStatus)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketRefreshOnlyCandidate(t *testing.T) {
	for _, test := range []struct {
		name        string
		scenario    normalTransportGateScenario
		wantAccount string
	}{
		{name: "refresh succeeds", scenario: normalTransportGateWSRefreshSuccess, wantAccount: "validation-upstream-a"},
		{name: "refresh unavailable rotates", scenario: normalTransportGateWSRefreshFailure, wantAccount: "validation-upstream-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			connection := normalTransportGateWebSocket(t, harness)
			frame := normalTransportGateWSFrame("normal-transport-turn-ws-refresh", "")
			if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			replies := normalTransportGateReadWSCompletion(t, connection)
			_ = connection.Close()
			if !bytes.Contains(bytes.Join(replies, nil), []byte("normal-transport-websocket")) {
				t.Fatalf("refresh-only completion = %q", replies)
			}
			if harness.refresh == nil {
				t.Fatal("refresh inventory unavailable")
			}
			if calls := harness.refresh.refreshCount(); calls != 1 {
				t.Fatalf("refresh calls = %d, want 1", calls)
			}
			receipts := harness.backend.snapshot()
			handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
			frames := normalTransportGateReceipts(receipts, "websocket_frame")
			if len(handshakes) != 1 || handshakes[0].accountID != test.wantAccount || handshakes[0].status != http.StatusSwitchingProtocols || len(frames) != 1 || frames[0].accountID != test.wantAccount || frames[0].payload != string(frame) {
				t.Fatalf("refresh-only receipts = handshakes %#v frames %#v, want only %s", handshakes, frames, test.wantAccount)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketDirectAuthUsesManagedRefreshFallback(t *testing.T) {
	for _, test := range []struct {
		name              string
		scenario          normalTransportGateScenario
		wantAccounts      []string
		wantCandidates    []string
		wantProviderError int
		wantSlot          uint32
	}{
		{
			name:           "refresh succeeds",
			scenario:       normalTransportGateWSDirectAuthRefreshSuccess,
			wantAccounts:   []string{"validation-upstream-a", "validation-upstream-a"},
			wantCandidates: []string{"validation-candidate-a-stale", "validation-candidate-a-refreshed"},
			wantSlot:       2,
		},
		{
			name:           "refresh failure rotates account",
			scenario:       normalTransportGateWSDirectAuthRefreshFailure,
			wantAccounts:   []string{"validation-upstream-a", "validation-upstream-b"},
			wantCandidates: []string{"validation-candidate-a-stale", "validation-candidate"},
			wantSlot:       3,
		},
		{
			name:              "refresh failure surfaces exact final auth",
			scenario:          normalTransportGateWSDirectAuthRefreshAllFailure,
			wantAccounts:      []string{"validation-upstream-a"},
			wantCandidates:    []string{"validation-candidate-a-stale"},
			wantProviderError: http.StatusUnauthorized,
			wantSlot:          1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			const turnID = "normal-transport-turn-ws-direct-refresh"
			frame := normalTransportGateWSFrame(turnID, "")
			connection := normalTransportGateWebSocket(t, harness)
			if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			if test.wantProviderError == 0 {
				normalTransportGateReadWSCompletion(t, connection)
			} else {
				normalTransportGateReadWSFinalProviderFailure(t, connection, test.wantProviderError)
			}
			_ = connection.Close()
			if harness.refresh == nil || harness.refresh.refreshCount() != 1 {
				t.Fatalf("direct-auth refresh calls = %v, want 1", harness.refresh)
			}
			handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
			if len(handshakes) != len(test.wantAccounts) {
				t.Fatalf("direct-auth refresh handshakes = %#v", handshakes)
			}
			for index := range test.wantAccounts {
				wantStatus := http.StatusSwitchingProtocols
				if index == 0 {
					wantStatus = http.StatusUnauthorized
				}
				if handshakes[index].accountID != test.wantAccounts[index] || handshakes[index].candidate != test.wantCandidates[index] || handshakes[index].status != wantStatus {
					t.Fatalf("direct-auth refresh handshake %d = %#v, want %s/%s/%d", index, handshakes[index], test.wantAccounts[index], test.wantCandidates[index], wantStatus)
				}
			}
			restored, err := harness.continuity.Store().LoadLane(
				NewCodexLeaseKey(CodexTurnMetadata{SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: turnID, RequestKind: CodexRequestTurn}),
				[]codex.AccountKey{codexInstalledHTTPValidationAccountA, codexInstalledHTTPValidationAccountB, codexInstalledHTTPValidationDefault},
				CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(restored.ResolvedRecords) != 1 {
				t.Fatalf("direct-auth refresh durable records = %#v", restored.ResolvedRecords)
			}
			record := restored.ResolvedRecords[0].Record
			attempt, found := codexLeaseAttemptByGeneration(record.Attempts, record.CurrentAttemptGeneration)
			if !found || attempt.Slot != test.wantSlot {
				t.Fatalf("direct-auth refresh attempt = %#v found=%t, want slot %d", attempt, found, test.wantSlot)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketHardLimitSurfacesOnlyAfterPoolExhaustion(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario normalTransportGateScenario
	}{
		{name: "application frame", scenario: normalTransportGateWSAllApplicationHardLimit},
		{name: "handshake", scenario: normalTransportGateWSAllHandshakeHardLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, retry := range []struct {
				name   string
				turnID string
			}{
				{name: "identical same-turn retry", turnID: "normal-transport-turn-ws-all-429"},
				{name: "successor turn", turnID: "normal-transport-turn-ws-all-429-successor"},
			} {
				t.Run(retry.name, func(t *testing.T) {
					harness := newNormalTransportGateHarness(t, test.scenario)
					connection := normalTransportGateWebSocket(t, harness)
					frame := normalTransportGateWSFrame("normal-transport-turn-ws-all-429", "")
					if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
						t.Fatal(err)
					}
					if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
						t.Fatal(err)
					}
					messageType, payload, err := connection.ReadMessage()
					_ = connection.Close()
					const finalHardLimit = `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`
					if err != nil || messageType != websocket.TextMessage {
						t.Fatalf("exhausted-pool downstream frame = type %d payload %q err %v, want exact final 429", messageType, payload, err)
					}
					normalTransportGateRequireSemanticJSON(t, payload, []byte(finalHardLimit), "exhausted-pool downstream frame")

					receipts := harness.backend.snapshot()
					handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
					frames := normalTransportGateReceipts(receipts, "websocket_frame")
					if len(handshakes) != 2 || handshakes[0].accountID != "validation-upstream-a" || handshakes[1].accountID != "validation-upstream-b" {
						t.Fatalf("exhausted-pool handshakes = %#v, want A then B", handshakes)
					}
					if test.scenario == normalTransportGateWSAllApplicationHardLimit {
						if handshakes[0].status != http.StatusSwitchingProtocols || handshakes[1].status != http.StatusSwitchingProtocols || len(frames) != 2 || frames[0].accountID != "validation-upstream-a" || frames[1].accountID != "validation-upstream-b" || frames[0].payload != string(frame) || frames[1].payload != string(frame) {
							t.Fatalf("exhausted application receipts = handshakes %#v frames %#v", handshakes, frames)
						}
					} else if handshakes[0].status != http.StatusTooManyRequests || handshakes[1].status != http.StatusTooManyRequests || len(frames) != 0 {
						t.Fatalf("exhausted handshake receipts = handshakes %#v frames %#v", handshakes, frames)
					}

					retryConnection := normalTransportGateWebSocket(t, harness)
					retryFrame := frame
					if retry.turnID != "normal-transport-turn-ws-all-429" {
						retryFrame = normalTransportGateWSFrame(retry.turnID, "")
					}
					if err := retryConnection.WriteMessage(websocket.TextMessage, retryFrame); err != nil {
						t.Fatal(err)
					}
					if err := retryConnection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
						t.Fatal(err)
					}
					messageType, payload, err = retryConnection.ReadMessage()
					_ = retryConnection.Close()
					if err != nil || messageType != websocket.TextMessage {
						t.Fatalf("exhausted-pool retry = type %d payload %q err %v, want exact provider 429 rather than 1011", messageType, payload, err)
					}
					normalTransportGateRequireSemanticJSON(t, payload, []byte(finalHardLimit), "exhausted-pool retry")
					receipts = harness.backend.snapshot()
					handshakes = normalTransportGateReceipts(receipts, "websocket_handshake")
					frames = normalTransportGateReceipts(receipts, "websocket_frame")
					wantRetryHandshakeStatus := http.StatusTooManyRequests
					if test.scenario == normalTransportGateWSAllApplicationHardLimit {
						wantRetryHandshakeStatus = http.StatusSwitchingProtocols
					}
					if len(handshakes) != 3 || handshakes[2].accountID != "validation-upstream-b" || handshakes[2].status != wantRetryHandshakeStatus {
						t.Fatalf("exhausted-pool retry handshakes = %#v, want one deterministic B probe", handshakes)
					}
					if test.scenario == normalTransportGateWSAllApplicationHardLimit {
						if len(frames) != 3 || frames[2].accountID != "validation-upstream-b" || frames[2].payload != string(retryFrame) {
							t.Fatalf("exhausted application retry frames = %#v, want only B probe", frames)
						}
					} else if len(frames) != 0 {
						t.Fatalf("exhausted handshake retry frames = %#v, want none", frames)
					}
					harness.backend.assertNoFailure(t)
				})
			}
		})
	}
}

func TestNormalProxyTransportWebSocketCurrentAdmissionFailureRelaysAndRetriesNextAccount(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario normalTransportGateScenario
	}{
		{name: "hard limit", scenario: normalTransportGateWSAdmittedHardLimit},
		{name: "unauthorized", scenario: normalTransportGateWSAdmittedUnauthorized},
		{name: "forbidden", scenario: normalTransportGateWSAdmittedForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			const retryTurn = "normal-transport-turn-ws-admitted"
			frame := normalTransportGateWSFrame(retryTurn, "")
			first := normalTransportGateWebSocket(t, harness)
			if err := first.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			normalTransportGateReadWSAdmissionThenProviderFailure(t, first, normalTransportGateAdmittedFailureStatus(test.scenario))
			_ = first.Close()

			second := normalTransportGateWebSocket(t, harness)
			if err := second.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			replies := normalTransportGateReadWSCompletion(t, second)
			_ = second.Close()
			if !bytes.Contains(bytes.Join(replies, nil), []byte("normal-transport-websocket")) {
				t.Fatalf("admitted-failure reset completion = %q", replies)
			}

			receipts := harness.backend.snapshot()
			handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
			frames := normalTransportGateReceipts(receipts, "websocket_frame")
			if len(handshakes) != 2 || handshakes[0].accountID != "validation-upstream-a" || handshakes[0].status != http.StatusSwitchingProtocols || handshakes[1].accountID != "validation-upstream-b" || handshakes[1].status != http.StatusSwitchingProtocols ||
				len(frames) != 2 || frames[0].accountID != "validation-upstream-a" || frames[1].accountID != "validation-upstream-b" || frames[0].payload != string(frame) || frames[1].payload != string(frame) {
				t.Fatalf("admitted-failure receipts = handshakes %#v frames %#v, want A then same-frame B reconnect", handshakes, frames)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketSequentialAdmittedHardLimitsSkipUnavailableAccounts(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSSequentialAdmittedHardLimit)
	const retryTurn = "normal-transport-turn-ws-sequential-admitted"
	frame := normalTransportGateWSFrame(retryTurn, "")
	for _, account := range []string{"validation-upstream-a", "validation-upstream-b"} {
		connection := normalTransportGateWebSocket(t, harness)
		if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
			t.Fatal(err)
		}
		normalTransportGateReadWSAdmissionThenProviderFailure(t, connection, http.StatusTooManyRequests)
		_ = connection.Close()
		receipts := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
		if len(receipts) == 0 || receipts[len(receipts)-1].accountID != account {
			t.Fatalf("sequential admitted receipt = %#v, want latest %s", receipts, account)
		}
	}

	third := normalTransportGateWebSocket(t, harness)
	if err := third.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatal(err)
	}
	replies := normalTransportGateReadWSCompletion(t, third)
	if !bytes.Contains(bytes.Join(replies, nil), []byte("normal-transport-websocket")) {
		t.Fatalf("sequential admitted reset completion = %q", replies)
	}

	successorFrame := normalTransportGateWSFrame("normal-transport-turn-ws-sequential-admitted-successor", `,"previous_response_id":"normal-transport-websocket"`)
	if err := third.WriteMessage(websocket.TextMessage, successorFrame); err != nil {
		t.Fatal(err)
	}
	successorReplies := normalTransportGateReadWSCompletion(t, third)
	_ = third.Close()
	if !bytes.Contains(bytes.Join(successorReplies, nil), []byte("normal-transport-websocket")) {
		t.Fatalf("sequential admitted successor completion = %q", successorReplies)
	}

	handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
	frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
	wantHandshakes := []string{"validation-upstream-a", "validation-upstream-b", "validation-upstream-default"}
	wantFrames := []string{"validation-upstream-a", "validation-upstream-b", "validation-upstream-default", "validation-upstream-default"}
	if len(handshakes) != len(wantHandshakes) || len(frames) != len(wantFrames) {
		t.Fatalf("sequential admitted receipts = handshakes %#v frames %#v", handshakes, frames)
	}
	for index, account := range wantHandshakes {
		if handshakes[index].accountID != account || handshakes[index].status != http.StatusSwitchingProtocols {
			t.Fatalf("sequential admitted handshake %d = %#v, want %s", index, handshakes[index], account)
		}
	}
	for index, account := range wantFrames {
		if frames[index].accountID != account {
			t.Fatalf("sequential admitted frame %d = %#v, want %s", index, frames[index], account)
		}
	}
	if frames[0].payload != string(frame) || frames[1].payload != string(frame) || frames[2].payload != string(frame) || frames[3].payload != string(successorFrame) {
		t.Fatalf("sequential admitted payloads = %#v", frames)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketSequentialAuthFailuresRemainRequestScoped(t *testing.T) {
	for _, test := range []struct {
		name       string
		scenario   normalTransportGateScenario
		authStatus int
		admitted   bool
	}{
		{name: "application unauthorized", scenario: normalTransportGateWSSequentialAdmittedUnauthorized, authStatus: http.StatusUnauthorized, admitted: true},
		{name: "application forbidden", scenario: normalTransportGateWSSequentialAdmittedForbidden, authStatus: http.StatusForbidden, admitted: true},
		{name: "handshake unauthorized", scenario: normalTransportGateWSSequentialHandshakeUnauthorized, authStatus: http.StatusUnauthorized},
		{name: "handshake forbidden", scenario: normalTransportGateWSSequentialHandshakeForbidden, authStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			const retryTurn = "normal-transport-turn-ws-sequential-auth"
			frame := normalTransportGateWSFrame(retryTurn, "")

			var connection *websocket.Conn
			if test.admitted {
				for _, account := range []string{"validation-upstream-a", "validation-upstream-b"} {
					connection = normalTransportGateWebSocket(t, harness)
					if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
						t.Fatal(err)
					}
					normalTransportGateReadWSAdmissionThenProviderFailure(t, connection, test.authStatus)
					_ = connection.Close()
					frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
					if len(frames) == 0 || frames[len(frames)-1].accountID != account {
						t.Fatalf("sequential auth receipt = %#v, want latest %s", frames, account)
					}
				}
				connection = normalTransportGateWebSocket(t, harness)
			} else {
				connection = normalTransportGateWebSocket(t, harness)
			}
			if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			replies := normalTransportGateReadWSCompletion(t, connection)
			if bytes.Contains(bytes.Join(replies, nil), []byte("authentication_error")) || !bytes.Contains(bytes.Join(replies, nil), []byte("normal-transport-websocket")) {
				t.Fatalf("sequential auth reset completion = %q", replies)
			}

			const laterTurn = "normal-transport-turn-ws-sequential-auth-later"
			snapshot, err := harness.continuity.LoadRouteSnapshot(
				context.Background(),
				NewCodexLeaseKey(CodexTurnMetadata{SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: laterTurn, RequestKind: CodexRequestTurn}),
				[]codex.AccountKey{codexInstalledHTTPValidationAccountA, codexInstalledHTTPValidationAccountB, codexInstalledHTTPValidationDefault},
				CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.UnavailableAccountKeys) != 0 {
				t.Fatalf("successor generic unavailable accounts = %#v, want request-scoped state cleared", snapshot.UnavailableAccountKeys)
			}

			laterFrame := normalTransportGateWSFrame(laterTurn, "")
			if err := connection.WriteMessage(websocket.TextMessage, laterFrame); err != nil {
				t.Fatal(err)
			}
			laterReplies := normalTransportGateReadWSCompletion(t, connection)
			_ = connection.Close()
			if !bytes.Contains(bytes.Join(laterReplies, nil), []byte("normal-transport-websocket")) {
				t.Fatalf("later explicit request completion = %q", laterReplies)
			}

			handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
			frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
			wantAccounts := []string{"validation-upstream-a", "validation-upstream-b", "validation-upstream-default", "validation-upstream-default"}
			if len(handshakes) != len(wantAccounts) {
				t.Fatalf("sequential auth handshakes = %#v, want A/%d B/%d C/101 then request-scoped A/101", handshakes, test.authStatus, test.authStatus)
			}
			for index, account := range wantAccounts {
				wantStatus := http.StatusSwitchingProtocols
				if !test.admitted && index < 2 {
					wantStatus = test.authStatus
				}
				if handshakes[index].accountID != account || handshakes[index].status != wantStatus {
					t.Fatalf("sequential auth handshake %d = %#v, want %s/%d", index, handshakes[index], account, wantStatus)
				}
			}
			wantFrameAccounts := wantAccounts
			if !test.admitted {
				wantFrameAccounts = wantAccounts[2:]
			}
			if len(frames) != len(wantFrameAccounts) {
				t.Fatalf("sequential auth frames = %#v, want accounts %#v", frames, wantFrameAccounts)
			}
			for index, account := range wantFrameAccounts {
				if frames[index].accountID != account {
					t.Fatalf("sequential auth frame %d = %#v, want %s", index, frames[index], account)
				}
				wantPayload := frame
				if index == len(wantFrameAccounts)-1 {
					wantPayload = laterFrame
				}
				if frames[index].payload != string(wantPayload) {
					t.Fatalf("sequential auth frame %d payload = %q, want %q", index, frames[index].payload, wantPayload)
				}
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketAllAuthFailuresRestartFreshCycle(t *testing.T) {
	for _, test := range []struct {
		name       string
		scenario   normalTransportGateScenario
		authStatus int
		admitted   bool
	}{
		{name: "application unauthorized", scenario: normalTransportGateWSAllAdmittedUnauthorized, authStatus: http.StatusUnauthorized, admitted: true},
		{name: "application forbidden", scenario: normalTransportGateWSAllAdmittedForbidden, authStatus: http.StatusForbidden, admitted: true},
		{name: "handshake unauthorized", scenario: normalTransportGateWSAllHandshakeUnauthorized, authStatus: http.StatusUnauthorized},
		{name: "handshake forbidden", scenario: normalTransportGateWSAllHandshakeForbidden, authStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			const turnID = "normal-transport-turn-ws-all-auth"
			frame := normalTransportGateWSFrame(turnID, "")
			var connection *websocket.Conn
			if test.admitted {
				for range 2 {
					connection = normalTransportGateWebSocket(t, harness)
					if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
						t.Fatal(err)
					}
					normalTransportGateReadWSAdmissionThenProviderFailure(t, connection, test.authStatus)
					_ = connection.Close()
				}
				connection = normalTransportGateWebSocket(t, harness)
			} else {
				connection = normalTransportGateWebSocket(t, harness)
			}
			if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			normalTransportGateReadWSFinalAuthFailure(t, connection, test.authStatus, test.admitted)
			_ = connection.Close()

			snapshot, err := harness.continuity.LoadRouteSnapshot(
				context.Background(),
				NewCodexLeaseKey(CodexTurnMetadata{SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: turnID, RequestKind: CodexRequestTurn}),
				[]codex.AccountKey{codexInstalledHTTPValidationAccountA, codexInstalledHTTPValidationAccountB, codexInstalledHTTPValidationDefault},
				CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.admitted && len(snapshot.UnavailableAccountKeys) != 0 {
				t.Fatalf("final auth failure left unavailable accounts = %#v, want fresh explicit cycle", snapshot.UnavailableAccountKeys)
			}

			retry := normalTransportGateWebSocket(t, harness)
			if err := retry.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			replies := normalTransportGateReadWSCompletion(t, retry)
			_ = retry.Close()
			if !bytes.Contains(bytes.Join(replies, nil), []byte("normal-transport-websocket")) {
				t.Fatalf("fresh auth cycle completion = %q", replies)
			}
			snapshot, err = harness.continuity.LoadRouteSnapshot(
				context.Background(),
				NewCodexLeaseKey(CodexTurnMetadata{SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: turnID, RequestKind: CodexRequestTurn}),
				[]codex.AccountKey{codexInstalledHTTPValidationAccountA, codexInstalledHTTPValidationAccountB, codexInstalledHTTPValidationDefault},
				CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.UnavailableAccountKeys) != 0 {
				t.Fatalf("fresh auth cycle retained unavailable accounts = %#v", snapshot.UnavailableAccountKeys)
			}

			handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
			frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
			if len(handshakes) != 4 {
				t.Fatalf("all-auth handshakes = %#v, want three failures then one fresh success", handshakes)
			}
			wantAccounts := []string{"validation-upstream-a", "validation-upstream-b", "validation-upstream-default"}
			for index, account := range wantAccounts {
				wantStatus := http.StatusSwitchingProtocols
				if !test.admitted {
					wantStatus = test.authStatus
				}
				if handshakes[index].accountID != account || handshakes[index].status != wantStatus {
					t.Fatalf("all-auth handshake %d = %#v, want %s/%d", index, handshakes[index], account, wantStatus)
				}
			}
			if handshakes[3].status != http.StatusSwitchingProtocols {
				t.Fatalf("fresh-cycle handshake = %#v, want 101", handshakes[3])
			}
			wantFrames := 4
			if !test.admitted {
				wantFrames = 1
			}
			if len(frames) != wantFrames || frames[len(frames)-1].payload != string(frame) {
				t.Fatalf("all-auth frames = %#v, want %d with fresh request last", frames, wantFrames)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketNonPortableHardLimitClosesRetryablyAndResetsAccount(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario normalTransportGateScenario
	}{
		{name: "application frame", scenario: normalTransportGateWSNonPortableApplicationHardLimit},
		{name: "handshake", scenario: normalTransportGateWSNonPortableHandshakeHardLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			seedMetadata := CodexTurnMetadata{
				SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: "normal-transport-turn-ws-seed", RequestKind: CodexRequestTurn,
			}
			seedStatus, seedBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, seedMetadata))
			if seedStatus != http.StatusOK || !bytes.Contains(seedBody, []byte(`"type":"response.completed"`)) {
				t.Fatalf("continuity seed = %d %q, want completed A/200", seedStatus, seedBody)
			}

			const retryTurn = "normal-transport-turn-ws-nonportable"
			nonPortable := normalTransportGateWSFrame(retryTurn, `,"previous_response_id":"normal-transport-http"`)
			first := normalTransportGateWebSocket(t, harness)
			if err := first.WriteMessage(websocket.TextMessage, nonPortable); err != nil {
				t.Fatal(err)
			}
			normalTransportGateReadWSAccountUnavailable(t, first)
			_ = first.Close()

			reset := normalTransportGateWSFrame(retryTurn, "")
			second := normalTransportGateWebSocket(t, harness)
			if err := second.WriteMessage(websocket.TextMessage, reset); err != nil {
				t.Fatal(err)
			}
			replies := normalTransportGateReadWSCompletion(t, second)
			_ = second.Close()
			if !bytes.Contains(bytes.Join(replies, nil), []byte("normal-transport-websocket")) {
				t.Fatalf("reset completion = %q", replies)
			}

			receipts := harness.backend.snapshot()
			handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
			frames := normalTransportGateReceipts(receipts, "websocket_frame")
			if len(handshakes) != 2 || handshakes[0].accountID != "validation-upstream-a" || handshakes[1].accountID != "validation-upstream-b" || handshakes[1].status != http.StatusSwitchingProtocols {
				t.Fatalf("non-portable hard-limit handshakes = %#v, want A then B/101", handshakes)
			}
			if test.scenario == normalTransportGateWSNonPortableApplicationHardLimit {
				if handshakes[0].status != http.StatusSwitchingProtocols || len(frames) != 2 || frames[0].accountID != "validation-upstream-a" || frames[0].payload != string(nonPortable) || frames[1].accountID != "validation-upstream-b" || frames[1].payload != string(reset) {
					t.Fatalf("non-portable application receipts = handshakes %#v frames %#v", handshakes, frames)
				}
			} else if handshakes[0].status != http.StatusTooManyRequests || len(frames) != 1 || frames[0].accountID != "validation-upstream-b" || frames[0].payload != string(reset) {
				t.Fatalf("non-portable handshake receipts = handshakes %#v frames %#v", handshakes, frames)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketResetSkipsEveryUnavailableAccount(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario normalTransportGateScenario
	}{
		{name: "application frame", scenario: normalTransportGateWSNonPortableTwoApplicationHardLimit},
		{name: "handshake", scenario: normalTransportGateWSNonPortableTwoHandshakeHardLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			seedMetadata := CodexTurnMetadata{
				SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: "normal-transport-turn-ws-unavailable-seed", RequestKind: CodexRequestTurn,
			}
			seedStatus, seedBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, seedMetadata))
			if seedStatus != http.StatusOK || !bytes.Contains(seedBody, []byte(`"encrypted_content":"opaque-normal-transport-state"`)) {
				t.Fatalf("unavailable-account seed = %d %q, want encrypted A/200", seedStatus, seedBody)
			}

			const retryTurn = "normal-transport-turn-ws-unavailable-reset"
			nonPortable := normalTransportGateWSFrame(retryTurn, `,"previous_response_id":"normal-transport-http"`)
			first := normalTransportGateWebSocket(t, harness)
			if err := first.WriteMessage(websocket.TextMessage, nonPortable); err != nil {
				t.Fatal(err)
			}
			normalTransportGateReadWSAccountUnavailable(t, first)
			_ = first.Close()

			reset := normalTransportGateWSFrame(retryTurn, "")
			second := normalTransportGateWebSocket(t, harness)
			if err := second.WriteMessage(websocket.TextMessage, reset); err != nil {
				t.Fatal(err)
			}
			replies := normalTransportGateReadWSCompletion(t, second)
			_ = second.Close()
			if !bytes.Contains(bytes.Join(replies, nil), []byte("normal-transport-websocket")) {
				t.Fatalf("unavailable-account reset completion = %q", replies)
			}

			receipts := harness.backend.snapshot()
			handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
			frames := normalTransportGateReceipts(receipts, "websocket_frame")
			if len(handshakes) != 3 || handshakes[0].accountID != "validation-upstream-a" || handshakes[1].accountID != "validation-upstream-b" || handshakes[2].accountID != "validation-upstream-default" || handshakes[2].status != http.StatusSwitchingProtocols {
				t.Fatalf("unavailable-account handshakes = %#v, want A then B then only C", handshakes)
			}
			if test.scenario == normalTransportGateWSNonPortableTwoApplicationHardLimit {
				if handshakes[0].status != http.StatusSwitchingProtocols || handshakes[1].status != http.StatusSwitchingProtocols || len(frames) != 3 || frames[0].payload != string(nonPortable) || frames[1].payload != string(reset) || frames[2].payload != string(reset) {
					t.Fatalf("unavailable-account application receipts = handshakes %#v frames %#v", handshakes, frames)
				}
			} else if handshakes[0].status != http.StatusTooManyRequests || handshakes[1].status != http.StatusTooManyRequests || len(frames) != 1 || frames[0].accountID != "validation-upstream-default" || frames[0].payload != string(reset) {
				t.Fatalf("unavailable-account handshake receipts = handshakes %#v frames %#v", handshakes, frames)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketNonPortableHardLimitSurfacesAfterResetExhaustsPool(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario normalTransportGateScenario
	}{
		{name: "application frame", scenario: normalTransportGateWSNonPortableAllApplicationHardLimit},
		{name: "handshake", scenario: normalTransportGateWSNonPortableAllHandshakeHardLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			seedMetadata := CodexTurnMetadata{
				SessionID: "normal-transport-session-ws", ThreadID: "normal-transport-thread-ws", TurnID: "normal-transport-turn-ws-exhaustion-seed", RequestKind: CodexRequestTurn,
			}
			seedStatus, seedBody := normalTransportGateHTTPCall(t, harness, normalTransportGateHTTPBody(t, seedMetadata))
			if seedStatus != http.StatusOK || !bytes.Contains(seedBody, []byte(`"encrypted_content":"opaque-normal-transport-state"`)) {
				t.Fatalf("exhaustion continuity seed = %d %q, want encrypted A/200", seedStatus, seedBody)
			}

			const retryTurn = "normal-transport-turn-ws-nonportable-exhaustion"
			nonPortable := normalTransportGateWSFrame(retryTurn, `,"previous_response_id":"normal-transport-http"`)
			first := normalTransportGateWebSocket(t, harness)
			if err := first.WriteMessage(websocket.TextMessage, nonPortable); err != nil {
				t.Fatal(err)
			}
			normalTransportGateReadWSAccountUnavailable(t, first)
			_ = first.Close()

			reset := normalTransportGateWSFrame(retryTurn, "")
			second := normalTransportGateWebSocket(t, harness)
			if err := second.WriteMessage(websocket.TextMessage, reset); err != nil {
				t.Fatal(err)
			}
			if err := second.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			messageType, payload, err := second.ReadMessage()
			_ = second.Close()
			const finalHardLimit = `{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`
			if err != nil || messageType != websocket.TextMessage {
				t.Fatalf("non-portable exhausted reset = type %d payload %q err %v, want exact final 429", messageType, payload, err)
			}
			normalTransportGateRequireSemanticJSON(t, payload, []byte(finalHardLimit), "non-portable exhausted reset")

			receipts := harness.backend.snapshot()
			handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
			frames := normalTransportGateReceipts(receipts, "websocket_frame")
			if len(handshakes) != 2 || handshakes[0].accountID != "validation-upstream-a" || handshakes[1].accountID != "validation-upstream-b" {
				t.Fatalf("non-portable exhausted handshakes = %#v, want A then B only", handshakes)
			}
			if test.scenario == normalTransportGateWSNonPortableAllApplicationHardLimit {
				if handshakes[0].status != http.StatusSwitchingProtocols || handshakes[1].status != http.StatusSwitchingProtocols || len(frames) != 2 || frames[0].payload != string(nonPortable) || frames[1].payload != string(reset) {
					t.Fatalf("non-portable exhausted application receipts = handshakes %#v frames %#v", handshakes, frames)
				}
			} else if handshakes[0].status != http.StatusTooManyRequests || handshakes[1].status != http.StatusTooManyRequests || len(frames) != 0 {
				t.Fatalf("non-portable exhausted handshake receipts = handshakes %#v frames %#v", handshakes, frames)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketCredentialRetriesUseExactDurableSlots(t *testing.T) {
	for _, test := range []struct {
		name              string
		scenario          normalTransportGateScenario
		wantStatuses      []int
		wantAttemptState  CodexAttemptState
		wantProviderError int
	}{
		{
			name:             "first unauthorized then second succeeds",
			scenario:         normalTransportGateWSCredentialAuthThenSuccess,
			wantStatuses:     []int{http.StatusUnauthorized, http.StatusSwitchingProtocols},
			wantAttemptState: CodexAttemptProviderCompleted,
		},
		{
			name:              "first unauthorized then second hard limit",
			scenario:          normalTransportGateWSCredentialAuthThenHardLimit,
			wantStatuses:      []int{http.StatusUnauthorized, http.StatusTooManyRequests},
			wantAttemptState:  CodexAttemptAccountUnavailable,
			wantProviderError: http.StatusTooManyRequests,
		},
		{
			name:              "both unauthorized",
			scenario:          normalTransportGateWSCredentialAllUnauthorized,
			wantStatuses:      []int{http.StatusUnauthorized, http.StatusUnauthorized},
			wantAttemptState:  CodexAttemptAccountUnavailable,
			wantProviderError: http.StatusUnauthorized,
		},
		{
			name:              "both forbidden",
			scenario:          normalTransportGateWSCredentialAllForbidden,
			wantStatuses:      []int{http.StatusForbidden, http.StatusForbidden},
			wantAttemptState:  CodexAttemptAccountUnavailable,
			wantProviderError: http.StatusForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newNormalTransportGateHarness(t, test.scenario)
			const turnID = "normal-transport-turn-ws-credential-provenance"
			frame := normalTransportGateWSFrame(turnID, "")
			connection := normalTransportGateWebSocket(t, harness)
			if err := connection.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Fatal(err)
			}
			if test.wantProviderError == 0 {
				normalTransportGateReadWSCompletion(t, connection)
			} else {
				normalTransportGateReadWSFinalProviderFailure(t, connection, test.wantProviderError)
			}
			_ = connection.Close()

			handshakes := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_handshake")
			if len(handshakes) != 2 {
				t.Fatalf("credential handshakes = %#v, want two exact candidates", handshakes)
			}
			wantCandidates := []string{"validation-candidate-a-one", "validation-candidate-a-two"}
			for index, candidate := range wantCandidates {
				if handshakes[index].accountID != "validation-upstream-a" || handshakes[index].candidate != candidate || handshakes[index].status != test.wantStatuses[index] {
					t.Fatalf("credential handshake %d = %#v, want A/%s/%d", index, handshakes[index], candidate, test.wantStatuses[index])
				}
			}
			frames := normalTransportGateReceipts(harness.backend.snapshot(), "websocket_frame")
			wantFrames := 0
			if test.wantProviderError == 0 {
				wantFrames = 1
			}
			if len(frames) != wantFrames || wantFrames == 1 && (frames[0].candidate != wantCandidates[1] || frames[0].payload != string(frame)) {
				t.Fatalf("credential frames = %#v, want %d on second candidate only", frames, wantFrames)
			}

			restored, err := harness.continuity.Store().LoadLane(
				NewCodexLeaseKey(CodexTurnMetadata{
					SessionID:   "normal-transport-session-ws",
					ThreadID:    "normal-transport-thread-ws",
					TurnID:      turnID,
					RequestKind: CodexRequestTurn,
				}),
				[]codex.AccountKey{codexInstalledHTTPValidationAccountA},
				CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(restored.ResolvedRecords) != 1 {
				t.Fatalf("credential durable records = %#v, want one", restored.ResolvedRecords)
			}
			record := restored.ResolvedRecords[0].Record
			attempt, found := codexLeaseAttemptByGeneration(record.Attempts, record.CurrentAttemptGeneration)
			if !found || attempt.Slot != 2 || attempt.State != test.wantAttemptState {
				t.Fatalf("credential durable attempt = %#v found=%t, want slot 2 state %d", attempt, found, test.wantAttemptState)
			}
			if len(record.AttemptEnvelope.Slots) != 2 || record.AttemptEnvelope.Slots[1].CandidateHash != harness.continuity.Store().hash("candidate", "validation-candidate-a-two") {
				t.Fatalf("credential durable envelope = %#v, want second candidate provenance", record.AttemptEnvelope)
			}
			harness.backend.assertNoFailure(t)
		})
	}
}

func TestNormalProxyTransportWebSocketReconnectsPortableSuccessorAfterCompletedIdleEOF(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSIdleEOF)
	connection := normalTransportGateWebSocket(t, harness)
	defer connection.Close()
	if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame("normal-transport-turn-idle-one", "")); err != nil {
		t.Fatal(err)
	}
	first := normalTransportGateReadWSCompletion(t, connection)
	if !bytes.Contains(bytes.Join(first, nil), []byte("normal-transport-idle-one")) {
		t.Fatalf("first completion = %q", first)
	}
	select {
	case <-harness.backend.firstWSClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not close completed idle connection")
	}

	secondFrame := normalTransportGateWSFrame("normal-transport-turn-idle-two", "")
	if err := connection.WriteMessage(websocket.TextMessage, secondFrame); err != nil {
		t.Fatal(err)
	}
	second := normalTransportGateReadWSCompletion(t, connection)
	if !bytes.Contains(bytes.Join(second, nil), []byte("normal-transport-idle-two")) {
		t.Fatalf("successor completion = %q", second)
	}

	receipts := harness.backend.snapshot()
	handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
	frames := normalTransportGateReceipts(receipts, "websocket_frame")
	if len(handshakes) != 2 || len(frames) != 2 || frames[0].connection == frames[1].connection || frames[1].payload != string(secondFrame) {
		t.Fatalf("idle reconnect receipts = handshakes %#v frames %#v", handshakes, frames)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketReusesCompletedUpstreamForSuccessor(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSIdleHeldOpen)
	connection := normalTransportGateWebSocket(t, harness)
	defer connection.Close()
	if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame("normal-transport-turn-idle-held-one", "")); err != nil {
		t.Fatal(err)
	}
	first := normalTransportGateReadWSCompletion(t, connection)
	if !bytes.Contains(bytes.Join(first, nil), []byte("normal-transport-idle-held-one")) {
		t.Fatalf("first held-idle completion = %q", first)
	}

	secondFrame := normalTransportGateWSFrame("normal-transport-turn-idle-held-two", `,"previous_response_id":"normal-transport-idle-held-one"`)
	if err := connection.WriteMessage(websocket.TextMessage, secondFrame); err != nil {
		t.Fatal(err)
	}
	second := normalTransportGateReadWSCompletion(t, connection)
	if !bytes.Contains(bytes.Join(second, nil), []byte("normal-transport-idle-held-two")) {
		t.Fatalf("held-idle successor completion = %q", second)
	}

	receipts := harness.backend.snapshot()
	handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
	frames := normalTransportGateReceipts(receipts, "websocket_frame")
	if len(handshakes) != 1 || len(frames) != 2 || frames[0].connection != frames[1].connection || frames[1].payload != string(secondFrame) {
		t.Fatalf("held-idle reuse receipts = handshakes %#v frames %#v", handshakes, frames)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketRequestsFullCreateWhenIdleClosesAfterPrecheck(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSIdleCloseAfterPrecheck)
	connection := normalTransportGateWebSocket(t, harness)
	if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame("normal-transport-turn-idle-race-one", "")); err != nil {
		t.Fatal(err)
	}
	first := normalTransportGateReadWSCompletion(t, connection)
	if !bytes.Contains(bytes.Join(first, nil), []byte("normal-transport-idle-race-one")) {
		t.Fatalf("first raced-idle completion = %q", first)
	}

	secondFrame := normalTransportGateWSFrame("normal-transport-turn-idle-race-two", `,"previous_response_id":"normal-transport-idle-race-one"`)
	if err := connection.WriteMessage(websocket.TextMessage, secondFrame); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSAccountUnavailable(t, connection)
	_ = connection.Close()

	retry := normalTransportGateWebSocket(t, harness)
	defer retry.Close()
	fullCreate := normalTransportGateWSFrame("normal-transport-turn-idle-race-two", "")
	if err := retry.WriteMessage(websocket.TextMessage, fullCreate); err != nil {
		t.Fatal(err)
	}
	second := normalTransportGateReadWSCompletion(t, retry)
	if !bytes.Contains(bytes.Join(second, nil), []byte("normal-transport-idle-race-two")) {
		t.Fatalf("raced-idle full-create completion = %q", second)
	}

	receipts := harness.backend.snapshot()
	handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
	frames := normalTransportGateReceipts(receipts, "websocket_frame")
	if len(handshakes) != 2 || len(frames) != 2 || frames[0].connection == frames[1].connection || frames[1].payload != string(fullCreate) {
		t.Fatalf("raced-idle full-create receipts = handshakes %#v frames %#v", handshakes, frames)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketReusesSocketForResponseAnchorSuccessor(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSResponseAnchorSocketBound)
	connection := normalTransportGateWebSocket(t, harness)
	defer connection.Close()

	firstFrame := normalTransportGateWSFrame("normal-transport-turn-socket-bound-one", "")
	if err := connection.WriteMessage(websocket.TextMessage, firstFrame); err != nil {
		t.Fatal(err)
	}
	first := normalTransportGateReadWSCompletion(t, connection)
	if !bytes.Contains(bytes.Join(first, nil), []byte("normal-transport-socket-bound-one")) {
		t.Fatalf("first socket-bound completion = %q", first)
	}

	secondFrame := normalTransportGateWSFrame(
		"normal-transport-turn-socket-bound-two",
		`,"previous_response_id":"normal-transport-socket-bound-one"`,
	)
	if err := connection.WriteMessage(websocket.TextMessage, secondFrame); err != nil {
		t.Fatal(err)
	}
	second := normalTransportGateReadWSCompletion(t, connection)
	if !bytes.Contains(bytes.Join(second, nil), []byte("normal-transport-socket-bound-two")) {
		t.Fatalf("socket-bound successor completion = %q", second)
	}

	receipts := harness.backend.snapshot()
	handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
	frames := normalTransportGateReceipts(receipts, "websocket_frame")
	if len(handshakes) != 1 || len(frames) != 2 || frames[0].connection != frames[1].connection || frames[1].payload != string(secondFrame) {
		t.Fatalf("socket-bound continuation receipts = handshakes %#v frames %#v", handshakes, frames)
	}
	harness.backend.assertNoFailure(t)
}

func TestNormalProxyTransportWebSocketClosedAnchorRequestsFullCreateRetry(t *testing.T) {
	harness := newNormalTransportGateHarness(t, normalTransportGateWSResponseAnchorSocketClosed)
	connection := normalTransportGateWebSocket(t, harness)
	if err := connection.WriteMessage(websocket.TextMessage, normalTransportGateWSFrame("normal-transport-turn-socket-closed", "")); err != nil {
		t.Fatal(err)
	}
	first := normalTransportGateReadWSCompletion(t, connection)
	if !bytes.Contains(bytes.Join(first, nil), []byte("normal-transport-socket-closed-one")) {
		t.Fatalf("first socket-closed completion = %q", first)
	}
	select {
	case <-harness.backend.firstWSClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not close completed socket-bound connection")
	}

	incremental := normalTransportGateWSFrame(
		"normal-transport-turn-socket-closed",
		`,"previous_response_id":"normal-transport-socket-closed-one"`,
	)
	if err := connection.WriteMessage(websocket.TextMessage, incremental); err != nil {
		t.Fatal(err)
	}
	normalTransportGateReadWSAccountUnavailable(t, connection)
	_ = connection.Close()

	retry := normalTransportGateWebSocket(t, harness)
	defer retry.Close()
	fullCreate := normalTransportGateWSFrame("normal-transport-turn-socket-closed", "")
	if err := retry.WriteMessage(websocket.TextMessage, fullCreate); err != nil {
		t.Fatal(err)
	}
	second := normalTransportGateReadWSCompletion(t, retry)
	if !bytes.Contains(bytes.Join(second, nil), []byte("normal-transport-socket-closed-two")) {
		t.Fatalf("socket-closed full-create completion = %q", second)
	}

	receipts := harness.backend.snapshot()
	handshakes := normalTransportGateReceipts(receipts, "websocket_handshake")
	frames := normalTransportGateReceipts(receipts, "websocket_frame")
	if len(handshakes) != 2 || len(frames) != 2 || frames[0].connection == frames[1].connection || frames[1].payload != string(fullCreate) {
		t.Fatalf("socket-closed retry receipts = handshakes %#v frames %#v", handshakes, frames)
	}
	harness.backend.assertNoFailure(t)
}

func normalTransportGateHTTPBody(t *testing.T, metadata CodexTurnMetadata) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "gpt-5.6-sol",
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "transport gate"}},
		}},
		"stream": true,
		"client_metadata": map[string]any{
			codexTurnMetadataKey: metadata,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func normalTransportGateHTTPContinuationBody(t *testing.T, metadata CodexTurnMetadata, previousResponseID string) []byte {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal(normalTransportGateHTTPBody(t, metadata), &request); err != nil {
		t.Fatal(err)
	}
	request["previous_response_id"] = previousResponseID
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func normalTransportGateHTTPCall(t *testing.T, harness *normalTransportGateHarness, body []byte) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, harness.proxy.URL+legacyCodexResponsesPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+harness.callerToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, codexProtocolMaxBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || len(responseBody) > codexProtocolMaxBytes {
		t.Fatalf("read normal transport response: read=%v close=%v bytes=%d", readErr, closeErr, len(responseBody))
	}
	return response.StatusCode, responseBody
}

func normalTransportGateWebSocket(t *testing.T, harness *normalTransportGateHarness) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+harness.callerToken)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Subprotocols: []string{"responses"}, Proxy: nil}
	connection, response, err := dialer.DialContext(ctx, "ws"+strings.TrimPrefix(harness.proxy.URL, "http")+legacyCodexResponsesPath, header)
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			t.Fatalf("downstream WebSocket dial = %v status=%d body=%q", err, response.StatusCode, body)
		}
		t.Fatal(err)
	}
	if response == nil || response.StatusCode != http.StatusSwitchingProtocols {
		_ = connection.Close()
		t.Fatalf("downstream WebSocket response = %#v", response)
	}
	return connection
}

func normalTransportGateReadWSCompletion(t *testing.T, connection *websocket.Conn) [][]byte {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	replies := make([][]byte, 0, 2)
	for range 2 {
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read downstream WebSocket completion: %v", err)
		}
		if messageType != websocket.TextMessage {
			t.Fatalf("downstream WebSocket message type = %d", messageType)
		}
		replies = append(replies, append([]byte(nil), payload...))
	}
	if !bytes.Contains(replies[0], []byte(`"type":"response.created"`)) || !bytes.Contains(replies[1], []byte(`"type":"response.completed"`)) {
		t.Fatalf("downstream WebSocket completion = %q", replies)
	}
	return replies
}

func normalTransportGateReadWSAccountUnavailable(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	if err == nil {
		t.Fatalf("non-portable hard limit leaked downstream frame type=%d payload=%q", messageType, payload)
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart || closeErr.Text != "account unavailable" {
		t.Fatalf("non-portable hard-limit close = %T %v, want 1012 account unavailable", err, err)
	}
}

func normalTransportGateReadWSAdmissionThenProviderFailure(t *testing.T, connection *websocket.Conn, status int) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	if err != nil || messageType != websocket.TextMessage || !bytes.Contains(payload, []byte(`"type":"response.created"`)) {
		t.Fatalf("admitted response = type %d payload %q err %v", messageType, payload, err)
	}
	messageType, payload, err = connection.ReadMessage()
	if err != nil || messageType != websocket.TextMessage {
		t.Fatalf("admitted provider failure = type %d payload %q err %v", messageType, payload, err)
	}
	errorType := "authentication_error"
	if status == http.StatusTooManyRequests {
		errorType = "usage_limit_reached"
	}
	want := []byte(fmt.Sprintf(`{"type":"error","status":%d,"error":{"type":%q}}`, status, errorType))
	normalTransportGateRequireSemanticJSON(t, payload, want, "admitted provider failure")
}

func normalTransportGateReadWSFinalAuthFailure(t *testing.T, connection *websocket.Conn, status int, admitted bool) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if admitted {
		messageType, payload, err := connection.ReadMessage()
		if err != nil || messageType != websocket.TextMessage || !bytes.Contains(payload, []byte(`"type":"response.created"`)) {
			t.Fatalf("final auth admission = type %d payload %q err %v", messageType, payload, err)
		}
	}
	messageType, payload, err := connection.ReadMessage()
	want := []byte(fmt.Sprintf(`{"type":"error","status":%d,"error":{"type":"authentication_error"}}`, status))
	if err != nil || messageType != websocket.TextMessage {
		t.Fatalf("final auth failure = type %d payload %q err %v, want exact %q", messageType, payload, err, want)
	}
	normalTransportGateRequireSemanticJSON(t, payload, want, "final auth failure")
}

func normalTransportGateReadWSFinalProviderFailure(t *testing.T, connection *websocket.Conn, status int) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	errorType := "authentication_error"
	if status == http.StatusTooManyRequests {
		errorType = "usage_limit_reached"
	}
	want := []byte(fmt.Sprintf(`{"type":"error","status":%d,"error":{"type":%q}}`, status, errorType))
	if err != nil || messageType != websocket.TextMessage {
		t.Fatalf("final provider failure = type %d payload %q err %v, want exact %q", messageType, payload, err, want)
	}
	normalTransportGateRequireSemanticJSON(t, payload, want, "final provider failure")
}

func normalTransportGateRequireSemanticJSON(t *testing.T, got, want []byte, label string) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s payload %q is not JSON: %v", label, got, err)
	}
	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("%s fixture %q is not JSON: %v", label, want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s = %q, want semantic JSON %q", label, got, want)
	}
}

func normalTransportGateAdmittedFailureStatus(scenario normalTransportGateScenario) int {
	switch scenario {
	case normalTransportGateWSAdmittedHardLimit:
		return http.StatusTooManyRequests
	case normalTransportGateWSAdmittedUnauthorized:
		return http.StatusUnauthorized
	case normalTransportGateWSAdmittedForbidden:
		return http.StatusForbidden
	default:
		return 0
	}
}

func normalTransportGateWSFrame(turnID, extra string) []byte {
	return normalTransportGateWSFrameForLane("normal-transport-session-ws", "normal-transport-thread-ws", turnID, extra)
}

func normalTransportGateWSFrameForLane(sessionID, threadID, turnID, extra string) []byte {
	return []byte(`{"type":"response.create","model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"session_id":"` + sessionID + `","thread_id":"` + threadID + `","turn_id":"` + turnID + `","request_kind":"turn"}},"input":[]` + extra + `}`)
}

func normalTransportGateReceipts(receipts []normalTransportGateReceipt, transport string) []normalTransportGateReceipt {
	result := make([]normalTransportGateReceipt, 0, len(receipts))
	for _, receipt := range receipts {
		if receipt.transport == transport {
			result = append(result, receipt)
		}
	}
	return result
}
