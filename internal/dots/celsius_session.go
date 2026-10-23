package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type celsiusPendingKey struct {
	alias string
	id    int64
}
type celsiusDesiredKey struct{ kind, topic string }
type celsiusWire struct {
	id      int64
	command map[string]any
}
type celsiusAwaiting struct {
	remaining  map[string]bool
	replies    map[string]any
	command    map[string]any
	ownerCount int
	deadline   time.Time
}
type celsiusIncoming struct {
	kind, alias string
	socket      CelsiusSocket
	data        []byte
	err         error
	stopClose   func() bool
}

func celsiusDecode(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("Invalid Celsius JSON frame")
	}
	return value, nil
}

func celsiusSend(socket CelsiusSocket, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return socket.WriteMessage(websocket.TextMessage, data)
}
func celsiusSendWire(socket CelsiusSocket, wire celsiusWire) error {
	return celsiusSend(socket, []any{map[string]any{"id": wire.id, "command": wire.command}})
}

func (b *CelsiusBroker) handle(parent context.Context, client CelsiusSocket, grant celsiusGrant) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	primary := b.Settings.SystemAlias
	native := b.Settings.IntegrationMode == "native"
	sockets := map[string]CelsiusSocket{}
	ready, unavailable, connecting := map[string]bool{}, map[string]bool{}, map[string]bool{}
	desired := map[string]map[celsiusDesiredKey]celsiusWire{}
	retries := map[string]time.Time{}
	retryDelay := map[string]time.Duration{}
	for _, alias := range celsiusAliases(primary, grant.owners) {
		if alias != primary {
			desired[alias] = map[celsiusDesiredKey]celsiusWire{}
			retryDelay[alias] = b.retryInitial
		}
	}
	pending := map[celsiusPendingKey]int64{}
	awaiting := map[int64]*celsiusAwaiting{}
	incoming := make(chan celsiusIncoming, 512)
	var nextID int64 = 1
	defer func() {
		if recover() != nil {
			b.event(map[string]any{"celsius_broker_error_type": "panic"})
		}
		cancel()
		for _, socket := range sockets {
			socket.Close()
		}
		client.Close()
	}()
	push := func(value celsiusIncoming) bool {
		select {
		case incoming <- value:
			return true
		case <-ctx.Done():
			return false
		}
	}
	receive := func(alias string, socket CelsiusSocket) {
		socketCtx, socketCancel := context.WithCancel(ctx)
		var keepaliveFailed atomic.Bool
		stop := context.AfterFunc(socketCtx, func() { defer func() { recover() }(); socket.Close() })
		if concrete, ok := socket.(*websocket.Conn); ok {
			socketKeepalive(socketCtx, concrete, func() { keepaliveFailed.Store(true); socket.Close() })
		}
		go func() {
			started := b.now()
			var receiveErr error
			defer func() {
				if recover() != nil {
					receiveErr = errors.New("Celsius receiver panic")
				}
				socketCancel()
				stop()
				socket.Close()
				code := 0
				var closeErr *websocket.CloseError
				if errors.As(receiveErr, &closeErr) {
					code = closeErr.Code
				}
				side := "upstream"
				ownerAlias := alias
				if alias == "client" {
					side = "downstream"
					ownerAlias = primary
				}
				reason := celsiusErrorType(receiveErr)
				if keepaliveFailed.Load() {
					reason = "keepalive_failure"
				}
				if ctx.Err() != nil {
					reason = "local_disposal"
				}
				b.event(map[string]any{"celsius_connection_closed": true, "owner_alias": ownerAlias, "side": side,
					"close_code": code, "reason_type": reason, "lifetime_ms": b.now().Sub(started).Milliseconds()})
				push(celsiusIncoming{kind: "closed", alias: alias, socket: socket, err: receiveErr})
			}()
			for {
				_, data, err := socket.ReadMessage()
				if err != nil {
					receiveErr = err
					return
				}
				if !push(celsiusIncoming{kind: "data", alias: alias, socket: socket, data: data}) {
					return
				}
			}
		}()
	}
	connectSecondary := func(alias string) {
		connecting[alias] = true
		go func() {
			var socket CelsiusSocket
			defer func() {
				if recover() != nil {
					if socket != nil {
						socket.Close()
					}
					push(celsiusIncoming{kind: "connect_error", alias: alias, err: errors.New("Celsius connector panic")})
				}
			}()
			endpoint := grant.urls[alias]
			var err error
			if grant.resolve != nil {
				endpoint, err = grant.resolve(ctx, alias)
			}
			if err == nil {
				err = celsiusValidateURL(endpoint)
			}
			if err == nil {
				socket, err = b.Dial(ctx, endpoint)
			}
			if err != nil {
				if socket != nil {
					socket.Close()
				}
				push(celsiusIncoming{kind: "connect_error", alias: alias, err: err})
				return
			}
			stopClose := context.AfterFunc(ctx, func() { defer func() { recover() }(); socket.Close() })
			if !push(celsiusIncoming{kind: "connected", alias: alias, socket: socket, stopClose: stopClose}) {
				stopClose()
				socket.Close()
			}
		}()
	}
	complete := func(nativeID int64, reply any) map[string]any {
		state := awaiting[nativeID]
		delete(awaiting, nativeID)
		for key, value := range pending {
			if value == nativeID {
				delete(pending, key)
			}
		}
		if object, ok := reply.(map[string]any); ok && state.command["type"] == "subscribe" && (len(state.replies) > 1 || (native && state.ownerCount > 1)) {
			object = celsiusClone(object)
			object["recovered"] = false
			object["catchups"] = []any{}
			reply = object
		}
		var replyType any
		if object, ok := reply.(map[string]any); ok {
			replyType = object["type"]
		}
		b.event(map[string]any{"celsius_command_ack": state.command["type"], "owner_count": len(state.replies), "reply_type": replyType})
		return map[string]any{"id": nativeID, "reply": reply}
	}
	retire := func(alias, reason string) error {
		unavailable[alias] = true
		delete(ready, alias)
		if socket := sockets[alias]; socket != nil {
			delete(sockets, alias)
			socket.Close()
		}
		var forwarded []any
		for id, state := range awaiting {
			if state.remaining[alias] {
				forwarded = append(forwarded, complete(id, map[string]any{"type": "error", "code": "cq_owner_unavailable"}))
			}
		}
		for key := range pending {
			if key.alias == alias {
				delete(pending, key)
			}
		}
		if native && !connecting[alias] {
			if _, exists := retries[alias]; !exists {
				retries[alias] = b.now().Add(retryDelay[alias])
				retryDelay[alias] = min(b.retryMaximum, retryDelay[alias]*2)
			}
		}
		b.event(map[string]any{"celsius_owner_unavailable": alias, "reason_type": reason})
		if len(forwarded) > 0 {
			return celsiusSend(client, forwarded)
		}
		return nil
	}
	for _, alias := range celsiusAliases(primary, grant.owners) {
		if native && alias != primary {
			connectSecondary(alias)
			continue
		}
		socket, err := b.Dial(ctx, grant.urls[alias])
		if err != nil {
			b.event(map[string]any{"celsius_broker_error_type": celsiusErrorType(err)})
			return
		}
		sockets[alias] = socket
		ready[alias] = true
		b.event(map[string]any{"celsius_owner_connected": alias, "upgrade_status": 101})
		receive(alias, socket)
	}
	receive("client", client)
	ticker := time.NewTicker(b.tickInterval)
	defer ticker.Stop()
	for {
		if native {
			for alias, deadline := range retries {
				if !deadline.After(b.now()) && !connecting[alias] {
					delete(retries, alias)
					connectSecondary(alias)
				}
			}
			for _, state := range awaiting {
				if !state.deadline.IsZero() && !state.deadline.After(b.now()) {
					for alias := range state.remaining {
						if err := retire(alias, "ack_timeout"); err != nil {
							return
						}
					}
				}
			}
		}
		var message celsiusIncoming
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			continue
		case message = <-incoming:
		}
		source := message.alias
		switch message.kind {
		case "connected":
			delete(connecting, source)
			delete(unavailable, source)
			delete(retries, source)
			retryDelay[source] = b.retryInitial
			sockets[source] = message.socket
			ready[source] = true
			b.event(map[string]any{"celsius_owner_connected": source, "upgrade_status": 101})
			receive(source, message.socket)
			message.stopClose()
			replay := map[int64]celsiusWire{}
			for _, wire := range desired[source] {
				replay[wire.id] = wire
			}
			for key, nativeID := range pending {
				if key.alias == source {
					command := celsiusClone(awaiting[nativeID].command)
					delete(command, "offset")
					replay[key.id] = celsiusWire{key.id, command}
				}
			}
			identifiers := make([]int64, 0, len(replay))
			for id := range replay {
				identifiers = append(identifiers, id)
			}
			slices.Sort(identifiers)
			for _, id := range identifiers {
				if err := celsiusSendWire(message.socket, replay[id]); err != nil {
					if retire(source, "send_failure") != nil {
						return
					}
					break
				}
				if nativeID, ok := pending[celsiusPendingKey{source, id}]; ok {
					if state := awaiting[nativeID]; state != nil && state.remaining[source] {
						state.deadline = b.now().Add(b.ackTimeout)
					}
				}
			}
			continue
		case "connect_error":
			delete(connecting, source)
			b.event(map[string]any{"celsius_receive_error_type": celsiusErrorType(message.err), "source": source})
			if !native || source == primary {
				return
			}
			if retire(source, "connect_failure") != nil {
				return
			}
			continue
		}
		if source != "client" && sockets[source] != message.socket {
			continue
		}
		if message.kind == "closed" {
			if native && source != "client" && source != primary {
				if retire(source, "upstream_closed") != nil {
					return
				}
				continue
			}
			return
		}
		value, err := celsiusDecode(message.data)
		if err != nil {
			if native && source != "client" && source != primary {
				if retire(source, "invalid_frame") != nil {
					return
				}
				continue
			}
			b.event(map[string]any{"celsius_broker_error_type": "invalid_frame"})
			return
		}
		if source == "client" {
			batch, ok := value.([]any)
			if !ok || len(batch) > 128 {
				return
			}
			for _, item := range batch {
				object, ok := item.(map[string]any)
				if !ok {
					return
				}
				command, ok := object["command"].(map[string]any)
				if !ok {
					return
				}
				nativeID, ok := celsiusID(object["id"])
				if !ok || awaiting[nativeID] != nil {
					return
				}
				destinations, err := celsiusTargets(command, primary, grant.owners)
				if err != nil {
					return
				}
				required := map[string]bool{}
				for _, alias := range destinations {
					required[alias] = true
				}
				if native && required[primary] {
					required = map[string]bool{primary: true}
				}
				state := &celsiusAwaiting{remaining: required, replies: map[string]any{}, command: command, ownerCount: len(destinations)}
				awaiting[nativeID] = state
				for _, alias := range destinations {
					upstream := celsiusClone(command)
					if alias != primary {
						delete(upstream, "offset")
					}
					id := nextID
					nextID++
					pending[celsiusPendingKey{alias, id}] = nativeID
					wire := celsiusWire{id, upstream}
					if native && alias != primary {
						count := 0
						for _, outstanding := range awaiting {
							if outstanding.remaining[alias] {
								count++
							}
						}
						if count > 128 {
							if retire(alias, "pending_limit") != nil {
								return
							}
							continue
						}
						kind, _ := command["type"].(string)
						topic, _ := command["topic_id"].(string)
						key := celsiusDesiredKey{kind, topic}
						if kind == "unsubscribe" {
							delete(desired[alias], celsiusDesiredKey{"subscribe", topic})
						} else if _, known := desired[alias][key]; !known && len(desired[alias]) >= 128 {
							if retire(alias, "subscription_limit") != nil {
								return
							}
							continue
						} else {
							desired[alias][key] = wire
						}
					}
					if native && unavailable[alias] {
						if required[alias] {
							if retire(alias, "owner_unavailable") != nil {
								return
							}
						}
						continue
					}
					if native && !ready[alias] {
						continue
					}
					if err := celsiusSendWire(sockets[alias], wire); err != nil {
						if !native || alias == primary {
							return
						}
						if retire(alias, "send_failure") != nil {
							return
						}
						continue
					}
					if native && !required[primary] {
						state.deadline = b.now().Add(b.ackTimeout)
					}
				}
			}
			continue
		}
		if batch, ok := value.([]any); ok {
			var forwarded []any
			for _, item := range batch {
				frame, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if reply, ok := frame["reply"]; ok {
					id, valid := celsiusID(frame["id"])
					if !valid {
						continue
					}
					key := celsiusPendingKey{source, id}
					nativeID, exists := pending[key]
					delete(pending, key)
					if !exists {
						continue
					}
					state := awaiting[nativeID]
					if state == nil {
						continue
					}
					delete(state.remaining, source)
					state.replies[source] = reply
					if len(state.remaining) == 0 {
						selected, ok := state.replies[primary]
						if !ok {
							selected = reply
						}
						forwarded = append(forwarded, complete(nativeID, selected))
					}
				} else if source == primary {
					forwarded = append(forwarded, frame)
				} else if projected := celsiusSecondaryFrame(frame, grant.owners, source); projected != nil {
					forwarded = append(forwarded, projected)
					payload := projected["payload"].(map[string]any)
					b.event(map[string]any{"celsius_secondary_event_type": payload["type"]})
				}
			}
			if len(forwarded) > 0 {
				if celsiusSend(client, forwarded) != nil {
					return
				}
			}
		} else if source == primary {
			if client.WriteMessage(websocket.TextMessage, message.data) != nil {
				return
			}
		}
	}
}
