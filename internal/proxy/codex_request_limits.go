package proxy

const (
	codexHTTPRequestMaxBytes      = 0
	codexWebSocketMessageMaxBytes = 64 << 20
	// OpenAI rejected an observed 40.9 MiB Codex request with WebSocket close
	// 1009. Bound CQ at 32 MiB before lease admission so Codex can fall back to
	// HTTP safely without repeatedly sending an already-large request.
	codexWebSocketUpstreamRequestMaxBytes = 32 << 20
	codexDiagnosticLineMaxBytes           = 10 << 20
)

func codexLimitExceeded(size, limit int) bool {
	return limit > 0 && size > limit
}
