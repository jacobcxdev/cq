package modelregistry

import (
	"context"
	"encoding/json"
	"time"
)

// SourceResult is the output of a NativeSource.Fetch call.
type SourceResult struct {
	// Entries are the parsed model entries from the upstream provider.
	Entries []Entry
	// CodexRawByID holds the full upstream JSON keyed by model slug/ID.
	// Populated only by CodexSource.
	CodexRawByID map[string]json.RawMessage
	// AnthropicRawByID holds the full upstream JSON keyed by model ID.
	// Populated only by AnthropicSource.
	AnthropicRawByID map[string]json.RawMessage
	// FetchedAt is the wall-clock time the data was retrieved.
	FetchedAt time.Time
	// MalformedEntries is the count of raw model entries that were skipped
	// because they failed to unmarshal or had an empty slug/ID.
	MalformedEntries int
}

// NativeSource is the interface implemented by provider-specific model catalogues.
type NativeSource interface {
	Fetch(ctx context.Context) (SourceResult, error)
}

// SourceFunc is a function adapter for NativeSource.
type SourceFunc func(context.Context) (SourceResult, error)

// Fetch implements NativeSource.
func (f SourceFunc) Fetch(ctx context.Context) (SourceResult, error) { return f(ctx) }
