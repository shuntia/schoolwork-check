package canvas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/net/html"

	"schoolwork-check/internal/extract"
	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

// The extract package is reached through these indirections so that this
// package compiles and its tests run whether or not the extractor has been
// wired in yet, and so tests can stub extraction deterministically.
var (
	// extractorReady reports whether extract.Text is usable.
	extractorReady = func() bool { fn := extract.Text; return fn != nil }

	// extractText runs extract.Text.
	extractText = func(ctx context.Context, in extract.Input) (extract.Result, error) {
		fn := extract.Text
		if fn == nil {
			return extract.Result{}, errors.New("extractor unavailable")
		}
		return fn(ctx, in)
	}

	// htmlToTextHook converts an HTML fragment to plain text, falling back to
	// a local strip when extract.HTMLToText is unset.
	htmlToTextHook = func(s string) string {
		fn := extract.HTMLToText
		if fn == nil {
			return fallbackHTMLToText(s)
		}
		return fn(s)
	}
)

// fileRe matches the Canvas file routes that appear in assignment HTML:
// /files/123, /courses/7/files/123, /api/v1/files/123, each optionally
// followed by /download and a verifier query.
var fileRe = regexp.MustCompile(`(?:^|/)(?:api/v1/)?(?:courses/\d+/)?files/(\d+)(?:/|$)`)

// fileIDFrom returns the Canvas file id referenced by a URL, if any.
func fileIDFrom(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	p := raw
	if u, err := url.Parse(raw); err == nil {
		p = u.Path
	}
	p = strings.TrimSuffix(strings.TrimSuffix(p, "/"), "/download")
	m := fileRe.FindStringSubmatch(p + "/")
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// descLink is one anchor found in a description.
type descLink struct {
	href   string
	text   string
	fileID int64
}

// scanDescription walks the description HTML for anchors, resolving Canvas
// file references (href or data-api-endpoint) and plain links.
func scanDescription(htmlStr string) []descLink {
	var out []descLink
	seen := map[string]bool{}
	z := html.NewTokenizer(strings.NewReader(htmlStr))
	var (
		inA     bool
		cur     descLink
		curText strings.Builder
	)
	flush := func() {
		if !inA {
			return
		}
		inA = false
		cur.text = strings.TrimSpace(collapseSpace(curText.String()))
		key := cur.href
		if cur.fileID != 0 {
			key = "file:" + strconv.FormatInt(cur.fileID, 10)
		}
		if key != "" && !seen[key] {
			seen[key] = true
			out = append(out, cur)
		}
		cur, curText = descLink{}, strings.Builder{}
	}
	for {
		switch z.Next() {
		case html.ErrorToken:
			flush()
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "a" {
				continue
			}
			flush()
			inA = true
			cur = descLink{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				switch string(k) {
				case "href":
					cur.href = string(v)
				case "data-api-endpoint":
					if id, ok := fileIDFrom(string(v)); ok {
						cur.fileID = id
					}
				}
			}
			if cur.fileID == 0 {
				if id, ok := fileIDFrom(cur.href); ok {
					cur.fileID = id
				}
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "a" {
				flush()
			}
		case html.TextToken:
			if inA {
				curText.Write(z.Text())
			}
		}
	}
}

// descriptionAttachments turns the links in a description into attachments,
// resolving Canvas files through the files API and extracting text where the
// options allow it.
func (c *Client) descriptionAttachments(ctx context.Context, descHTML string, courseID int64, extracted, failures *atomic.Int64) []model.Attachment {
	links := scanDescription(descHTML)
	if len(links) == 0 {
		return nil
	}
	out := make([]model.Attachment, 0, len(links))
	for _, l := range links {
		if ctx.Err() != nil {
			return out
		}
		if l.fileID != 0 {
			meta, err := c.getFile(ctx, l.fileID)
			if err != nil {
				failures.Add(1)
				c.log.Warn("canvas: file metadata fetch failed", "file_id", l.fileID, "err", err)
				name := l.text
				if name == "" {
					name = "file " + strconv.FormatInt(l.fileID, 10)
				}
				out = append(out, model.Attachment{
					Name:         name,
					URL:          c.absURL(l.href),
					ContentError: "canvas: file metadata unavailable: " + err.Error(),
				})
				continue
			}
			att := c.attachmentFrom(ctx, meta, extracted, failures)
			if att.Name == "" {
				att.Name = l.text
			}
			out = append(out, att)
			continue
		}
		u := c.absURL(l.href)
		if !isLinkable(u) {
			continue
		}
		name := l.text
		if name == "" {
			name = u
		}
		out = append(out, model.Attachment{Name: name, URL: u, MimeType: "text/uri-list"})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isLinkable filters out anchors that are not real destinations.
func isLinkable(raw string) bool {
	if raw == "" || strings.HasPrefix(raw, "#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "":
		return u.Host != "" || u.Path != ""
	default:
		return false
	}
}

// absURL resolves a possibly relative Canvas URL against the instance base.
func (c *Client) absURL(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if strings.HasPrefix(raw, "/") {
		return c.baseURL + raw
	}
	return raw
}

// attachmentFrom builds a model.Attachment from Canvas file metadata,
// downloading and extracting its text when enabled.
func (c *Client) attachmentFrom(ctx context.Context, f apiAttachment, extracted, failures *atomic.Int64) model.Attachment {
	att := model.Attachment{
		Name:      f.name(),
		URL:       f.URL,
		MimeType:  f.ContentType,
		SizeBytes: f.Size,
	}
	if !c.opts.ExtractAttachments {
		return att
	}
	key, version := c.fileCacheKey(f)
	if e, ok := c.opts.FileCache.Get(key, version); ok {
		att.Content, att.ContentError, att.Truncated = e.Content, e.ContentError, e.Truncated
		return att
	}
	if c.extractInto(ctx, &att, extracted, failures) {
		c.opts.FileCache.Put(key, version, filecache.Entry{
			Content: att.Content, ContentError: att.ContentError, Truncated: att.Truncated})
	}
	return att
}

// fileCacheKey identifies a Canvas file and the version its extraction is
// valid for. Canvas bumps updated_at when a file is replaced; the extraction
// limits are part of the version because they change the result. No
// timestamp means no version, so the file is never cached.
func (c *Client) fileCacheKey(f apiAttachment) (key, version string) {
	key = "canvas:" + c.baseURL + ":file:" + f.ID.String()
	if f.ID.String() == "" || (f.UpdatedAt == "" && f.ModifiedAt == "") {
		return key, ""
	}
	return key, fmt.Sprintf("%s|%s|%d|%d|%d", f.UpdatedAt, f.ModifiedAt, f.Size,
		c.opts.MaxExtractedText, c.opts.MaxAttachmentBytes)
}

// extractInto downloads att.URL and fills Content/ContentError/Truncated.
// An attachment that cannot be read never fails its task. It reports
// whether the outcome is permanent for this file version (text, or an
// unsupported type) and so worth caching; download and parse failures are
// not.
func (c *Client) extractInto(ctx context.Context, att *model.Attachment, extracted, failures *atomic.Int64) bool {
	if !c.opts.ExtractAttachments || att.URL == "" || att.MimeType == "text/uri-list" {
		return false
	}
	if att.SizeBytes > 0 && att.SizeBytes > c.opts.MaxAttachmentBytes {
		att.ContentError = fmt.Sprintf("canvas: attachment too large (%d bytes > %d)", att.SizeBytes, c.opts.MaxAttachmentBytes)
		return false
	}
	if !extractorReady() {
		att.ContentError = "extractor unavailable"
		return false
	}
	resp, err := c.doRequest(ctx, att.URL)
	if err != nil {
		failures.Add(1)
		att.ContentError = "canvas: download failed: " + err.Error()
		c.log.Warn("canvas: attachment download failed", "name", att.Name, "err", err)
		return false
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, c.opts.MaxAttachmentBytes+1)

	res, err := extractText(ctx, extract.Input{
		Name:     att.Name,
		MimeType: att.MimeType,
		Reader:   body,
		Limit:    c.opts.MaxExtractedText,
	})
	if err != nil {
		var unsup *extract.UnsupportedError
		if errors.As(err, &unsup) {
			att.ContentError = unsup.Error()
			return true
		}
		failures.Add(1)
		att.ContentError = err.Error()
		c.log.Warn("canvas: attachment extraction failed", "name", att.Name, "err", err)
		return false
	}
	att.Content = res.Text
	att.Truncated = res.Truncated
	if res.Text != "" {
		extracted.Add(1)
	}
	return true
}

// getFile returns Canvas file metadata (name, type, size, download URL).
func (c *Client) getFile(ctx context.Context, fileID int64) (apiAttachment, error) {
	var f apiAttachment
	err := c.getJSON(ctx, c.url("/api/v1/files/"+strconv.FormatInt(fileID, 10), ""), &f)
	return f, err
}

// htmlToText converts description HTML to plain text through the extract
// package, falling back to a local strip so this package builds and tests on
// its own.
func htmlToText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return htmlToTextHook(s)
}

// blockTags end a line in the fallback converter.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "ul": true,
	"ol": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "table": true, "blockquote": true, "pre": true, "section": true,
}

// fallbackHTMLToText is a crude tag-strip used only when extract.HTMLToText is
// unset. It keeps paragraph and list breaks as newlines.
func fallbackHTMLToText(s string) string {
	var b strings.Builder
	skipDepth := 0
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return tidyText(b.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			n := string(name)
			if n == "script" || n == "style" {
				skipDepth++
				continue
			}
			if n == "li" {
				b.WriteString("\n- ")
				continue
			}
			if blockTags[n] {
				b.WriteString("\n")
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			n := string(name)
			if n == "script" || n == "style" {
				if skipDepth > 0 {
					skipDepth--
				}
				continue
			}
			if blockTags[n] {
				b.WriteString("\n")
			}
		case html.TextToken:
			if skipDepth == 0 {
				b.Write(z.Text())
			}
		}
	}
}

// tidyText collapses runs of whitespace without losing paragraph breaks.
func tidyText(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, ln := range lines {
		ln = strings.TrimSpace(collapseSpace(ln))
		if ln == "" {
			if len(out) == 0 || blank {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, ln)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// collapseSpace replaces runs of spaces, tabs and NBSP with a single space.
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == ' ' || r == '\r' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}
