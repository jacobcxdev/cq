package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

type responsesStateBindings[S comparable] struct {
	mu             sync.Mutex
	responses      map[string]S
	opaque         map[string]S
	reasoning      map[string]json.RawMessage
	reasoningBytes int
}

// Bind native opaque state to the exact subscription that produced it. A
// restart forgets these ephemeral bindings and requires a fresh plaintext chain.
func (b *responsesStateBindings[S]) record(state S, id string, output []json.RawMessage) {
	var zero S
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.responses == nil {
		b.responses = make(map[string]S)
		b.opaque = make(map[string]S)
		b.reasoning = make(map[string]json.RawMessage)
	}
	if len(b.responses) >= 4096 {
		for k := range b.responses {
			delete(b.responses, k)
			break
		}
	}
	if id != "" {
		if previous, exists := b.responses[id]; exists && previous != state {
			b.responses[id] = zero
		} else {
			b.responses[id] = state
		}
	}
	for _, raw := range output {
		var item struct {
			Encrypted string `json:"encrypted_content"`
			Type      string `json:"type"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Encrypted != "" {
			if len(b.opaque) >= 4096 {
				for k := range b.opaque {
					delete(b.opaque, k)
					b.reasoningBytes -= len(b.reasoning[k])
					delete(b.reasoning, k)
					break
				}
			}
			sum := sha256.Sum256([]byte(item.Encrypted))
			key := hex.EncodeToString(sum[:])
			if previous, exists := b.opaque[key]; exists && previous != state {
				b.opaque[key] = zero
				b.reasoningBytes -= len(b.reasoning[key])
				delete(b.reasoning, key)
			} else {
				b.opaque[key] = state
				if item.Type == "reasoning" {
					b.reasoningBytes -= len(b.reasoning[key])
					delete(b.reasoning, key)
					for b.reasoningBytes+len(raw) > maxRequestBody {
						for old, value := range b.reasoning {
							b.reasoningBytes -= len(value)
							delete(b.reasoning, old)
							delete(b.opaque, old)
							break
						}
						if len(raw) > maxRequestBody {
							delete(b.opaque, key)
							break
						}
					}
					if len(raw) <= maxRequestBody {
						b.reasoning[key] = append(json.RawMessage(nil), raw...)
						b.reasoningBytes += len(raw)
					}
				}
			}
		}
	}
}
func (b *responsesStateBindings[S]) hasReferences() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.responses) != 0 || len(b.opaque) != 0
}
