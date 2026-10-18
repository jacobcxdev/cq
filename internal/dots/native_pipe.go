package dots

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"
)

const nativeMaxFrame = 2 * 1024 * 1024
const nativeMaxRequest = 1024 * 1024
const nativeTimeout = 35 * time.Second

// NativeEvaluator invokes the stock renderer inside the verified app process.
type NativeEvaluator interface {
	EvaluateAll(context.Context, string) ([]any, error)
}

type nativePipe struct {
	read, write *os.File
	commands    sync.Mutex
	mu          sync.Mutex
	sequence    uint64
	pending     uint64
	reply       chan map[string]any
	done        chan struct{}
}

func newNativePipe(read, write *os.File) *nativePipe {
	p := &nativePipe{read: read, write: write, done: make(chan struct{})}
	go p.readLoop()
	return p
}

func (p *nativePipe) readLoop() {
	defer func() { _ = recover(); close(p.done) }()
	r := bufio.NewReaderSize(p.read, 65536)
	var frame []byte
	dropping := false
	for {
		fragment, err := r.ReadSlice(0)
		if !dropping {
			if len(frame)+len(fragment) > nativeMaxFrame {
				frame = nil
				dropping = true
			} else {
				frame = append(frame, fragment...)
			}
		}
		if err == nil {
			if !dropping {
				var value map[string]any
				if json.Unmarshal(frame[:len(frame)-1], &value) == nil {
					p.mu.Lock()
					if id, ok := value["id"].(float64); ok && id == float64(p.pending) && p.reply != nil {
						select {
						case p.reply <- value:
						default:
						}
					}
					p.mu.Unlock()
				}
			}
			frame = nil
			dropping = false
		} else if !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

func (p *nativePipe) call(ctx context.Context, method string, params map[string]any, session string) (map[string]any, error) {
	p.commands.Lock()
	defer p.commands.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.sequence++
	id := p.sequence
	reply := make(chan map[string]any, 1)
	p.pending = id
	p.reply = reply
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.pending = 0; p.reply = nil; p.mu.Unlock() }()
	if params == nil {
		params = map[string]any{}
	}
	value := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		value["sessionId"] = session
	}
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > nativeMaxRequest {
		return nil, errors.New("native debugger command too large")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(nativeTimeout)
	}
	if err = p.write.SetWriteDeadline(deadline); err != nil {
		return nil, errors.New("native debugger pipe unavailable")
	}
	data = append(data, 0)
	for len(data) > 0 {
		n, e := p.write.Write(data)
		if e != nil {
			return nil, errors.New("native debugger pipe unavailable")
		}
		data = data[n:]
	}
	select {
	case response := <-reply:
		result, ok := response["result"].(map[string]any)
		if response["error"] != nil || !ok {
			return nil, errors.New("native debugger command failed")
		}
		return result, nil
	case <-ctx.Done():
		return nil, errors.New("native debugger command timed out")
	case <-p.done:
		return nil, errors.New("native debugger pipe unavailable")
	}
}

func (p *nativePipe) evaluate(ctx context.Context, expression string, all bool) ([]any, error) {
	if expression == "" {
		return nil, errors.New("native debugger expression invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, nativeTimeout)
	defer cancel()
	response, err := p.call(ctx, "Target.getTargets", nil, "")
	if err != nil {
		return nil, err
	}
	infos, ok := response["targetInfos"].([]any)
	if !ok {
		return nil, errors.New("native renderer unavailable")
	}
	var targets []string
	for _, info := range infos {
		t, ok := info.(map[string]any)
		if !ok {
			continue
		}
		id, _ := t["targetId"].(string)
		if t["type"] == "page" && t["url"] == "app://-/index.html" && id != "" {
			targets = append(targets, id)
		}
	}
	sort.Strings(targets)
	if len(targets) == 0 {
		return nil, errors.New("native renderer unavailable")
	}
	if !all {
		targets = targets[:1]
	}
	values := make([]any, 0, len(targets))
	for _, target := range targets {
		value, e := p.evaluateTarget(ctx, target, expression)
		if e != nil {
			if !all {
				return nil, e
			}
			value = map[string]any{"installed": false}
		}
		values = append(values, value)
	}
	return values, nil
}

func (p *nativePipe) evaluateTarget(ctx context.Context, target, expression string) (any, error) {
	attached, err := p.call(ctx, "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, "")
	if err != nil {
		return nil, err
	}
	session, _ := attached["sessionId"].(string)
	if session == "" {
		return nil, errors.New("native renderer session unavailable")
	}
	defer func() {
		deadline := time.Now().Add(2 * time.Second)
		if original, ok := ctx.Deadline(); ok && original.Before(deadline) {
			deadline = original
		}
		detach, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		defer cancel()
		_, _ = p.call(detach, "Target.detachFromTarget", map[string]any{"sessionId": session}, "")
	}()
	result, err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}, session)
	if err != nil {
		return nil, err
	}
	remote, ok := result["result"].(map[string]any)
	if result["exceptionDetails"] != nil || !ok {
		return nil, errors.New("native API invocation failed")
	}
	return remote["value"], nil
}
