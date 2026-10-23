package output

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/jacobcxdev/cq/internal/app"
)

// JSONRenderer renders a Report as JSON.
type JSONRenderer struct {
	W        io.Writer
	Pretty   bool
	Colorise bool
}

func (r *JSONRenderer) Render(_ context.Context, report app.Report) error {
	if r.Pretty {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if r.Colorise {
			data = coloriseJSON(data)
		}
		if _, err := r.W.Write(data); err != nil {
			return err
		}
		_, err = r.W.Write([]byte("\n"))
		return err
	}
	return json.NewEncoder(r.W).Encode(report)
}

// coloriseJSON adds ANSI colour codes to JSON output.
// Keys: bold blue, strings: green, numbers: yellow, booleans: yellow, null: dim.
func coloriseJSON(src []byte) []byte {
	var out bytes.Buffer
	i := 0
	for i < len(src) {
		if src[i] == '"' {
			j := i + 1
			for j < len(src) {
				if src[j] == '\\' && j+1 < len(src) {
					j += 2
					continue
				}
				if src[j] == '"' {
					j++
					break
				}
				j++
			}
			str := src[i:j]
			k := j
			for k < len(src) && (src[k] == ' ' || src[k] == '\t') {
				k++
			}
			if k < len(src) && src[k] == ':' {
				out.WriteString("\033[1;34m")
				out.Write(str)
				out.WriteString("\033[0m")
			} else {
				out.WriteString("\033[32m")
				out.Write(str)
				out.WriteString("\033[0m")
			}
			i = j
		} else if (src[i] >= '0' && src[i] <= '9') || (src[i] == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9') {
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == '-' || src[j] == 'e' || src[j] == 'E' || src[j] == '+') {
				j++
			}
			out.WriteString("\033[33m")
			out.Write(src[i:j])
			out.WriteString("\033[0m")
			i = j
		} else if i+4 <= len(src) && string(src[i:i+4]) == "true" {
			out.WriteString("\033[33mtrue\033[0m")
			i += 4
		} else if i+5 <= len(src) && string(src[i:i+5]) == "false" {
			out.WriteString("\033[33mfalse\033[0m")
			i += 5
		} else if i+4 <= len(src) && string(src[i:i+4]) == "null" {
			out.WriteString("\033[2mnull\033[0m")
			i += 4
		} else {
			out.WriteByte(src[i])
			i++
		}
	}
	return out.Bytes()
}
