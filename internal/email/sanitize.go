package email

import (
	"html/template"
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// SanitizeOptions controls how untrusted message HTML is rendered.
type SanitizeOptions struct {
	// AllowRemote permits remote images, backgrounds and fonts. When false,
	// remote content is removed to protect the reader from tracking.
	AllowRemote bool
	// CIDURL resolves a Content-ID to a same-origin URL for inline images.
	// When nil, cid: references are removed.
	CIDURL func(cid string) string
}

// Rendered is a sanitized, self-contained HTML document ready to be shown in a
// sandboxed iframe (via srcdoc).
type Rendered struct {
	Document string `json:"document"`
	// RemoteBlocked reports whether remote content was removed.
	RemoteBlocked bool `json:"remote_blocked"`
	// HasQuote reports whether quoted text was found and collapsed.
	HasQuote bool `json:"has_quote"`
}

var (
	policyOnce sync.Once
	policy     *bluemonday.Policy
)

func htmlPolicy() *bluemonday.Policy {
	policyOnce.Do(func() {
		p := bluemonday.UGCPolicy()
		p.AllowElements("font", "center", "span", "div", "section", "header", "footer", "article",
			"main", "nav", "aside", "small", "big", "u", "s", "strike", "sub", "sup", "mark", "abbr",
			"address", "caption", "colgroup", "col", "thead", "tbody", "tfoot", "tr", "td", "th",
			"table", "hr", "br", "wbr", "picture", "figure", "figcaption", "blockquote", "pre", "code")
		p.AllowAttrs("style", "class", "id", "dir", "lang", "title", "align", "valign", "bgcolor",
			"background", "width", "height", "border", "nowrap").Globally()
		p.AllowAttrs("cellpadding", "cellspacing", "rules", "frame", "summary").OnElements("table")
		p.AllowAttrs("colspan", "rowspan", "headers", "scope", "abbr", "axis").OnElements("td", "th")
		p.AllowAttrs("span").OnElements("col", "colgroup")
		p.AllowAttrs("color", "face", "size").OnElements("font")
		p.AllowAttrs("type").OnElements("blockquote", "ol", "ul", "li")
		p.AllowAttrs("start", "reversed").OnElements("ol")
		p.AllowAttrs("src", "alt", "hspace", "vspace", "usemap").OnElements("img")
		p.AllowAttrs("href", "name", "target").OnElements("a")
		p.AllowURLSchemes("http", "https", "mailto", "tel", "cid")
		p.AllowDataURIImages()
		p.AllowRelativeURLs(false)
		p.RequireNoFollowOnLinks(false)
		p.RequireNoReferrerOnLinks(true)
		p.AddTargetBlankToFullyQualifiedLinks(true)
		p.RequireParseableURLs(true)
		policy = p
	})
	return policy
}

var (
	cssURL       = regexp.MustCompile(`(?i)url\(\s*(['"]?)(.*?)(['"]?)\s*\)`)
	cssImport    = regexp.MustCompile(`(?i)@import[^;]*;?`)
	cssDangerous = regexp.MustCompile(`(?i)(expression\s*\(|javascript:|vbscript:|-moz-binding|behavior\s*:)`)
	cssStyleEnd  = regexp.MustCompile(`(?i)</\s*style`)
	cssPosition  = regexp.MustCompile(`(?i)position\s*:\s*(fixed|absolute)`)
)

// RenderHTML sanitizes untrusted message HTML and wraps it in a standalone
// document with a restrictive Content-Security-Policy.
func RenderHTML(src string, opts SanitizeOptions) Rendered {
	var css strings.Builder
	body := src
	if doc, err := html.Parse(strings.NewReader(src)); err == nil {
		// Pull out <style> blocks; bluemonday would discard them.
		var styles []*html.Node
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode && n.DataAtom == atom.Style {
				styles = append(styles, n)
				return
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)
		for _, s := range styles {
			css.WriteString(nodeText(s))
			css.WriteString("\n")
			s.Parent.RemoveChild(s)
		}
		var b strings.Builder
		if bodyNode := findElement(doc, atom.Body); bodyNode != nil {
			for c := bodyNode.FirstChild; c != nil; c = c.NextSibling {
				_ = html.Render(&b, c)
			}
		} else {
			_ = html.Render(&b, doc)
		}
		body = b.String()
	}

	clean := htmlPolicy().Sanitize(body)
	var r Rendered
	clean, r.RemoteBlocked, r.HasQuote = postProcess(clean, opts)
	sanitizedCSS, cssBlocked := sanitizeCSS(css.String(), opts)
	r.RemoteBlocked = r.RemoteBlocked || cssBlocked
	r.Document = wrapDocument(clean, sanitizedCSS, opts.AllowRemote)
	return r
}

// RenderText renders a plain-text body as a sanitized document.
func RenderText(text string) Rendered {
	body := `<div class="tm-plain">` + TextToHTML(text) + `</div>`
	clean, _, hasQuote := postProcess(htmlPolicy().Sanitize(body), SanitizeOptions{})
	return Rendered{Document: wrapDocument(clean, "", false), HasQuote: hasQuote}
}

const baseCSS = `html,body{margin:0;padding:0;background:#fff;color:#202124}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;font-size:14px;line-height:1.5;word-wrap:break-word;overflow-wrap:anywhere;padding:2px}
img{max-width:100%;height:auto}
table{max-width:100%}
pre{white-space:pre-wrap}
blockquote{margin:0 0 0 .8ex;border-left:2px solid #c6c9ce;padding-left:1ex;color:#5f6368}
a{color:#1a73e8}
.tm-plain{white-space:normal}
.tm-quote{display:none}
body.tm-show-quotes .tm-quote{display:block}
img[data-tm-blocked]{display:none}`

func wrapDocument(body, css string, allowRemote bool) string {
	imgSrc := "'self' data:"
	fontSrc := "data:"
	if allowRemote {
		imgSrc += " https: http:"
		fontSrc += " https:"
	}
	csp := "default-src 'none'; img-src " + imgSrc + "; style-src 'unsafe-inline'; font-src " + fontSrc + "; form-action 'none'"
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8">`)
	b.WriteString(`<meta http-equiv="Content-Security-Policy" content="` + template.HTMLEscapeString(csp) + `">`)
	b.WriteString(`<meta name="referrer" content="no-referrer"><base target="_blank">`)
	b.WriteString(`<style>` + baseCSS + `</style>`)
	if strings.TrimSpace(css) != "" {
		b.WriteString(`<style>` + css + `</style>`)
	}
	b.WriteString(`</head><body>`)
	b.WriteString(body)
	b.WriteString(`</body></html>`)
	return b.String()
}

// sanitizeCSS neutralizes dangerous constructs in <style> content and handles
// url() references according to opts.
func sanitizeCSS(css string, opts SanitizeOptions) (string, bool) {
	if strings.TrimSpace(css) == "" {
		return "", false
	}
	css = cssStyleEnd.ReplaceAllString(css, "")
	css = cssImport.ReplaceAllString(css, "")
	css = cssDangerous.ReplaceAllString(css, "blocked(")
	blocked := false
	css = cssURL.ReplaceAllStringFunc(css, func(m string) string {
		sub := cssURL.FindStringSubmatch(m)
		u, ok, remote := rewriteURL(strings.TrimSpace(sub[2]), opts)
		if remote && !ok {
			blocked = true
		}
		if !ok {
			return "none"
		}
		return `url("` + strings.ReplaceAll(u, `"`, `%22`) + `")`
	})
	return css, blocked
}

// rewriteURL decides what to do with a resource URL. It returns the URL to
// use, whether it is allowed, and whether it was a remote URL.
func rewriteURL(u string, opts SanitizeOptions) (string, bool, bool) {
	lower := strings.ToLower(u)
	switch {
	case strings.HasPrefix(lower, "cid:"):
		if opts.CIDURL == nil {
			return "", false, false
		}
		target := opts.CIDURL(strings.Trim(u[4:], "<>"))
		return target, target != "", false
	case strings.HasPrefix(lower, "data:image/"):
		if strings.Contains(lower, "svg") {
			return "", false, false
		}
		return u, true, false
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "//"):
		return u, opts.AllowRemote, true
	}
	return "", false, false
}

func postProcess(fragment string, opts SanitizeOptions) (string, bool, bool) {
	ctx := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(fragment), ctx)
	if err != nil {
		return fragment, false, false
	}
	root := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	blocked := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for i := 0; i < len(n.Attr); i++ {
				a := &n.Attr[i]
				switch {
				case a.Key == "src" && n.DataAtom == atom.Img, a.Key == "background":
					u, ok, remote := rewriteURL(strings.TrimSpace(a.Val), opts)
					if remote && !ok {
						blocked = true
					}
					if ok {
						a.Val = u
					} else {
						n.Attr = append(n.Attr[:i], n.Attr[i+1:]...)
						i--
						if n.DataAtom == atom.Img && remote {
							n.Attr = append(n.Attr, html.Attribute{Key: "data-tm-blocked", Val: "1"})
						}
					}
				case a.Key == "style":
					a.Val = cssDangerous.ReplaceAllString(a.Val, "blocked(")
					a.Val = cssURL.ReplaceAllStringFunc(a.Val, func(m string) string {
						sub := cssURL.FindStringSubmatch(m)
						u, ok, remote := rewriteURL(strings.TrimSpace(sub[2]), opts)
						if remote && !ok {
							blocked = true
						}
						if !ok {
							return "none"
						}
						return `url("` + strings.ReplaceAll(u, `"`, `%22`) + `")`
					})
					// Prevent content from escaping its box over the UI chrome.
					lower := strings.ToLower(a.Val)
					if strings.Contains(lower, "position") && (strings.Contains(lower, "fixed") || strings.Contains(lower, "absolute")) {
						a.Val = cssPosition.ReplaceAllString(a.Val, "position:static")
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	hasQuote := markQuote(root)

	var b strings.Builder
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		_ = html.Render(&b, c)
	}
	return b.String(), blocked, hasQuote
}

// markQuote finds the quoted part of a reply (Gmail, Outlook, Thunderbird,
// Apple Mail and TitoMail conventions) and marks it as collapsible.
func markQuote(root *html.Node) bool {
	var found *html.Node
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode && isQuoteNode(n) && hasTextBefore(root, n) {
			found = n
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(root)
	if found == nil {
		return false
	}
	addClass(found, "tm-quote")
	// Outlook puts the quoted body in siblings following the header block.
	// Thunderbird puts an attribution line before the blockquote.
	if attr(found, "id") == "divRplyFwdMsg" || hasClass(found, "moz-cite-prefix") {
		for s := found.NextSibling; s != nil; s = s.NextSibling {
			if s.Type == html.ElementNode {
				addClass(s, "tm-quote")
			} else if s.Type == html.TextNode && strings.TrimSpace(s.Data) != "" {
				wrapper := &html.Node{Type: html.ElementNode, Data: "span", DataAtom: atom.Span}
				s.Parent.InsertBefore(wrapper, s)
				s.Parent.RemoveChild(s)
				wrapper.AppendChild(s)
				addClass(wrapper, "tm-quote")
				s = wrapper
			}
		}
	}
	return true
}

func isQuoteNode(n *html.Node) bool {
	if hasClass(n, "gmail_quote") || hasClass(n, "tm_quote") || hasClass(n, "yahoo_quoted") ||
		hasClass(n, "moz-cite-prefix") || attr(n, "id") == "divRplyFwdMsg" {
		return true
	}
	if n.DataAtom == atom.Blockquote {
		if attr(n, "type") == "cite" {
			return true
		}
		// Plain-text quotes converted by TextToHTML sit directly in tm-plain.
		if n.Parent != nil && hasClass(n.Parent, "tm-plain") && n.NextSibling == nil {
			return true
		}
	}
	return false
}

func hasTextBefore(root, target *html.Node) bool {
	found := false
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n == target {
			return true
		}
		if n.Type == html.TextNode && strings.TrimSpace(n.Data) != "" {
			found = true
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Img {
			found = true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(root)
	return found
}

func hasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

func addClass(n *html.Node, class string) {
	for i, a := range n.Attr {
		if a.Key == "class" {
			n.Attr[i].Val = strings.TrimSpace(a.Val + " " + class)
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: "class", Val: class})
}

func findElement(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findElement(c, a); f != nil {
			return f
		}
	}
	return nil
}

// SanitizeComposedHTML cleans HTML written in the web composer before it is
// sent. It is less permissive than display sanitizing but keeps formatting.
func SanitizeComposedHTML(src string) string {
	return htmlPolicy().Sanitize(src)
}

// BodyHTML returns the inner HTML of the <body> element of a document, which
// is how a saved draft is loaded back into the composer.
func BodyHTML(doc string) string {
	n, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return doc
	}
	body := findElement(n, atom.Body)
	if body == nil {
		return doc
	}
	var b strings.Builder
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		_ = html.Render(&b, c)
	}
	return b.String()
}

// QuoteHTML prepares a message body for quoting in a reply or forward. The
// result is sanitized, and images other than data: URLs are removed so that
// opening the composer never loads remote (tracking) content or refers to
// MIME parts of the original message.
func QuoteHTML(src string) string {
	body := src
	if strings.Contains(strings.ToLower(src), "<body") {
		body = BodyHTML(src)
	}
	clean := htmlPolicy().Sanitize(body)
	ctx := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(clean), ctx)
	if err != nil {
		return clean
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Type == html.ElementNode && c.DataAtom == atom.Img {
				keep := false
				for _, at := range c.Attr {
					v := strings.ToLower(strings.TrimSpace(at.Val))
					if at.Key == "src" && strings.HasPrefix(v, "data:image/") && !strings.Contains(v, "svg") {
						keep = true
					}
				}
				if !keep {
					n.RemoveChild(c)
				}
			} else {
				walk(c)
			}
			c = next
		}
	}
	root := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	for _, n := range nodes {
		if n.Parent != nil {
			n.Parent.RemoveChild(n)
		}
		root.AppendChild(n)
	}
	walk(root)
	var b strings.Builder
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		_ = html.Render(&b, c)
	}
	return b.String()
}
