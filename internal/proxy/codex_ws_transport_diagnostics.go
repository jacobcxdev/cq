package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"syscall"

	"github.com/gorilla/websocket"
)

// A dial failure retains only a fixed diagnostic label. The original cause is
// unwrapped for control flow but its text, URL and addresses are never logged.
type codexWSDialTransportError struct {
	reason codexHTTPTransportFailureReason
	cause  error
}

func (*codexWSDialTransportError) Error() string     { return "Codex upstream WebSocket dial failed" }
func (err *codexWSDialTransportError) Unwrap() error { return err.cause }

func codexWSDialResultError(response *http.Response, dialErr, parseErr error) error {
	cause := errors.Join(dialErr, parseErr)
	if response != nil || dialErr == nil {
		return cause
	}
	// Credential, reserve and continuity errors can precede the socket dial.
	// Keep their existing authority classification; cancellation and deadlines
	// still describe this attempted dial's transport outcome.
	domain := codexRequestFailureReason(dialErr)
	if domain != CodexRequestFailureUnknown && domain != CodexRequestFailureContextCanceled && domain != CodexRequestFailureDeadlineExceeded {
		return cause
	}
	var reserve *codexReserveLimitError
	if errors.As(dialErr, &reserve) {
		return cause
	}
	reason := classifyCodexHTTPTransportFailure(dialErr, codexHTTPTransportFacts{})
	var certificateErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	switch {
	case errors.Is(dialErr, os.ErrDeadlineExceeded):
		reason = codexHTTPTransportFailureTimeout
	case errors.Is(dialErr, syscall.ECONNREFUSED):
		reason = "connection_refused"
	case errors.As(dialErr, &certificateErr), errors.As(dialErr, &unknownAuthority), errors.As(dialErr, &hostnameErr), errors.As(dialErr, &invalidCertificate):
		reason = codexHTTPTransportFailureTLS
	case errors.Is(dialErr, websocket.ErrBadHandshake):
		reason = codexHTTPTransportFailureProtocol
	}
	return &codexWSDialTransportError{reason: reason, cause: cause}
}

func markCodexWSDialTerminal(ctx context.Context, dial codexWSDialResult, accountHint string, status int) {
	reason, errorClass := codexWSProviderFailureClassification(dial.wrapped.HardUsageLimit, dial.wrapped.AuthFailure)
	markCodexWSProviderTerminal(ctx, string(CodexSSEError), reason, errorClass, accountHint, status)
	var transport *codexWSDialTransportError
	if errors.As(dial.err, &transport) {
		trace, _ := ctx.Value(codexWSTerminalTraceKey{}).(*codexWSTerminalTrace)
		if trace != nil && trace.event != nil {
			trace.event.Stage = "upstream_dial"
			trace.event.Reason = string(transport.reason)
			trace.event.ErrorClass = "transport_error"
			trace.event.UpstreamStatus = 0
		}
	}
}
