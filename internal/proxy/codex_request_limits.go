package proxy

const (
	codexHTTPRequestMaxBytes      = 0
	codexWebSocketMessageMaxBytes = 64 << 20
	// OpenAI rejected observed 17.6 MB and 19.0 MB requests with close 1009.
	// Reject above 16 MiB before lease admission, then require HTTP on reconnect.
	codexWebSocketUpstreamRequestMaxBytes = 16 << 20
	codexDiagnosticLineMaxBytes           = 10 << 20
)

func codexLimitExceeded(size, limit int) bool {
	return limit > 0 && size > limit
}
