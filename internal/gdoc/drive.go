// drive.go fetches a document's text from Drive. It is the only part of this
// package that talks to Google, and it sits behind docSource so the parsing
// above it is testable without the network.

package gdoc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"schoolwork-check/internal/classroom"
	"schoolwork-check/internal/extract"
)

// Google-native Drive types this package knows how to read.
const (
	mimeGoogleAppsPrefix = "application/vnd.google-apps."
	mimeGoogleDoc        = "application/vnd.google-apps.document"
	mimeGoogleSheet      = "application/vnd.google-apps.spreadsheet"
	mimeGoogleSlides     = "application/vnd.google-apps.presentation"
)

// exportFormats is what each Google-native type is exported as, best first.
// Markdown keeps a Doc's tables as rows of cells, which is exactly the shape
// a calendar is in; plain text flattens every cell onto its own line, so it
// is only the fallback for the older export backends that refuse markdown.
var exportFormats = map[string][]string{
	mimeGoogleDoc:    {"text/markdown", "text/plain"},
	mimeGoogleSheet:  {"text/csv"},
	mimeGoogleSlides: {"text/plain"},
}

// doc is one document read out of Drive.
type doc struct {
	ID       string
	Name     string
	MimeType string
	URL      string
	// Version changes whenever the document does; it keys the caches.
	Version   string
	Text      string
	Truncated bool

	// size is the uploaded file's size, kept for the download limit.
	size int64
}

// docSource reads a document from Drive in two steps, so that a calendar
// whose parse is still fresh costs one cheap metadata call instead of
// downloading eight pages nobody is going to read. Tests fake it.
type docSource interface {
	// Meta returns the document's identity and revision, without its text.
	Meta(ctx context.Context, id string) (doc, error)
	// Fill exports or downloads the text into d.Text.
	Fill(ctx context.Context, d *doc) error
}

type driveSource struct {
	drv      *drive.Service
	maxText  int
	maxBytes int64
	log      *slog.Logger
}

// newDriveSource builds a Drive client from the token google-login cached.
// The drive.readonly scope that Classroom attachments already need covers
// every document the student can open, so no second consent is required.
func newDriveSource(ctx context.Context, credentialsFile, tokenFile string, maxText int, maxBytes int64, log *slog.Logger) (*driveSource, error) {
	hc, err := classroom.HTTPClient(ctx, credentialsFile, tokenFile, log)
	if err != nil {
		return nil, err
	}
	drv, err := drive.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("gdoc: building Drive client: %w", err)
	}
	return &driveSource{drv: drv, maxText: maxText, maxBytes: maxBytes, log: log}, nil
}

func (s *driveSource) Meta(ctx context.Context, id string) (doc, error) {
	meta, err := s.drv.Files.Get(id).
		Fields("id,name,mimeType,size,version,modifiedTime,md5Checksum,webViewLink").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return doc{}, fmt.Errorf("reading its Drive metadata: %w", hint(err))
	}
	d := doc{
		ID:       id,
		Name:     meta.Name,
		MimeType: meta.MimeType,
		URL:      meta.WebViewLink,
		Version:  fmt.Sprintf("%d|%s|%s|%d", meta.Version, meta.ModifiedTime, meta.Md5Checksum, s.limit()),
	}
	if d.URL == "" {
		d.URL = "https://drive.google.com/open?id=" + id
	}
	d.size = meta.Size
	return d, nil
}

func (s *driveSource) Fill(ctx context.Context, d *doc) error {
	var err error
	switch {
	case len(exportFormats[d.MimeType]) > 0:
		d.Text, d.Truncated, err = s.export(ctx, d.ID, exportFormats[d.MimeType])
	case strings.HasPrefix(d.MimeType, mimeGoogleAppsPrefix):
		err = fmt.Errorf("a %s cannot be exported as text", strings.TrimPrefix(d.MimeType, mimeGoogleAppsPrefix))
	default:
		d.Text, d.Truncated, err = s.download(ctx, d)
	}
	return err
}

// export downloads a Google-native file as text, trying each format in turn:
// a backend that refuses one (400) may well serve the next.
//
// The download limit is deliberately not the text limit. A Doc's Markdown
// export inlines every image as base64 — a calendar with a dozen film stills
// in it arrives as 689 KB of which 14 KB is words — so the blobs are stripped
// first and the text limit applies to what is left. Capping the download
// instead would throw away the second half of the calendar to make room for
// pictures of Casablanca.
func (s *driveSource) export(ctx context.Context, id string, formats []string) (string, bool, error) {
	var lastErr error
	for _, mime := range formats {
		res, err := s.drv.Files.Export(id, mime).Context(ctx).Download()
		if err != nil {
			lastErr = err
			s.log.Debug("gdoc: export refused", "doc", id, "format", mime, "err", err)
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(res.Body, maxRawExport))
		res.Body.Close()
		if err != nil {
			return "", false, fmt.Errorf("downloading its %s export: %w", mime, err)
		}
		text := stripImageData(string(raw))
		s.log.Debug("gdoc: exported", "doc", id, "format", mime, "raw_bytes", len(raw), "text_bytes", len(text))
		return clipText(text, s.limit())
	}
	return "", false, fmt.Errorf("exporting it as text: %w", hint(lastErr))
}

// maxRawExport bounds the download itself, before the images are stripped.
const maxRawExport = 64 << 20

// imageDefinition is a Markdown reference definition holding an inline image:
// "[image3]: <data:image/png;base64,iVBOR...>". They sit at the end of a
// Docs export and are the bulk of it.
var imageDefinition = regexp.MustCompile(`^\[[^\]]+\]:\s*<?data:`)

// inlineDataURI is the same blob written inline.
var inlineDataURI = regexp.MustCompile(`data:[a-zA-Z0-9.+-]+/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]{100,}`)

// stripImageData removes embedded image data, keeping every line that has
// words in it.
func stripImageData(text string) string {
	if !strings.Contains(text, ";base64,") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) / 4)
	for _, line := range strings.SplitAfter(text, "\n") {
		if imageDefinition.MatchString(line) {
			continue
		}
		b.WriteString(inlineDataURI.ReplaceAllString(line, "(image)"))
	}
	return b.String()
}

// clipText cuts text to the limit on a rune boundary.
func clipText(s string, limit int) (string, bool, error) {
	if limit <= 0 || len(s) <= limit {
		return s, false, nil
	}
	for limit > 0 && s[limit]&0xC0 == 0x80 {
		limit--
	}
	return s[:limit], true, nil
}

// download fetches an uploaded file (PDF, DOCX, ...) and extracts its text.
func (s *driveSource) download(ctx context.Context, d *doc) (string, bool, error) {
	if s.maxBytes > 0 && d.size > s.maxBytes {
		return "", false, fmt.Errorf("file too large: %d bytes > limit %d", d.size, s.maxBytes)
	}
	if extract.Text == nil {
		return "", false, errors.New("extractor unavailable")
	}
	resp, err := s.drv.Files.Get(d.ID).SupportsAllDrives(true).Context(ctx).Download()
	if err != nil {
		return "", false, fmt.Errorf("downloading it: %w", hint(err))
	}
	defer resp.Body.Close()

	var body io.Reader = resp.Body
	if s.maxBytes > 0 {
		body = io.LimitReader(body, s.maxBytes)
	}
	out, err := extract.Text(ctx, extract.Input{
		Name:     d.Name,
		MimeType: d.MimeType,
		Reader:   body,
		Limit:    s.limit(),
	})
	if err != nil {
		return "", false, fmt.Errorf("extracting its text: %w", err)
	}
	return out.Text, out.Truncated, nil
}

// DefaultMaxText is how much of a calendar is read. Whole semesters of rows
// run well past the per-attachment cap, and a calendar cut off in October is
// worse than useless.
const DefaultMaxText = 512 << 10

func (s *driveSource) limit() int {
	if s.maxText > 0 {
		return s.maxText
	}
	return DefaultMaxText
}

// hint turns Drive's 403/404 on a document the student has never opened into
// the one sentence that fixes it.
func hint(err error) error {
	var ae *googleapi.Error
	if errors.As(err, &ae) && (ae.Code == 403 || ae.Code == 404) {
		return fmt.Errorf("%w — open the document once in the browser with the "+
			"account you ran google-login as, or ask the teacher for access", err)
	}
	return err
}
