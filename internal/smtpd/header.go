package smtpd

import (
	"bytes"
	"strings"
)

// headerField is one header field including its folded continuation lines
// and line terminator.
type headerField struct {
	name string
	raw  []byte
}

func (f headerField) value() string {
	_, v, _ := strings.Cut(string(f.raw), ":")
	v = strings.NewReplacer("\r\n", "", "\n", "").Replace(v)
	return strings.TrimSpace(v)
}

// splitHeader splits a message into its header fields and the remainder,
// which starts with the blank line separating header and body (if any).
func splitHeader(raw []byte) ([]headerField, []byte) {
	var fields []headerField
	pos := 0
	for pos < len(raw) {
		end := bytes.IndexByte(raw[pos:], '\n')
		if end < 0 {
			end = len(raw)
		} else {
			end += pos + 1
		}
		line := raw[pos:end]
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && len(fields) > 0 {
			last := &fields[len(fields)-1]
			last.raw = raw[pos-len(last.raw) : end]
		} else {
			name, _, _ := bytes.Cut(line, []byte(":"))
			fields = append(fields, headerField{name: strings.TrimSpace(string(name)), raw: line})
		}
		pos = end
	}
	return fields, raw[pos:]
}

// rewriteHeader prepends the given fields (without line terminators) and
// drops existing fields for which drop returns true.
func rewriteHeader(raw []byte, prepend []string, drop func(headerField) bool) []byte {
	fields, rest := splitHeader(raw)
	var b bytes.Buffer
	b.Grow(len(raw) + 512)
	for _, f := range prepend {
		b.WriteString(f)
		b.WriteString("\r\n")
	}
	for _, f := range fields {
		if drop != nil && drop(f) {
			continue
		}
		b.Write(f.raw)
		if !bytes.HasSuffix(f.raw, []byte("\n")) {
			b.WriteString("\r\n")
		}
	}
	b.Write(rest)
	return b.Bytes()
}

// countHeader returns how many fields named name the message has.
func countHeader(raw []byte, name string) int {
	fields, _ := splitHeader(raw)
	n := 0
	for _, f := range fields {
		if strings.EqualFold(f.name, name) {
			n++
		}
	}
	return n
}

// foldHeader formats a header field, folding it before parts after the
// first so that lines stay short.
func foldHeader(name string, parts ...string) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteString(":")
	for i, p := range parts {
		if i == 0 {
			b.WriteString(" ")
		} else {
			b.WriteString("\r\n\t")
		}
		b.WriteString(p)
	}
	return b.String()
}
