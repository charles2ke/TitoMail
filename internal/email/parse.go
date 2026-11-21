// Package email parses, builds and sanitizes RFC 5322 / MIME messages.
package email

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	netmail "net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // register non-UTF-8 charsets
	gomail "github.com/emersion/go-message/mail"
	"golang.org/x/text/encoding/charmap"
)

// maxBodyBytes limits how much of a single text part is decoded for display.
const maxBodyBytes = 4 << 20

// Address is a mailbox address with an optional display name.
type Address struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// String formats the address for use in a message header.
func (a Address) String() string {
	return (&netmail.Address{Name: a.Name, Address: a.Address}).String()
}

// Display returns the display name, or the address when no name is set.
func (a Address) Display() string {
	if a.Name != "" {
		return a.Name
	}
	return a.Address
}

// Attachment describes a non-body MIME part.
type Attachment struct {
	PartID      string `json:"part"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	ContentID   string `json:"content_id,omitempty"`
	// Inline is true for parts referenced from the HTML body via cid: URLs.
	Inline bool `json:"inline"`
}

// Message is the parsed, display-oriented representation of a message.
type Message struct {
	MessageID   string
	InReplyTo   []string
	References  []string
	Subject     string
	From        []Address
	Sender      []Address
	ReplyTo     []Address
	To          []Address
	Cc          []Address
	Bcc         []Address
	Date        time.Time
	Text        string
	HTML        string
	Attachments []Attachment
	Header      gomail.Header
}

// Parse parses a raw message. It never fails: malformed input degrades to a
// best-effort result so that every message can be stored and displayed.
func Parse(raw []byte) *Message {
	m := &Message{}
	ent, _ := message.Read(bytes.NewReader(raw))
	if ent == nil {
		// The header could not be parsed at all; show the raw bytes as text.
		m.Text = toUTF8(raw)
		return m
	}
	m.Header = gomail.Header{Header: ent.Header}
	m.parseHeader()

	var texts, htmls []string
	_ = ent.Walk(func(path []int, part *message.Entity, err error) error {
		if part == nil {
			return nil
		}
		mediaType, params, _ := part.Header.ContentType()
		if mediaType == "" {
			mediaType = "text/plain"
		}
		if strings.HasPrefix(mediaType, "multipart/") {
			return nil
		}
		disp, dparams, _ := part.Header.ContentDisposition()
		filename := decodeFilename(dparams["filename"])
		if filename == "" {
			filename = decodeFilename(params["name"])
		}
		isBody := (mediaType == "text/plain" || mediaType == "text/html") &&
			disp != "attachment" && (filename == "" || disp == "inline")
		if isBody && len(texts)+len(htmls) < 50 {
			b, _ := io.ReadAll(io.LimitReader(part.Body, maxBodyBytes))
			s := toUTF8(b)
			if mediaType == "text/html" {
				htmls = append(htmls, s)
			} else {
				texts = append(texts, s)
			}
			return nil
		}
		n, _ := io.Copy(io.Discard, part.Body)
		att := Attachment{
			PartID:      partID(path),
			Filename:    filename,
			ContentType: mediaType,
			Size:        n,
			ContentID:   strings.Trim(part.Header.Get("Content-Id"), "<> "),
		}
		if att.Filename == "" {
			att.Filename = defaultFilename(part, mediaType, len(m.Attachments)+1)
		}
		m.Attachments = append(m.Attachments, att)
		return nil
	})

	m.Text = strings.Join(texts, "\n")
	m.HTML = strings.Join(htmls, "\n")
	if m.HTML != "" {
		lower := strings.ToLower(m.HTML)
		for i := range m.Attachments {
			a := &m.Attachments[i]
			if a.ContentID != "" && strings.Contains(lower, "cid:"+strings.ToLower(a.ContentID)) {
				a.Inline = true
			}
		}
	}
	return m
}

func (m *Message) parseHeader() {
	h := m.Header
	m.Subject, _ = h.Subject()
	if m.Subject == "" {
		m.Subject = h.Get("Subject")
	}
	m.Subject = cleanHeaderText(m.Subject)
	m.From = addressList(h, "From")
	m.Sender = addressList(h, "Sender")
	m.ReplyTo = addressList(h, "Reply-To")
	m.To = addressList(h, "To")
	m.Cc = addressList(h, "Cc")
	m.Bcc = addressList(h, "Bcc")
	if id, err := h.MessageID(); err == nil {
		m.MessageID = id
	} else {
		m.MessageID = strings.Trim(strings.TrimSpace(h.Get("Message-Id")), "<>")
	}
	m.InReplyTo = msgIDList(h, "In-Reply-To")
	m.References = msgIDList(h, "References")
	if d, err := h.Date(); err == nil && !d.IsZero() {
		m.Date = d
	}
}

// AllRecipients returns To, Cc and Bcc combined.
func (m *Message) AllRecipients() []Address {
	out := make([]Address, 0, len(m.To)+len(m.Cc)+len(m.Bcc))
	out = append(out, m.To...)
	out = append(out, m.Cc...)
	return append(out, m.Bcc...)
}

// PrimaryFrom returns the first From address, if any.
func (m *Message) PrimaryFrom() Address {
	if len(m.From) > 0 {
		return m.From[0]
	}
	if len(m.Sender) > 0 {
		return m.Sender[0]
	}
	return Address{}
}

// BodyText returns a plain-text rendition of the body.
func (m *Message) BodyText() string {
	if strings.TrimSpace(m.Text) != "" {
		return m.Text
	}
	if m.HTML != "" {
		return HTMLToText(m.HTML)
	}
	return ""
}

// Snippet returns a short single-line preview of the body.
func (m *Message) Snippet() string {
	return MakeSnippet(m.BodyText(), 200)
}

// MakeSnippet collapses whitespace and truncates text to max runes.
func MakeSnippet(text string, max int) string {
	var b strings.Builder
	space := false
	count := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip quoted text and common reply separators in previews.
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		for _, r := range trimmed {
			if r == '\u200c' || r == '\u200b' || r == '\u00ad' || r == '\ufeff' || r == '\u034f' {
				continue
			}
			if r == ' ' || r == '\t' || r == '\r' || r == '\u00a0' {
				space = true
				continue
			}
			if space && b.Len() > 0 {
				b.WriteByte(' ')
				count++
			}
			space = false
			b.WriteRune(r)
			count++
			if count >= max {
				return b.String() + "…"
			}
		}
		space = true
	}
	return b.String()
}

// ExtractPart returns the header and decoded body of the MIME part with the
// given IMAP-style part number (e.g. "2" or "1.3").
func ExtractPart(raw []byte, id string) (message.Header, []byte, error) {
	ent, err := message.Read(bytes.NewReader(raw))
	if ent == nil {
		return message.Header{}, nil, err
	}
	var (
		found  bool
		header message.Header
		body   []byte
	)
	errStop := fmt.Errorf("stop")
	err = ent.Walk(func(path []int, part *message.Entity, err error) error {
		if part == nil {
			return nil
		}
		if partID(path) != id {
			return nil
		}
		if mt, _, _ := part.Header.ContentType(); strings.HasPrefix(mt, "multipart/") {
			return nil
		}
		b, rerr := io.ReadAll(part.Body)
		if rerr != nil {
			return rerr
		}
		found, header, body = true, part.Header, b
		return errStop
	})
	if !found {
		if err != nil && err != errStop {
			return message.Header{}, nil, err
		}
		return message.Header{}, nil, fmt.Errorf("part %s not found", id)
	}
	return header, body, nil
}

// AttachmentByContentID finds the attachment with the given Content-ID.
func (m *Message) AttachmentByContentID(cid string) *Attachment {
	for i := range m.Attachments {
		if strings.EqualFold(m.Attachments[i].ContentID, cid) {
			return &m.Attachments[i]
		}
	}
	return nil
}

func partID(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = strconv.Itoa(p + 1)
	}
	return strings.Join(parts, ".")
}

func addressList(h gomail.Header, key string) []Address {
	if !h.Has(key) {
		return nil
	}
	list, err := h.AddressList(key)
	if err == nil {
		out := make([]Address, 0, len(list))
		for _, a := range list {
			if a == nil {
				continue
			}
			out = append(out, Address{Name: cleanHeaderText(a.Name), Address: strings.TrimSpace(a.Address)})
		}
		return out
	}
	// Fall back to a lenient parse for malformed headers.
	raw, _ := h.Text(key)
	if raw == "" {
		raw = h.Get(key)
	}
	var out []Address
	for _, piece := range splitAddressList(raw) {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		if a, err := netmail.ParseAddress(piece); err == nil {
			out = append(out, Address{Name: cleanHeaderText(a.Name), Address: a.Address})
			continue
		}
		name, addr := piece, piece
		if lt := strings.LastIndex(piece, "<"); lt >= 0 {
			if gt := strings.Index(piece[lt:], ">"); gt > 0 {
				addr = piece[lt+1 : lt+gt]
				name = strings.Trim(strings.TrimSpace(piece[:lt]), `"`)
			}
		}
		if strings.Contains(addr, "@") {
			if name == addr {
				name = ""
			}
			out = append(out, Address{Name: cleanHeaderText(name), Address: strings.TrimSpace(addr)})
		}
	}
	return out
}

func splitAddressList(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, depth := false, 0
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '<' && !inQuote:
			depth++
		case r == '>' && !inQuote && depth > 0:
			depth--
		case (r == ',' || r == ';') && !inQuote && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	return append(out, cur.String())
}

func msgIDList(h gomail.Header, key string) []string {
	if !h.Has(key) {
		return nil
	}
	if ids, err := h.MsgIDList(key); err == nil && len(ids) > 0 {
		return ids
	}
	// Lenient fallback: grab anything between angle brackets.
	v := h.Get(key)
	var out []string
	for {
		lt := strings.Index(v, "<")
		if lt < 0 {
			break
		}
		gt := strings.Index(v[lt:], ">")
		if gt < 0 {
			break
		}
		if id := strings.TrimSpace(v[lt+1 : lt+gt]); id != "" {
			out = append(out, id)
		}
		v = v[lt+gt+1:]
	}
	return out
}

var wordDecoder = &mime.WordDecoder{CharsetReader: func(charset string, r io.Reader) (io.Reader, error) {
	if message.CharsetReader == nil {
		return nil, fmt.Errorf("unsupported charset %q", charset)
	}
	return message.CharsetReader(charset, r)
}}

func decodeFilename(s string) string {
	if s == "" {
		return ""
	}
	if strings.Contains(s, "=?") {
		if d, err := wordDecoder.DecodeHeader(s); err == nil {
			s = d
		}
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func defaultFilename(part *message.Entity, mediaType string, n int) string {
	if mediaType == "message/rfc822" {
		return "message.eml"
	}
	ext := ""
	if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
		ext = exts[0]
	}
	switch mediaType {
	case "message/delivery-status":
		return "delivery-status.txt"
	case "text/rfc822-headers":
		return "headers.txt"
	case "text/calendar":
		return "invite.ics"
	}
	return fmt.Sprintf("attachment-%d%s", n, ext)
}

func cleanHeaderText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(strings.ToValidUTF8(s, "\uFFFD"))
}

// toUTF8 converts bytes to a valid UTF-8 string, assuming Windows-1252 for
// undeclared 8-bit content that is not valid UTF-8.
func toUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if d, err := charmap.Windows1252.NewDecoder().Bytes(b); err == nil {
		return string(d)
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}
