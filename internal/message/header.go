// Package message provides read-only views of immutable message bytes.
package message

import (
	"bufio"
	"bytes"
	"io"
	"net/textproto"
	"strings"
)

// Headers parses each header field independently, ignoring malformed fields and
// their continuations. A bad field must not hide valid fields that follow it.
// Orphan continuations are ignored, and the first empty line ends the headers;
// body lines are never interpreted as headers. Raw bytes are never repaired.
// ignored counts rejected fields and orphan continuation lines.
func Headers(raw []byte) (header textproto.MIMEHeader, ignored int) {
	header = make(textproto.MIMEHeader)
	start, end := -1, 0
	flush := func() {
		if start < 0 {
			return
		}
		// Extra newlines also terminate a final field lacking a trailing newline.
		reader := io.MultiReader(bytes.NewReader(raw[start:end]), strings.NewReader("\r\n\r\n"))
		field, err := textproto.NewReader(bufio.NewReader(reader)).ReadMIMEHeader()
		if err != nil {
			ignored++
		} else {
			for name, values := range field {
				header[name] = append(header[name], values...)
			}
		}
		start = -1
	}
	for pos := 0; pos < len(raw); {
		next := len(raw)
		if n := bytes.IndexByte(raw[pos:], '\n'); n >= 0 {
			next = pos + n + 1
		}
		line := bytes.TrimSuffix(bytes.TrimSuffix(raw[pos:next], []byte("\n")), []byte("\r"))
		if len(line) == 0 {
			flush()
			return header, ignored
		}
		if line[0] == ' ' || line[0] == '\t' {
			if start < 0 {
				ignored++
			} else {
				end = next
			}
		} else {
			flush()
			start, end = pos, next
		}
		pos = next
	}
	flush()
	return header, ignored
}
