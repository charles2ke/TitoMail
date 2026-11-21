package delivery

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"regexp"
	"strings"
	"time"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

var statusCodeRe = regexp.MustCompile(`\b([245]\.\d{1,3}\.\d{1,3})\b`)

// headerSection returns the header of a raw message (up to the first empty
// line), with CRLF line endings.
func headerSection(raw []byte) []byte {
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if i := bytes.Index(raw, sep); i >= 0 {
			raw = raw[:i]
			break
		}
	}
	if len(raw) > 64<<10 {
		raw = raw[:64<<10]
	}
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return []byte(strings.ReplaceAll(s, "\n", "\r\n") + "\r\n")
}

// buildDSN creates a delivery status notification (RFC 3464) for failed
// recipients of e, threaded with the original message.
func buildDSN(hostname string, e *store.QueueEntry, failed []*store.QueueRcpt, now time.Time) []byte {
	var rb [12]byte
	_, _ = rand.Read(rb[:])
	boundary := "dsn-" + hex.EncodeToString(rb[:])
	var b bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...); b.WriteString("\r\n") }

	w("From: Mail Delivery Subsystem <mailer-daemon@%s>", hostname)
	w("To: <%s>", e.MailFrom)
	w("Subject: %s", mime.QEncoding.Encode("utf-8", "Delivery Status Notification (Failure)"))
	w("Date: %s", now.Format(time.RFC1123Z))
	w("Message-ID: <%s>", email.NewMessageID(hostname))
	if e.MessageID != "" {
		w("In-Reply-To: <%s>", e.MessageID)
		w("References: <%s>", e.MessageID)
	}
	w("Auto-Submitted: auto-replied")
	w("MIME-Version: 1.0")
	w("Content-Type: multipart/report; report-type=delivery-status; boundary=\"%s\"", boundary)
	w("")
	w("--%s", boundary)
	w("Content-Type: text/plain; charset=utf-8")
	w("Content-Transfer-Encoding: 8bit")
	w("")
	subject := e.Subject
	if subject == "" {
		subject = "(no subject)"
	}
	w("Your message %q could not be delivered to the following", subject)
	w("recipient(s):")
	w("")
	for _, r := range failed {
		w("  %s", r.Rcpt)
		w("    %s", oneLine(r.LastError))
		w("")
	}
	w("This is an automatically generated message from %s.", hostname)
	w("")
	w("--%s", boundary)
	w("Content-Type: message/delivery-status")
	w("")
	w("Reporting-MTA: dns; %s", hostname)
	w("Arrival-Date: %s", e.CreatedAt.Format(time.RFC1123Z))
	for _, r := range failed {
		status := "5.0.0"
		if m := statusCodeRe.FindStringSubmatch(r.LastError); m != nil && m[1][0] == '5' {
			status = m[1]
		} else if strings.Contains(r.LastError, "gave up after") {
			status = "4.4.7"
		}
		w("")
		w("Final-Recipient: rfc822; %s", r.Rcpt)
		w("Action: failed")
		w("Status: %s", status)
		if r.LastError != "" {
			w("Diagnostic-Code: smtp; %s", oneLine(r.LastError))
		}
		if !r.LastAttemptAt.IsZero() {
			w("Last-Attempt-Date: %s", r.LastAttemptAt.Format(time.RFC1123Z))
		}
	}
	w("")
	w("--%s", boundary)
	w("Content-Type: text/rfc822-headers")
	w("")
	b.Write(headerSection(e.Data))
	w("--%s--", boundary)
	return b.Bytes()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 900 {
		s = s[:900]
	}
	return s
}
