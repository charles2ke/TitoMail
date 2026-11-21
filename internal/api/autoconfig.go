package api

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/charles2ke/TitoMail/internal/email"
)

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

type serverEntry struct {
	kind   string // imap or smtp
	port   int
	socket string // SSL, STARTTLS or plain
}

func (a *API) clientServers() []serverEntry {
	tlsSocket := func(implicit bool) string {
		switch {
		case !a.opts.TLS:
			return "plain"
		case implicit:
			return "SSL"
		}
		return "STARTTLS"
	}
	var out []serverEntry
	add := func(kind string, port int, implicit bool) {
		if port > 0 && (a.opts.TLS || !implicit) {
			out = append(out, serverEntry{kind, port, tlsSocket(implicit)})
		}
	}
	add("imap", a.opts.IMAPSPort, true)
	add("imap", a.opts.IMAPPort, false)
	add("smtp", a.opts.SubmissionsPort, true)
	add("smtp", a.opts.SubmissionPort, false)
	return out
}

// autoconfig serves the Thunderbird autoconfiguration file
// (https://wiki.mozilla.org/Thunderbird:Autoconfiguration), which is also
// used by K-9 Mail / Thunderbird for Android, Evolution and others.
func (a *API) autoconfig(w http.ResponseWriter, r *http.Request) {
	domain := email.DomainOf(email.NormalizeAddress(r.URL.Query().Get("emailaddress")))
	if domain == "" {
		host := strings.ToLower(r.Host)
		if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
			host = host[:i]
		}
		domain = strings.TrimPrefix(host, "autoconfig.")
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<clientConfig version="1.1">` + "\n")
	fmt.Fprintf(&b, "  <emailProvider id=\"%s\">\n    <domain>%s</domain>\n", xmlEscape(domain), xmlEscape(domain))
	fmt.Fprintf(&b, "    <displayName>%s</displayName>\n    <displayShortName>%s</displayShortName>\n", xmlEscape(domain+" (TitoMail)"), xmlEscape(domain))
	for _, s := range a.clientServers() {
		tag := "incomingServer"
		if s.kind == "smtp" {
			tag = "outgoingServer"
		}
		fmt.Fprintf(&b, "    <%s type=\"%s\">\n      <hostname>%s</hostname>\n      <port>%d</port>\n      <socketType>%s</socketType>\n"+
			"      <authentication>password-cleartext</authentication>\n      <username>%%EMAILADDRESS%%</username>\n    </%s>\n",
			tag, s.kind, xmlEscape(a.opts.Hostname), s.port, s.socket, tag)
	}
	if a.opts.PublicURL != "" {
		fmt.Fprintf(&b, "    <documentation url=\"%s\"><descr lang=\"en\">Webmail</descr></documentation>\n", xmlEscape(a.opts.PublicURL))
	}
	b.WriteString("  </emailProvider>\n</clientConfig>\n")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// autodiscover answers Outlook's "plain old XML" Autodiscover request with
// IMAP and SMTP settings.
func (a *API) autodiscover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Request struct {
			EMailAddress string `xml:"EMailAddress"`
		} `xml:"Request"`
	}
	_ = xml.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	addr := email.NormalizeAddress(req.Request.EMailAddress)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<Autodiscover xmlns="http://schemas.microsoft.com/exchange/autodiscover/responseschema/2006">` + "\n")
	b.WriteString(`  <Response xmlns="http://schemas.microsoft.com/exchange/autodiscover/outlook/responseschema/2006a">` + "\n")
	b.WriteString("    <Account>\n      <AccountType>email</AccountType>\n      <Action>settings</Action>\n")
	done := map[string]bool{}
	for _, s := range a.clientServers() {
		if done[s.kind] {
			continue // Outlook only uses the first entry of each type.
		}
		done[s.kind] = true
		ssl := "off"
		if s.socket != "plain" {
			ssl = "on"
		}
		fmt.Fprintf(&b, "      <Protocol>\n        <Type>%s</Type>\n        <Server>%s</Server>\n        <Port>%d</Port>\n"+
			"        <LoginName>%s</LoginName>\n        <DomainRequired>off</DomainRequired>\n        <SPA>off</SPA>\n"+
			"        <SSL>%s</SSL>\n", strings.ToUpper(s.kind), xmlEscape(a.opts.Hostname), s.port, xmlEscape(addr), ssl)
		if s.socket == "STARTTLS" {
			b.WriteString("        <Encryption>TLS</Encryption>\n")
		} else if s.socket == "SSL" {
			b.WriteString("        <Encryption>SSL</Encryption>\n")
		}
		b.WriteString("        <AuthRequired>on</AuthRequired>\n      </Protocol>\n")
	}
	b.WriteString("    </Account>\n  </Response>\n</Autodiscover>\n")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}
