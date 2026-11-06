package dots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// cloudHarnessGateway belongs to one native app-server/stdio bridge. Relay
// reconnects replace writers, while native client_closed removes subscriptions.
type cloudHarnessGateway struct {
	h              *cloudHarness
	origin         string
	mu             sync.Mutex
	closed         bool
	streams        map[string]*cloudHarnessGatewayStream
	sessions       map[*cloudHarnessGatewaySession]bool
	outputs        map[string]uint64
	generations    map[string]uint64
	nextGeneration uint64
	preparedCount  int
}

type cloudHarnessGatewayStream struct {
	input              sync.Mutex
	clientID, streamID string
	writer             *cloudHarnessGatewaySession
	resumes            map[string]cloudMessage
	inputSequence      uint64
	prepared           map[uint64]cloudHarnessGatewayPrepared
}

type cloudHarnessGatewayPrepared struct {
	id  string
	rpc []byte
}

// Match the native transport's bounded channel capacity. Prepared RPCs remain
// bounded to one direct envelope each and live only until their native reply.
const cloudHarnessGatewayPreparedLimit = 128

type cloudHarnessGatewaySession struct {
	native, backend           *websocket.Conn
	nativeWrite, backendWrite sync.Mutex
	generation                uint64
}

func newCloudHarnessGateway(h *cloudHarness) *cloudHarnessGateway {
	return &cloudHarnessGateway{h: h, origin: "https://chatgpt.com", streams: make(map[string]*cloudHarnessGatewayStream), sessions: make(map[*cloudHarnessGatewaySession]bool), outputs: make(map[string]uint64), generations: make(map[string]uint64)}
}

func (g *cloudHarnessGateway) Serve(listener net.Listener) error {
	server := &http.Server{Handler: http.HandlerFunc(g.serveHTTP), ReadHeaderTimeout: 25 * time.Second, MaxHeaderBytes: 65536}
	done := make(chan struct{})
	ctx := context.Background()
	if g.h != nil && g.h.b != nil && g.h.b.ctx != nil {
		ctx = g.h.b.ctx
	}
	go func() {
		defer func() { _ = recover() }()
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	err := server.Serve(listener)
	close(done)
	g.mu.Lock()
	g.closed = true
	for session := range g.sessions {
		_ = session.native.Close()
		_ = session.backend.Close()
	}
	g.mu.Unlock()
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return errors.New("native harness gateway server unavailable")
}

func cloudHarnessGatewayHeaders(headers http.Header) http.Header {
	result := headers.Clone()
	for _, name := range []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Accept", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Length"} {
		result.Del(name)
	}
	return result
}

func (g *cloudHarnessGateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.RequestURI() != "/backend-api/wham/remote/control/server" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "native gateway request invalid", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.nextGeneration++
	generation := g.nextGeneration
	g.mu.Unlock()
	origin, err := url.Parse(g.origin)
	if err != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") || origin.User != nil {
		http.Error(w, "native gateway upstream unavailable", http.StatusBadGateway)
		return
	}
	upstreamURL := *origin
	upstreamURL.Path, upstreamURL.RawPath, upstreamURL.RawQuery = r.URL.Path, r.URL.RawPath, ""
	dialer := websocket.Dialer{NetDialContext: dotsDialContext, HandshakeTimeout: 25 * time.Second, EnableCompression: true, Subprotocols: websocket.Subprotocols(r)}
	if origin.Scheme == "https" {
		dialer.TLSClientConfig, err = cloudPublicTLS()
		if err != nil {
			http.Error(w, "native gateway upstream unavailable", http.StatusBadGateway)
			return
		}
		upstreamURL.Scheme = "wss"
	} else {
		upstreamURL.Scheme = "ws"
	}
	backend, response, err := dialer.DialContext(r.Context(), upstreamURL.String(), cloudHarnessGatewayHeaders(r.Header))
	if err != nil {
		if response == nil {
			http.Error(w, "native gateway upstream unavailable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range cloudHarnessGatewayHeaders(response.Header) {
			w.Header()[key] = values
		}
		body, readErr := httputil.ReadBody(response.Body)
		if readErr != nil {
			http.Error(w, "native gateway upstream response unavailable", http.StatusBadGateway)
			return
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
		return
	}
	defer backend.Close()
	upgrader := websocket.Upgrader{HandshakeTimeout: 25 * time.Second, EnableCompression: true, CheckOrigin: func(*http.Request) bool { return true }}
	if protocol := backend.Subprotocol(); protocol != "" {
		upgrader.Subprotocols = []string{protocol}
	}
	native, err := upgrader.Upgrade(w, r, cloudHarnessGatewayHeaders(response.Header))
	if err != nil {
		return
	}
	defer native.Close()
	session := &cloudHarnessGatewaySession{native: native, backend: backend, generation: generation}
	// The native protocol bounds assembled messages at 100 MiB. Unsupported
	// frames below that transport bound remain opaque, even above codec bounds.
	native.SetReadLimit(100 * 1024 * 1024)
	backend.SetReadLimit(100 * 1024 * 1024)
	for _, direction := range []bool{true, false} {
		from := backend
		if !direction {
			from = native
		}
		toNative := direction
		from.SetPingHandler(func(data string) error { return session.control(toNative, websocket.PingMessage, []byte(data)) })
		from.SetPongHandler(func(data string) error { return session.control(toNative, websocket.PongMessage, []byte(data)) })
		from.SetCloseHandler(func(code int, text string) error {
			_ = session.control(toNative, websocket.CloseMessage, websocket.FormatCloseMessage(code, text))
			return &websocket.CloseError{Code: code, Text: text}
		})
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.sessions[session] = true
	g.mu.Unlock()
	defer g.disconnected(session)
	done := make(chan struct{}, 2)
	for _, direction := range []bool{true, false} {
		go func(toNative bool) {
			defer func() { _ = recover(); done <- struct{}{} }()
			g.relay(session, toNative)
		}(direction)
	}
	<-done
	_ = native.Close()
	_ = backend.Close()
	<-done
}

func (s *cloudHarnessGatewaySession) write(toNative bool, kind int, raw []byte) error {
	connection, lock := s.backend, &s.backendWrite
	if toNative {
		connection, lock = s.native, &s.nativeWrite
	}
	lock.Lock()
	defer lock.Unlock()
	_ = connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer connection.SetWriteDeadline(time.Time{})
	return connection.WriteMessage(kind, raw)
}

func (s *cloudHarnessGatewaySession) control(toNative bool, kind int, raw []byte) error {
	connection, lock := s.backend, &s.backendWrite
	if toNative {
		connection, lock = s.native, &s.nativeWrite
	}
	lock.Lock()
	defer lock.Unlock()
	return connection.WriteControl(kind, raw, time.Now().Add(5*time.Second))
}

func (g *cloudHarnessGateway) relay(session *cloudHarnessGatewaySession, toNative bool) {
	from := session.native
	if toNative {
		from = session.backend
	}
	for {
		kind, raw, err := from.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.TextMessage {
			raw, err = g.message(session, toNative, raw)
			if err != nil {
				_ = session.control(false, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "native gateway preparation unavailable"))
				return
			}
		}
		if session.write(toNative, kind, raw) != nil {
			return
		}
	}
}

func cloudHarnessGatewaySource(clientID, streamID string) string {
	digest := sha256.Sum256([]byte(clientID + "\x00" + streamID))
	return "gateway/" + hex.EncodeToString(digest[:])
}

func (g *cloudHarnessGateway) bind(session *cloudHarnessGatewaySession, clientID, streamID string, create bool) string {
	source := cloudHarnessGatewaySource(clientID, streamID)
	g.mu.Lock()
	g.sessionGenerationLocked(session)
	stream := g.streams[source]
	known := stream != nil
	if session.generation < g.generations[source] {
		g.mu.Unlock()
		return source
	}
	if stream == nil && create {
		stream = &cloudHarnessGatewayStream{clientID: clientID, streamID: streamID, resumes: make(map[string]cloudMessage), prepared: make(map[uint64]cloudHarnessGatewayPrepared)}
		g.streams[source] = stream
	}
	recovered := stream != nil && known && stream.writer != session
	if stream != nil {
		stream.writer = session
		g.generations[source] = session.generation
	}
	g.mu.Unlock()
	if recovered {
		g.h.gatewayRecovered(source)
	}
	return source
}

func (g *cloudHarnessGateway) sessionGenerationLocked(session *cloudHarnessGatewaySession) {
	if session.generation == 0 {
		g.nextGeneration++
		session.generation = g.nextGeneration
	}
}

func (g *cloudHarnessGateway) currentSession(session *cloudHarnessGatewaySession, source string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessionGenerationLocked(session)
	return session.generation >= g.generations[source]
}

func (g *cloudHarnessGateway) message(session *cloudHarnessGatewaySession, toNative bool, raw []byte) ([]byte, error) {
	envelope, ok := decodeCloudGatewayEnvelope(raw)
	if !ok {
		if toNative {
			fields, valid := cloudGatewayObject(raw)
			kind, clientID, streamID := cloudHarnessString(fields, "type"), cloudHarnessString(fields, "client_id"), cloudHarnessString(fields, "stream_id")
			if sequence, present := fields["seq_id"]; present {
				var value uint64
				valid = valid && json.Unmarshal(sequence, &value) == nil && value > 0
			}
			if cursor, present := fields["cursor"]; present {
				var value *string
				valid = valid && json.Unmarshal(cursor, &value) == nil
			}
			if valid && cloudGatewayIdentifier(clientID) && cloudGatewayIdentifier(streamID) {
				if kind == "ping" {
					g.bind(session, clientID, streamID, false)
				} else if kind == "client_closed" && g.currentSession(session, cloudHarnessGatewaySource(clientID, streamID)) {
					g.closeSource(session, cloudHarnessGatewaySource(clientID, streamID))
				}
			}
		}
		return raw, nil
	}
	if (toNative && envelope.Kind != "client_message") || (!toNative && envelope.Kind != "server_message") {
		return raw, nil
	}
	identity := cloudHarnessGatewaySource(envelope.ClientID, envelope.StreamID)
	if toNative && !g.currentSession(session, identity) {
		return raw, nil
	}
	existing := false
	if toNative {
		g.mu.Lock()
		existing = g.streams[identity] != nil
		g.mu.Unlock()
		g.bind(session, envelope.ClientID, envelope.StreamID, true)
		g.mu.Lock()
		stream := g.streams[identity]
		g.mu.Unlock()
		if stream == nil {
			return raw, nil
		}
		// A reconnect may replay while the first preparation still waits on a
		// private native RPC. Publish its transformed bytes before retrying it.
		stream.input.Lock()
		defer stream.input.Unlock()
		if !g.currentSession(session, identity) {
			return raw, nil
		}
	}
	if toNative && envelope.Method == "initialize" && len(envelope.ID) != 0 {
		// Native replaces this logical connection on repeated initialise, even
		// without a client_closed event. A network reconnect alone does not.
		source := cloudHarnessGatewaySource(envelope.ClientID, envelope.StreamID)
		if existing {
			g.detach(source)
		}
	}
	source := g.bind(session, envelope.ClientID, envelope.StreamID, toNative)
	rpc := envelope.Fields["message"]
	if !toNative {
		g.mu.Lock()
		first := envelope.Sequence > g.outputs[source]
		if first {
			g.outputs[source] = envelope.Sequence
		}
		g.mu.Unlock()
		if g.h != nil && envelope.Method == "" {
			id := cloudHarnessString(envelope.Message, "id")
			suffix := strings.TrimPrefix(id, g.h.prefix)
			sequence, err := strconv.ParseUint(suffix, 10, 64)
			g.h.mu.Lock()
			issued := strings.HasPrefix(id, g.h.prefix) && err == nil && sequence > 0 && sequence <= g.h.sequence && strconv.FormatUint(sequence, 10) == suffix
			g.h.mu.Unlock()
			if issued {
				if replacement, _, owned := cloudGatewayPrivateReply(raw, g.h.prefix); owned {
					g.h.observeFrom(source, rpc)
					return replacement, nil
				}
			}
		}
		if !first {
			return raw, nil
		}
		if envelope.Method == "" {
			g.completePrepared(source, envelope.ID)
		}
		g.h.observeFrom(source, rpc)
		return raw, nil
	}
	reserved, retained := false, false
	defer func() {
		if reserved && !retained {
			g.mu.Lock()
			g.preparedCount--
			g.mu.Unlock()
		}
	}()
	if envelope.Sequence != 0 && envelope.Method != "initialize" {
		g.mu.Lock()
		stream := g.streams[source]
		if stream != nil && envelope.Sequence <= stream.inputSequence {
			prepared := bytes.Clone(stream.prepared[envelope.Sequence].rpc)
			g.mu.Unlock()
			return cloudHarnessGatewayPreparedEnvelope(raw, envelope.Fields, prepared)
		}
		if g.preparedCount >= cloudHarnessGatewayPreparedLimit {
			g.mu.Unlock()
			return nil, errors.New("native gateway prepared request capacity unavailable")
		}
		g.preparedCount++
		reserved = true
		g.mu.Unlock()
	}
	if envelope.Method == "thread/resume" && envelope.ThreadID != "" && len(envelope.ID) != 0 {
		added := false
		g.mu.Lock()
		if stream := g.streams[source]; stream != nil {
			added = stream.resumes[envelope.ThreadID] == nil
			stream.resumes[envelope.ThreadID] = cloudHarnessClone(cloudHarnessObject(envelope.Message["params"]))
		}
		g.mu.Unlock()
		if added {
			g.h.gatewayCandidateChanged(envelope.ThreadID)
		}
	}
	prepared, err := g.h.prepareFrom(source, envelope.Message, rpc)
	if err != nil {
		return nil, errors.New("native gateway preparation unavailable")
	}
	changed := !bytes.Equal(prepared, rpc)
	if !changed {
		prepared = nil
	}
	updated, err := cloudHarnessGatewayPreparedEnvelope(raw, envelope.Fields, prepared)
	if err != nil {
		g.cancelPending(source, envelope.ID)
		return nil, err
	}
	if envelope.Sequence != 0 {
		g.mu.Lock()
		if stream := g.streams[source]; stream != nil {
			stream.inputSequence = envelope.Sequence
			if changed {
				stream.prepared[envelope.Sequence] = cloudHarnessGatewayPrepared{id: cloudHarnessID(envelope.ID), rpc: bytes.Clone(prepared)}
				retained = true
			}
		}
		g.mu.Unlock()
	}
	return updated, nil
}

func cloudHarnessGatewayPreparedEnvelope(original []byte, fields cloudMessage, prepared []byte) ([]byte, error) {
	if prepared == nil {
		return original, nil
	}
	fields["message"] = bytes.TrimSpace(prepared)
	updated, err := json.Marshal(fields)
	if err != nil {
		return nil, errors.New("native gateway prepared request unavailable")
	}
	if len(updated) > cloudGatewayMaxEnvelopeBytes {
		return nil, errors.New("native gateway prepared request exceeds direct envelope bound")
	}
	return updated, nil
}

func (g *cloudHarnessGateway) completePrepared(source string, id json.RawMessage) {
	if id == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if stream := g.streams[source]; stream != nil {
		key := cloudHarnessID(id)
		for sequence, prepared := range stream.prepared {
			if prepared.id == key {
				delete(stream.prepared, sequence)
				g.preparedCount--
			}
		}
	}
}

func (g *cloudHarnessGateway) cancelPending(source string, id json.RawMessage) {
	if g.h == nil || id == nil {
		return
	}
	g.h.mu.Lock()
	key := cloudHarnessRequestKey(source, id)
	if pending, found := g.h.requests[key]; found {
		cloudHarnessGatewayPendingCounters(g.h, pending)
		delete(g.h.requests, key)
	}
	g.h.mu.Unlock()
	g.h.signal()
}

func cloudHarnessGatewayPendingCounters(h *cloudHarness, pending cloudHarnessRequest) {
	if thread := h.threads[cloudHarnessString(pending.params, "threadId")]; thread != nil {
		if pending.method == "turn/start" && thread.pendingTurns > 0 {
			thread.pendingTurns--
		}
		if pending.method == "thread/settings/update" && thread.pendingSettings > 0 {
			thread.pendingSettings--
		}
	}
}

func (g *cloudHarnessGateway) closeSource(session *cloudHarnessGatewaySession, source string) {
	g.mu.Lock()
	stream := g.streams[source]
	g.mu.Unlock()
	if stream != nil {
		stream.input.Lock()
		defer stream.input.Unlock()
	}
	if g.currentSession(session, source) {
		g.mu.Lock()
		g.generations[source] = session.generation
		g.mu.Unlock()
		g.detach(source)
	}
}

func (g *cloudHarnessGateway) disconnected(session *cloudHarnessGatewaySession) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.sessions, session)
	for _, stream := range g.streams {
		if stream.writer == session {
			stream.writer = nil
		}
	}
}

func (g *cloudHarnessGateway) detach(source string) {
	g.mu.Lock()
	if stream := g.streams[source]; stream != nil {
		g.preparedCount -= len(stream.prepared)
	}
	delete(g.streams, source)
	g.mu.Unlock()
	if g.h == nil {
		return
	}
	g.h.mu.Lock()
	for _, thread := range g.h.threads {
		cloudHarnessSubscription(thread, source, false)
	}
	for key, pending := range g.h.requests {
		if strings.HasPrefix(key, source+"\x00") {
			cloudHarnessGatewayPendingCounters(g.h, pending)
			delete(g.h.requests, key)
		}
	}
	g.h.mu.Unlock()
	g.h.signal()
}

func (g *cloudHarnessGateway) ResumeParams(source, threadID string) cloudMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	if stream := g.streams[source]; stream != nil && stream.resumes[threadID] != nil {
		return cloudHarnessClone(stream.resumes[threadID])
	}
	return nil
}

// ResumeSources reports request-only candidates, including disconnected logical
// clients. A private unsubscribe result must establish their actual ownership.
func (g *cloudHarnessGateway) ResumeSources(threadID string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var sources []string
	for source, stream := range g.streams {
		if stream.resumes[threadID] != nil {
			sources = append(sources, source)
		}
	}
	slices.Sort(sources)
	return sources
}

func (g *cloudHarnessGateway) Write(source string, rawJSONRPC []byte) error {
	request, ok := cloudGatewayObject(rawJSONRPC)
	method := request.method()
	params, paramsOK := cloudGatewayObject(request["params"])
	id := cloudHarnessString(request, "id")
	if !ok || !paramsOK || g.h == nil || !cloudGatewayPrivatePrefix(g.h.prefix) || !cloudGatewayIdentifier(id) || !strings.HasPrefix(id, g.h.prefix) || len(id) == len(g.h.prefix) || (method != "thread/unsubscribe" && method != "thread/resume") || !cloudGatewayIdentifier(cloudHarnessString(params, "threadId")) || request["result"] != nil || request["error"] != nil {
		return errors.New("native gateway private request invalid")
	}
	if method == "thread/resume" {
		params["excludeTurns"] = json.RawMessage("true")
	}
	request["params"] = cloudHarnessRaw(params)
	g.mu.Lock()
	stream := g.streams[source]
	if stream == nil || stream.writer == nil {
		g.mu.Unlock()
		return errors.New("native gateway source unavailable")
	}
	writer, clientID, streamID := stream.writer, stream.clientID, stream.streamID
	g.mu.Unlock()
	raw := cloudHarnessRaw(map[string]any{"type": "client_message", "client_id": clientID, "stream_id": streamID, "message": request})
	if len(raw) > cloudGatewayMaxEnvelopeBytes || writer.write(true, websocket.TextMessage, raw) != nil {
		return errors.New("native gateway private request unavailable")
	}
	return nil
}
