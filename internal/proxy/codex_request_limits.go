package proxy

const (
	codexHTTPRequestMaxBytes      = 0
	codexWebSocketMessageMaxBytes = 64 << 20
	codexDiagnosticLineMaxBytes   = 10 << 20
)

func codexLimitExceeded(size, limit int) bool {
	return limit > 0 && size > limit
}
