package email

import (
	"strings"
	"testing"
)

const sampleMultipart = "From: \"Alice Example\" <alice@example.org>\r\n" +
	"To: bob@example.com, \"Carol\" <carol@example.com>\r\n" +
	"Subject: =?UTF-8?Q?Caf=C3=A9_meeting?=\r\n" +
	"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
	"Message-ID: <abc123@example.org>\r\n" +
	"In-Reply-To: <parent@example.com>\r\n" +
	"References: <root@example.com> <parent@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
	"\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=iso-8859-1\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"Hello Bob, see you at the caf=E9.\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<html><head><style>p{color:red;background:url(http://tracker.example/x.png)}</style></head>" +
	"<body><p onclick=\"evil()\">Hello <b>Bob</b></p><script>alert(1)</script>" +
	"<img src=\"http://tracker.example/pixel.gif\"><img src=\"cid:logo@x\">" +
	"<a href=\"javascript:alert(1)\">bad</a><a href=\"https://example.org\">good</a></body></html>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: image/png\r\n" +
	"Content-ID: <logo@x>\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--outer--\r\n"

func TestParseMultipart(t *testing.T) {
	m := Parse([]byte(sampleMultipart))
	if m.Subject != "Café meeting" {
		t.Errorf("subject = %q", m.Subject)
	}
	if len(m.From) != 1 || m.From[0].Name != "Alice Example" || m.From[0].Address != "alice@example.org" {
		t.Errorf("from = %+v", m.From)
	}
	if len(m.To) != 2 || m.To[1].Name != "Carol" {
		t.Errorf("to = %+v", m.To)
	}
	if m.MessageID != "abc123@example.org" {
		t.Errorf("message-id = %q", m.MessageID)
	}
	if len(m.References) != 2 || m.InReplyTo[0] != "parent@example.com" {
		t.Errorf("refs = %v in-reply-to = %v", m.References, m.InReplyTo)
	}
	if !strings.Contains(m.Text, "café") {
		t.Errorf("text not decoded from latin1: %q", m.Text)
	}
	if !strings.Contains(m.HTML, "<b>Bob</b>") {
		t.Errorf("html = %q", m.HTML)
	}
	if len(m.Attachments) != 2 {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	if !m.Attachments[0].Inline || m.Attachments[0].PartID != "2" {
		t.Errorf("inline image = %+v", m.Attachments[0])
	}
	if a := m.Attachments[1]; a.Filename != "report.pdf" || a.PartID != "3" || a.Size != 9 || a.Inline {
		t.Errorf("pdf = %+v", a)
	}
	_, body, err := ExtractPart([]byte(sampleMultipart), "3")
	if err != nil || string(body) != "%PDF-1.4\n" {
		t.Errorf("extract = %q, %v", body, err)
	}
}

func TestRenderHTMLBlocksDangerousContent(t *testing.T) {
	m := Parse([]byte(sampleMultipart))
	r := RenderHTML(m.HTML, SanitizeOptions{CIDURL: func(cid string) string { return "/parts/" + cid }})
	doc := r.Document
	for _, bad := range []string{"onclick", "<script", "alert(1)", "javascript:", "tracker.example"} {
		if strings.Contains(doc, bad) {
			t.Errorf("document contains %q:\n%s", bad, doc)
		}
	}
	if !r.RemoteBlocked {
		t.Error("expected remote content to be reported as blocked")
	}
	if !strings.Contains(doc, `src="/parts/logo@x"`) {
		t.Errorf("cid image not rewritten:\n%s", doc)
	}
	if !strings.Contains(doc, `p{color:red;`) {
		t.Errorf("style block not preserved:\n%s", doc)
	}
	if !strings.Contains(doc, `rel="noopener noreferrer"`) && !strings.Contains(doc, `noreferrer`) {
		t.Errorf("links missing noreferrer:\n%s", doc)
	}

	allowed := RenderHTML(m.HTML, SanitizeOptions{AllowRemote: true})
	if !strings.Contains(allowed.Document, "http://tracker.example/pixel.gif") {
		t.Errorf("remote image missing when allowed:\n%s", allowed.Document)
	}
}

func TestQuoteDetection(t *testing.T) {
	r := RenderHTML(`<div>Thanks!</div><div class="gmail_quote">On Mon, Bob wrote:<blockquote>old</blockquote></div>`, SanitizeOptions{})
	if !r.HasQuote || !strings.Contains(r.Document, "tm-quote") {
		t.Errorf("quote not detected: %s", r.Document)
	}
	r = RenderHTML(`<blockquote type="cite">only a quote</blockquote>`, SanitizeOptions{})
	if r.HasQuote {
		t.Error("message consisting only of a quote should not be collapsed")
	}
	rt := RenderText("Sounds good\n\nOn Tue, Ann wrote:\n> earlier\n> text")
	if !rt.HasQuote {
		t.Errorf("plain-text quote not detected: %s", rt.Document)
	}
}

func TestBuildRoundTrip(t *testing.T) {
	o := &Outgoing{
		From:       Address{Name: "Zoë", Address: "zoe@example.com"},
		To:         []Address{{Address: "bob@example.org"}},
		Bcc:        []Address{{Address: "secret@example.org"}},
		Subject:    "Résumé attached",
		HTML:       "<p>Hi <b>Bob</b></p>",
		InReplyTo:  "parent@example.org",
		References: []string{"root@example.org", "parent@example.org"},
		Attachments: []OutgoingAttachment{
			{Filename: "notes.txt", ContentType: "text/plain", Data: []byte("hello")},
		},
	}
	raw, err := o.Build(false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret@example.org") {
		t.Error("Bcc leaked into delivered copy")
	}
	m := Parse(raw)
	if m.Subject != "Résumé attached" || m.From[0].Name != "Zoë" {
		t.Errorf("header round trip failed: %q %+v", m.Subject, m.From)
	}
	if !strings.Contains(m.Text, "Hi Bob") || !strings.Contains(m.HTML, "<b>Bob</b>") {
		t.Errorf("bodies: text=%q html=%q", m.Text, m.HTML)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "notes.txt" || m.Attachments[0].Size != 5 {
		t.Errorf("attachments = %+v", m.Attachments)
	}
	if m.InReplyTo[0] != "parent@example.org" || len(m.References) != 2 {
		t.Errorf("threading headers lost: %v %v", m.InReplyTo, m.References)
	}
	withBcc, _ := o.Build(true)
	if !strings.Contains(string(withBcc), "secret@example.org") {
		t.Error("Bcc missing from sender copy")
	}
	if !strings.Contains(string(raw), "\r\n") {
		t.Error("expected CRLF line endings")
	}
}

func TestHTMLToText(t *testing.T) {
	got := HTMLToText(`<p>Hello <b>world</b></p><ul><li>one</li><li>two</li></ul><p><a href="https://x.org/a">link</a></p><style>x{}</style>`)
	for _, want := range []string{"Hello world", "• one", "• two", "link (https://x.org/a)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "x{}") {
		t.Errorf("style content leaked: %q", got)
	}
}

func TestHTMLToTextKeepsSpacesAroundInlineTags(t *testing.T) {
	for in, want := range map[string]string{
		"meet on <b>Friday</b>&nbsp;to plan":     "meet on Friday to plan",
		"meet on&nbsp;<b>Friday</b> to plan":     "meet on Friday to plan",
		"meet on <b>Friday </b>to plan":          "meet on Friday to plan",
		"<i>a</i><b>b</b>":                       "ab",
		"caf\u00e9 <b>au</b>\u00a0lait":          "caf\u00e9 au lait",
		"<span>one</span>&nbsp;<span>two</span>": "one two",
	} {
		if got := HTMLToText(in); got != want {
			t.Errorf("HTMLToText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSnippet(t *testing.T) {
	s := MakeSnippet("Hi   there\n\n> quoted\nnext\u200c line", 100)
	if s != "Hi there next line" {
		t.Errorf("snippet = %q", s)
	}
}

func TestParseAddressList(t *testing.T) {
	list, err := ParseAddressList(`"Doe, John" <john@example.com>; jane@example.org`)
	if err != nil || len(list) != 2 || list[0].Name != "Doe, John" || list[1].Address != "jane@example.org" {
		t.Errorf("list = %+v err = %v", list, err)
	}
	if _, err := ParseAddressList("not-an-address"); err == nil {
		t.Error("expected error for invalid address")
	}
}
