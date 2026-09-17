package cli

import (
	"io"
	"strings"
)

// Confirm consumes exactly one LF-terminated response. The handler supplies the
// exact prompt, enforces JSON/TTY/consent flags before calling, and pauses its
// budget only after all work has quiesced. EOF never grants consent, even if it
// follows an unterminated affirmative word.
func Confirm(session *Session, prompt string) (bool, error) {
	if err := writeBytes(session.Err, []byte(prompt)); err != nil {
		return false, err
	}
	var response strings.Builder
	var one [1]byte
	for {
		// A temporary buffered reader could consume the next confirmation's
		// response. Single-byte reads leave subsequent input with its owner.
		_, err := io.ReadFull(session.In, one[:])
		if err != nil {
			if err == io.EOF {
				return false, nil
			}
			return false, err
		}
		if one[0] == '\n' {
			answer := strings.TrimSpace(response.String())
			return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
		}
		response.WriteByte(one[0])
	}
}
