package email

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"mime"
	netmail "net/mail"
	"strings"
	"time"

	gomail "github.com/emersion/go-message/mail"
)

// OutgoingAttachment is a file attached to a composed message.
type OutgoingAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Outgoing describes a message composed by a user.
type Outgoing struct {
	From        Address
	To          []Address
	Cc          []Address
	Bcc         []Address
	ReplyTo     []Address
	Subject     string
	HTML        string
	Text        string
	MessageID   string
	InReplyTo   string
	References  []string
	Date        time.Time
	Attachments []OutgoingAttachment
	// Headers are additional header fields, e.g. Auto-Submitted.
	Headers [][2]string
}

// NewMessageID returns a new globally unique Message-ID (without brackets).
func NewMessageID(domain string) string {
	var b [15]byte
	_, _ = rand.Read(b[:])
	id := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
	if domain == "" {
		domain = "titomail.local"
	}
	return fmt.Sprintf("%d.%s@%s", time.Now().UnixMilli(), id, domain)
}

// Build serializes the message. When includeBcc is true the Bcc header is
// kept, which is appropriate for the sender's own Sent copy only.
func (o *Outgoing) Build(includeBcc bool) ([]byte, error) {
	if o.Date.IsZero() {
		o.Date = time.Now()
	}
	if o.MessageID == "" {
		o.MessageID = NewMessageID(domainOf(o.From.Address))
	}
	text := o.Text
	if text == "" && o.HTML != "" {
		text = HTMLToText(o.HTML)
	}

	var h gomail.Header
	h.SetDate(o.Date)
	h.SetAddressList("From", toMailAddrs([]Address{o.From}))
	if len(o.To) > 0 {
		h.SetAddressList("To", toMailAddrs(o.To))
	}
	if len(o.Cc) > 0 {
		h.SetAddressList("Cc", toMailAddrs(o.Cc))
	}
	if includeBcc && len(o.Bcc) > 0 {
		h.SetAddressList("Bcc", toMailAddrs(o.Bcc))
	}
	if len(o.ReplyTo) > 0 {
		h.SetAddressList("Reply-To", toMailAddrs(o.ReplyTo))
	}
	h.SetSubject(o.Subject)
	h.SetMessageID(o.MessageID)
	if o.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{o.InReplyTo})
	}
	if len(o.References) > 0 {
		h.SetMsgIDList("References", o.References)
	}
	h.Set("MIME-Version", "1.0")
	h.Set("User-Agent", "TitoMail")
	for _, kv := range o.Headers {
		h.Set(kv[0], kv[1])
	}

	var buf bytes.Buffer
	writeAlternative := func(iw *gomail.InlineWriter) error {
		var th gomail.InlineHeader
		th.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
		w, err := iw.CreatePart(th)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(text)); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		var hh gomail.InlineHeader
		hh.SetContentType("text/html", map[string]string{"charset": "utf-8"})
		w, err = iw.CreatePart(hh)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(htmlDocument(o.HTML))); err != nil {
			return err
		}
		return w.Close()
	}

	switch {
	case len(o.Attachments) == 0 && o.HTML == "":
		h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		w, err := gomail.CreateSingleInlineWriter(&buf, h)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(text)); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case len(o.Attachments) == 0:
		iw, err := gomail.CreateInlineWriter(&buf, h)
		if err != nil {
			return nil, err
		}
		if err := writeAlternative(iw); err != nil {
			return nil, err
		}
		if err := iw.Close(); err != nil {
			return nil, err
		}
	default:
		mw, err := gomail.CreateWriter(&buf, h)
		if err != nil {
			return nil, err
		}
		if o.HTML == "" {
			var th gomail.InlineHeader
			th.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
			w, err := mw.CreateSingleInline(th)
			if err != nil {
				return nil, err
			}
			if _, err := w.Write([]byte(text)); err != nil {
				return nil, err
			}
			if err := w.Close(); err != nil {
				return nil, err
			}
		} else {
			iw, err := mw.CreateInline()
			if err != nil {
				return nil, err
			}
			if err := writeAlternative(iw); err != nil {
				return nil, err
			}
			if err := iw.Close(); err != nil {
				return nil, err
			}
		}
		for _, a := range o.Attachments {
			var ah gomail.AttachmentHeader
			ct := a.ContentType
			if ct == "" {
				ct = "application/octet-stream"
			}
			mediaType, params, err := mime.ParseMediaType(ct)
			if err != nil {
				mediaType, params = "application/octet-stream", nil
			}
			ah.SetContentType(mediaType, params)
			ah.SetFilename(a.Filename)
			w, err := mw.CreateAttachment(ah)
			if err != nil {
				return nil, err
			}
			if _, err := w.Write(a.Data); err != nil {
				return nil, err
			}
			if err := w.Close(); err != nil {
				return nil, err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func htmlDocument(body string) string {
	if strings.Contains(strings.ToLower(body), "<html") {
		return body
	}
	return `<!DOCTYPE html><html><head><meta charset="utf-8"></head><body>` + body + `</body></html>`
}

func toMailAddrs(list []Address) []*gomail.Address {
	out := make([]*gomail.Address, 0, len(list))
	for _, a := range list {
		out = append(out, &gomail.Address{Name: a.Name, Address: a.Address})
	}
	return out
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// ParseAddressList parses a user-entered list of addresses separated by
// commas or semicolons.
func ParseAddressList(s string) ([]Address, error) {
	var out []Address
	for _, piece := range splitAddressList(s) {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		a, err := ParseAddress(piece)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// ParseAddress parses and validates a single address.
func ParseAddress(s string) (Address, error) {
	s = strings.TrimSpace(s)
	pa, err := parseNetAddress(s)
	if err != nil {
		return Address{}, fmt.Errorf("invalid address %q", s)
	}
	if !ValidAddress(pa.Address) {
		return Address{}, fmt.Errorf("invalid address %q", s)
	}
	return Address{Name: pa.Name, Address: pa.Address}, nil
}

// ValidAddress performs a pragmatic syntax check of an email address.
func ValidAddress(addr string) bool {
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 || len(addr) > 254 {
		return false
	}
	local, domain := addr[:at], addr[at+1:]
	if len(local) > 64 || strings.ContainsAny(addr, " \t\r\n<>()[]\\,;:\"") {
		return false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return false
	}
	return strings.Contains(domain, ".") || domain == "localhost"
}

// NormalizeAddress lower-cases the domain part of an address and trims it.
func NormalizeAddress(addr string) string {
	addr = strings.TrimSpace(strings.Trim(strings.TrimSpace(addr), "<>"))
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return strings.ToLower(addr)
	}
	return strings.ToLower(addr[:at]) + "@" + strings.ToLower(addr[at+1:])
}

// DomainOf returns the lower-cased domain part of an address.
func DomainOf(addr string) string {
	return strings.ToLower(domainOf(addr))
}

// LocalPart returns the part of an address before the @.
func LocalPart(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[:i]
	}
	return addr
}

func parseNetAddress(s string) (*netmail.Address, error) {
	a, err := netmail.ParseAddress(s)
	if err == nil {
		return a, nil
	}
	if a2, err2 := netmail.ParseAddress("<" + strings.Trim(s, "<> ") + ">"); err2 == nil {
		return a2, nil
	}
	return nil, err
}
