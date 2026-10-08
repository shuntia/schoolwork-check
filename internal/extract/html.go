package extract

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlToText is the concrete implementation behind the package-level
// HTMLToText var. It is tolerant of fragments and of malformed markup, and
// never panics: on a parse failure it falls back to tag stripping.
func htmlToText(doc string) string {
	if strings.TrimSpace(doc) == "" {
		return ""
	}
	node, err := html.Parse(strings.NewReader(doc))
	if err != nil || node == nil {
		return tidyText(stripTags(doc))
	}
	r := &htmlRenderer{}
	r.walk(bodyOf(node))
	return tidyText(r.sb.String())
}

// bodyOf returns the <body> element of a parsed document, or the document
// itself when the parser produced something unusual.
func bodyOf(n *html.Node) *html.Node {
	var body *html.Node
	var find func(*html.Node)
	find = func(cur *html.Node) {
		if body != nil || cur == nil {
			return
		}
		if cur.Type == html.ElementNode && cur.DataAtom == atom.Body {
			body = cur
			return
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(n)
	if body != nil {
		return body
	}
	return n
}

type listState struct {
	ordered bool
	n       int
}

type htmlRenderer struct {
	sb    strings.Builder
	pre   int
	lists []listState
}

// skipped elements never contribute text.
var skippedElements = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Head:     true,
	atom.Title:    true,
	atom.Noscript: true,
	atom.Iframe:   true,
	atom.Object:   true,
	atom.Template: true,
	atom.Svg:      true,
}

// blockElements end the current line when they open and when they close.
var blockElements = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Li: true, atom.Ul: true,
	atom.Ol: true, atom.Dl: true, atom.Dt: true, atom.Dd: true,
	atom.Tr: true, atom.Table: true, atom.Thead: true, atom.Tbody: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true,
	atom.H5: true, atom.H6: true, atom.Blockquote: true, atom.Pre: true,
	atom.Section: true, atom.Article: true, atom.Header: true,
	atom.Footer: true, atom.Nav: true, atom.Aside: true, atom.Form: true,
	atom.Figure: true, atom.Figcaption: true, atom.Hr: true, atom.Main: true,
	atom.Address: true, atom.Fieldset: true, atom.Legend: true,
}

var trailingWS = regexp.MustCompile(`[ \t\f\v]+\n`)
var manyNewlines = regexp.MustCompile(`\n{3,}`)
var spaceRun = regexp.MustCompile(`[ \t\f\v\x{00a0}]+`)
var tagRun = regexp.MustCompile(`(?s)<[^>]*>`)

func (r *htmlRenderer) walk(n *html.Node) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.TextNode:
		r.writeText(n.Data)
		return
	case html.ElementNode:
		r.element(n)
		return
	case html.CommentNode, html.DoctypeNode:
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.walk(c)
	}
}

func (r *htmlRenderer) element(n *html.Node) {
	a := n.DataAtom
	if skippedElements[a] {
		return
	}

	switch a {
	case atom.Br:
		r.sb.WriteByte('\n')
		return
	case atom.Img:
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
			r.sb.WriteString("[image: " + alt + "]")
		}
		return
	case atom.A:
		r.anchor(n)
		return
	case atom.Ul, atom.Ol:
		r.newline()
		r.lists = append(r.lists, listState{ordered: a == atom.Ol})
		r.children(n)
		r.lists = r.lists[:len(r.lists)-1]
		r.newline()
		return
	case atom.Li:
		r.newline()
		r.sb.WriteString(r.bullet())
		r.children(n)
		r.newline()
		return
	case atom.Pre:
		r.newline()
		r.pre++
		r.children(n)
		r.pre--
		r.newline()
		return
	case atom.Td, atom.Th:
		r.children(n)
		r.sb.WriteByte('\t')
		return
	}

	block := blockElements[a]
	if block {
		r.newline()
	}
	r.children(n)
	if block {
		r.newline()
	}
}

func (r *htmlRenderer) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.walk(c)
	}
}

// anchor renders "text (href)" when the href adds information.
func (r *htmlRenderer) anchor(n *html.Node) {
	sub := &htmlRenderer{pre: r.pre}
	sub.children(n)
	text := strings.TrimSpace(tidyText(sub.sb.String()))

	href := strings.TrimSpace(attr(n, "href"))
	abs := isAbsoluteHTTP(href)

	switch {
	case text == "" && abs:
		r.sb.WriteString(href)
	case text == "":
		return
	case abs && !sameLink(text, href):
		r.sb.WriteString(text + " (" + href + ")")
	default:
		r.sb.WriteString(text)
	}
}

func (r *htmlRenderer) bullet() string {
	if len(r.lists) == 0 {
		return "- "
	}
	ls := &r.lists[len(r.lists)-1]
	if !ls.ordered {
		return "- "
	}
	ls.n++
	return fmt.Sprintf("%d. ", ls.n)
}

func (r *htmlRenderer) writeText(s string) {
	if s == "" {
		return
	}
	if r.pre > 0 {
		r.sb.WriteString(s)
		return
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", " ")
	s = spaceRun.ReplaceAllString(s, " ")
	// A space right after a line break is markup indentation, not content.
	if strings.HasPrefix(s, " ") && r.atLineStart() {
		s = strings.TrimLeft(s, " ")
	}
	if s == "" {
		return
	}
	r.sb.WriteString(s)
}

// atLineStart reports whether nothing has been written on the current line.
func (r *htmlRenderer) atLineStart() bool {
	s := r.sb.String()
	return s == "" || strings.HasSuffix(s, "\n")
}

// newline appends a newline unless the buffer already ends with one.
func (r *htmlRenderer) newline() {
	s := r.sb.String()
	if s == "" {
		return
	}
	if strings.HasSuffix(strings.TrimRight(s, " \t"), "\n") {
		return
	}
	r.sb.WriteByte('\n')
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func isAbsoluteHTTP(href string) bool {
	l := strings.ToLower(href)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

func sameLink(text, href string) bool {
	t := strings.TrimRight(strings.TrimSpace(text), "/")
	h := strings.TrimRight(strings.TrimSpace(href), "/")
	if strings.EqualFold(t, h) {
		return true
	}
	// "example.com/x" as the label for "https://example.com/x"
	for _, p := range []string{"https://", "http://"} {
		if strings.EqualFold(strings.TrimPrefix(strings.ToLower(h), p), strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// tidyText normalizes whitespace: trailing spaces gone, at most one blank line.
func tidyText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, " ", " ")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = trailingWS.ReplaceAllString(s, "\n")
	s = manyNewlines.ReplaceAllString(s, "\n\n")

	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// stripTags is the fallback when the parser itself fails.
func stripTags(s string) string {
	return html.UnescapeString(tagRun.ReplaceAllString(s, " "))
}
