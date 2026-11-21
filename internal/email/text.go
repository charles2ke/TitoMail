package email

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTMLToText converts an HTML document to readable plain text. It is used for
// text/plain alternatives, snippets and full-text indexing.
func HTMLToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return src
	}
	var w textWriter
	w.walk(doc)
	return w.String()
}

type textWriter struct {
	b         strings.Builder
	pendingNL int
	listDepth int
	quote     int
	pre       int
	lineStart bool
}

func (w *textWriter) String() string {
	return strings.TrimSpace(w.b.String())
}

func (w *textWriter) newline(n int) {
	if w.b.Len() == 0 {
		return
	}
	if n > w.pendingNL {
		w.pendingNL = n
	}
}

func (w *textWriter) write(s string) {
	if s == "" {
		return
	}
	if w.pendingNL > 0 {
		for i := 0; i < w.pendingNL; i++ {
			w.b.WriteString("\n")
		}
		w.pendingNL = 0
		w.lineStart = true
	}
	if w.lineStart && w.quote > 0 {
		w.b.WriteString(strings.Repeat("> ", w.quote))
	}
	w.lineStart = false
	w.b.WriteString(s)
}

func (w *textWriter) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		w.text(n.Data)
		return
	case html.ElementNode:
	case html.DocumentNode:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			w.walk(c)
		}
		return
	default:
		return
	}

	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Head, atom.Title, atom.Noscript, atom.Template:
		return
	case atom.Br:
		w.write("")
		w.b.WriteString("\n")
		w.lineStart = true
		return
	case atom.Hr:
		w.newline(1)
		w.write("----------")
		w.newline(1)
		return
	case atom.Img:
		if alt := attr(n, "alt"); strings.TrimSpace(alt) != "" {
			w.text(alt)
		}
		return
	}

	block := isBlock(n.DataAtom)
	if block {
		w.newline(1)
	}
	switch n.DataAtom {
	case atom.P, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Table:
		w.newline(2)
	case atom.Blockquote:
		w.newline(1)
		w.quote++
		defer func() { w.quote--; w.newline(1) }()
	case atom.Pre:
		w.pre++
		defer func() { w.pre-- }()
	case atom.Ul, atom.Ol:
		w.listDepth++
		defer func() { w.listDepth-- }()
	case atom.Li:
		w.newline(1)
		w.write(strings.Repeat("  ", max(w.listDepth-1, 0)) + "• ")
	case atom.Td, atom.Th:
		if n.PrevSibling != nil {
			w.write(" ")
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c)
	}

	if n.DataAtom == atom.A {
		href := strings.TrimSpace(attr(n, "href"))
		label := strings.TrimSpace(nodeText(n))
		if href != "" && strings.HasPrefix(href, "http") && href != label && !strings.Contains(label, href) {
			w.write(" (" + href + ")")
		}
	}
	if block {
		w.newline(1)
	}
	switch n.DataAtom {
	case atom.P, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Table:
		w.newline(2)
	}
}

func (w *textWriter) text(s string) {
	if w.pre > 0 {
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			if i > 0 {
				w.b.WriteString("\n")
				w.lineStart = true
			}
			w.write(l)
		}
		return
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		if s != "" && w.b.Len() > 0 && w.pendingNL == 0 && !w.lineStart {
			last := w.b.String()
			if !strings.HasSuffix(last, " ") && !strings.HasSuffix(last, "\n") {
				w.b.WriteString(" ")
			}
		}
		return
	}
	text := strings.Join(fields, " ")
	if startsWithSpace(s) && w.b.Len() > 0 && w.pendingNL == 0 && !w.lineStart {
		last := w.b.String()
		if !strings.HasSuffix(last, " ") && !strings.HasSuffix(last, "\n") {
			text = " " + text
		}
	}
	if endsWithSpace(s) {
		text += " "
	}
	w.write(text)
}

// startsWithSpace and endsWithSpace treat no-break spaces like the spaces
// strings.Fields drops: editors use them next to inline tags (as in
// "<b>bold</b>&nbsp;text"), and losing them would join the words.
func startsWithSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return s != "" && unicode.IsSpace(r)
}

func endsWithSpace(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return s != "" && unicode.IsSpace(r)
}

func isBlock(a atom.Atom) bool {
	switch a {
	case atom.Address, atom.Article, atom.Aside, atom.Blockquote, atom.Center, atom.Dd, atom.Div,
		atom.Dl, atom.Dt, atom.Fieldset, atom.Figcaption, atom.Figure, atom.Footer, atom.Form,
		atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Header, atom.Li, atom.Main,
		atom.Nav, atom.Ol, atom.P, atom.Pre, atom.Section, atom.Table, atom.Tr, atom.Ul, atom.Tbody,
		atom.Thead, atom.Tfoot, atom.Caption:
		return true
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return b.String()
}

// TextToHTML converts plain text to simple HTML, preserving line breaks and
// linkifying URLs.
func TextToHTML(text string) string {
	var b strings.Builder
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	quote := 0
	for i, line := range lines {
		depth := 0
		rest := line
		for strings.HasPrefix(rest, ">") {
			depth++
			rest = strings.TrimPrefix(strings.TrimPrefix(rest, ">"), " ")
		}
		for quote < depth {
			b.WriteString(`<blockquote>`)
			quote++
		}
		for quote > depth {
			b.WriteString(`</blockquote>`)
			quote--
		}
		b.WriteString(linkify(html.EscapeString(rest)))
		if i < len(lines)-1 {
			b.WriteString("<br>\n")
		}
	}
	for ; quote > 0; quote-- {
		b.WriteString(`</blockquote>`)
	}
	return b.String()
}

func linkify(escaped string) string {
	var b strings.Builder
	s := escaped
	for {
		i := indexURL(s)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		s = s[i:]
		end := strings.IndexAny(s, " \t<>\"'")
		if end < 0 {
			end = len(s)
		}
		u := strings.TrimRight(s[:end], ".,;:!?)")
		if strings.HasSuffix(u, "&gt") {
			u = strings.TrimSuffix(u, "&gt")
		}
		b.WriteString(`<a href="` + u + `">` + u + `</a>`)
		s = s[len(u):]
	}
	return b.String()
}

func indexURL(s string) int {
	a := strings.Index(s, "http://")
	b := strings.Index(s, "https://")
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}
