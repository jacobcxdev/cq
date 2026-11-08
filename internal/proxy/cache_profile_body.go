package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"sync"

	"github.com/jacobcxdev/cq/internal/cacheprofile"
)

const cacheProfileResponseLimit = 1 << 20

// Reading the upstream controls progress. Profiling never drains, retries,
// blocks for another chunk or changes errors observed by the caller.
type cacheProfileBody struct {
	io.ReadCloser
	attempt *cacheProfileAttempt
	mu      sync.Mutex
	parser  cacheProfileUsageParser
}

func newCacheProfileBody(body io.ReadCloser, contentType string, a *cacheProfileAttempt) io.ReadCloser {
	media, _, _ := mime.ParseMediaType(contentType)
	b := &cacheProfileBody{ReadCloser: body, attempt: a}
	if media == "text/event-stream" {
		b.parser.sse = NewCodexSSEParser(cacheProfileResponseLimit)
	} else if media != "application/json" {
		b.parser.failed = true
	}
	return b
}

func (b *cacheProfileBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if n > 0 {
		b.parser.feed(p[:n])
	}
	if err == io.EOF {
		b.parser.eof()
		b.attempt.finish("completed", b.parser.result())
	} else if err != nil {
		b.attempt.finish("read_error", b.parser.result())
	}
	b.mu.Unlock()
	return n, err
}

func (b *cacheProfileBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	b.attempt.finish("closed", b.parser.result())
	b.mu.Unlock()
	return err
}

type cacheProfileUsageParser struct {
	sse       *CodexSSEParser
	body      []byte
	failed    bool
	terminal  bool
	usage     cacheprofile.Usage
	anthropic map[string]json.RawMessage
}

func (p *cacheProfileUsageParser) feed(data []byte) {
	if p.failed {
		return
	}
	if p.sse == nil {
		if len(data) > cacheProfileResponseLimit-len(p.body) {
			p.failed = true
			p.body = nil
			return
		}
		p.body = append(p.body, data...)
		return
	}
	events, err := p.sse.Feed(data)
	if err != nil {
		p.failed = true
		return
	}
	for _, event := range events {
		p.consume(event.Data)
	}
}

func (p *cacheProfileUsageParser) eof() {
	if p.failed {
		return
	}
	if p.sse != nil {
		events, err := p.sse.Finish()
		if err != nil {
			p.failed = true
			return
		}
		for _, event := range events {
			p.consume(event.Data)
		}
		return
	}
	if !validCacheProfileJSON(p.body) {
		p.failed = true
		return
	}
	if usage, ok := cacheprofile.UsageFromJSON(p.body); ok {
		p.usage = usage
		p.terminal = true
	}
	p.body = nil
}

func (p *cacheProfileUsageParser) result() cacheprofile.Usage {
	if p.failed || !p.terminal {
		return cacheprofile.Usage{}
	}
	return p.usage
}

func (p *cacheProfileUsageParser) consume(data []byte) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		p.terminal = true
		return
	}
	if !validCacheProfileJSON(data) {
		p.failed = true
		return
	}
	var event struct {
		Type    string `json:"type"`
		Message struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &event) != nil {
		p.failed = true
		return
	}
	switch event.Type {
	case "message_start":
		if p.anthropic != nil {
			p.failed = true
			return
		}
		if _, ok := cacheprofile.UsageFromJSON(data); !ok {
			p.failed = true
			return
		}
		if json.Unmarshal(event.Message.Usage, &p.anthropic) != nil || p.anthropic == nil {
			p.failed = true
			return
		}
		p.updateAnthropic()
	case "message_delta":
		if p.anthropic == nil {
			p.failed = true
			return
		}
		var delta map[string]json.RawMessage
		if len(event.Usage) == 0 {
			return
		}
		if json.Unmarshal(event.Usage, &delta) != nil || delta == nil {
			p.failed = true
			return
		}
		for key, value := range delta {
			p.anthropic[key] = value
		}
		p.updateAnthropic()
	case "message_stop":
		p.terminal = true
	case "response.completed", "response.failed", "response.incomplete":
		usage, ok := cacheprofile.UsageFromJSON(data)
		if ok {
			p.usage = usage
		}
		p.terminal = true
	default:
		if usage, ok := cacheprofile.UsageFromJSON(data); ok {
			p.usage = usage
			p.terminal = true
		}
	}
}

func (p *cacheProfileUsageParser) updateAnthropic() {
	data, err := json.Marshal(map[string]any{"usage": p.anthropic})
	if err != nil {
		p.failed = true
		return
	}
	usage, ok := cacheprofile.UsageFromJSON(data)
	if !ok {
		p.failed = true
		return
	}
	p.usage = usage
}

// Unknown or duplicate usage authority is not a zero-cache observation. Check
// before decoding maps so duplicate fields cannot disappear during SSE merging.
func validCacheProfileJSON(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			close, err := decoder.Token()
			return err == nil && close == json.Delim('}')
		case '[':
			for decoder.More() {
				if !value(depth + 1) {
					return false
				}
			}
			close, err := decoder.Token()
			return err == nil && close == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}
