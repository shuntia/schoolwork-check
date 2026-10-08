package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func init() {
	Text = extractText
	HTMLToText = htmlToText
}

// format is the internal notion of "how do we read these bytes".
type format int

const (
	formatUnknown format = iota
	formatText
	formatHTML
	formatPDF
	formatDOCX
	formatPPTX
)

// How much raw input we are willing to read relative to the output limit.
// Markup is far less dense than the text we get out of it, so HTML gets more.
const (
	textReadFactor = 4
	htmlReadFactor = 8
	maxReadBytes   = 64 << 20 // absolute ceiling for a single attachment
)

var mimeFormats = map[string]format{
	"application/pdf":   formatPDF,
	"application/x-pdf": formatPDF,

	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   formatDOCX,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": formatPPTX,

	"text/html":             formatHTML,
	"application/xhtml+xml": formatHTML,

	"application/json":          formatText,
	"application/x-ndjson":      formatText,
	"application/javascript":    formatText,
	"application/x-javascript":  formatText,
	"application/typescript":    formatText,
	"application/x-sh":          formatText,
	"application/x-shellscript": formatText,
	"application/x-latex":       formatText,
	"application/x-tex":         formatText,
	"application/rtf":           formatText,
	"application/x-yaml":        formatText,
	"application/yaml":          formatText,
	"application/xml":           formatText,
}

var extFormats = map[string]format{
	".pdf":   formatPDF,
	".docx":  formatDOCX,
	".pptx":  formatPPTX,
	".html":  formatHTML,
	".htm":   formatHTML,
	".xhtml": formatHTML,

	".txt": formatText, ".md": formatText, ".markdown": formatText,
	".csv": formatText, ".tsv": formatText, ".json": formatText,
	".log": formatText, ".tex": formatText, ".rtf": formatText,
	".xml": formatText, ".yaml": formatText, ".yml": formatText,
	".py": formatText, ".java": formatText, ".c": formatText,
	".h": formatText, ".cpp": formatText, ".hpp": formatText,
	".cs": formatText, ".js": formatText, ".ts": formatText,
	".tsx": formatText, ".jsx": formatText, ".go": formatText,
	".rs": formatText, ".rb": formatText, ".php": formatText,
	".sh": formatText, ".sql": formatText, ".css": formatText,
	".r": formatText, ".m": formatText, ".kt": formatText,
	".swift": formatText, ".scala": formatText, ".pl": formatText,
	".lua": formatText, ".asm": formatText, ".s": formatText,
}

// normalizeMIME strips parameters and lowercases: "Text/Plain; charset=utf-8" -> "text/plain".
func normalizeMIME(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(s); err == nil {
		return strings.ToLower(strings.TrimSpace(mt))
	}
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func formatForMIME(mt string) format {
	if mt == "" {
		return formatUnknown
	}
	if f, ok := mimeFormats[mt]; ok {
		return f
	}
	switch {
	case strings.HasPrefix(mt, "text/"):
		// text/plain, text/markdown, text/csv, text/x-python, ...
		return formatText
	case strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return formatText
	}
	return formatUnknown
}

func formatForExt(ext string) format {
	if ext == "" {
		return formatUnknown
	}
	return extFormats[strings.ToLower(ext)]
}

// classify picks a format from the MIME type first, then the filename extension.
func classify(mt, ext string) format {
	if f := formatForMIME(mt); f != formatUnknown {
		return f
	}
	return formatForExt(ext)
}

// extractText is the concrete implementation behind the package-level Text var.
func extractText(ctx context.Context, in Input) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	limit := in.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}

	mt := normalizeMIME(in.MimeType)
	ext := strings.ToLower(filepath.Ext(in.Name))
	f := classify(mt, ext)
	if f == formatUnknown {
		return Result{}, &UnsupportedError{MimeType: in.MimeType, Name: in.Name}
	}
	if in.Reader == nil {
		return Result{}, errors.New("extract: nil reader")
	}

	var (
		text string
		err  error
	)
	switch f {
	case formatText:
		var raw []byte
		if raw, err = readAtMost(in.Reader, readBudget(limit, textReadFactor)); err == nil {
			text = cleanText(string(raw))
		}
	case formatHTML:
		var raw []byte
		if raw, err = readAtMost(in.Reader, readBudget(limit, htmlReadFactor)); err == nil {
			text = htmlToText(string(raw))
		}
	case formatPDF:
		var raw []byte
		if raw, err = readAll(in.Reader); err == nil {
			text, err = pdfText(ctx, raw)
		}
	case formatDOCX:
		var raw []byte
		if raw, err = readAll(in.Reader); err == nil {
			text, err = docxText(ctx, raw)
		}
	case formatPPTX:
		var raw []byte
		if raw, err = readAll(in.Reader); err == nil {
			text, err = pptxText(ctx, raw)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	out, truncated := cutRunes(cleanText(text), limit)
	return Result{Text: out, Truncated: truncated}, nil
}

func readBudget(limit, factor int) int {
	n := limit * factor
	if n <= 0 || n > maxReadBytes {
		return maxReadBytes
	}
	return n
}

// readAtMost reads at most n bytes, plus one so callers could notice overflow.
func readAtMost(r io.Reader, n int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(n)+1))
	if err != nil {
		return nil, fmt.Errorf("extract: read: %w", err)
	}
	return b, nil
}

// readAll buffers the whole input: the PDF and zip parsers need random access.
func readAll(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxReadBytes))
	if err != nil {
		return nil, fmt.Errorf("extract: read: %w", err)
	}
	return b, nil
}

// cleanText drops NULs and invalid UTF-8, normalizes newlines and trims
// trailing whitespace on every line.
func cleanText(s string) string {
	if s == "" {
		return ""
	}
	if strings.ContainsRune(s, 0) {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\ufeff", "")

	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t\v\f")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// cutRunes truncates s to at most limit bytes without splitting a rune.
func cutRunes(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	i := limit
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i], true
}
