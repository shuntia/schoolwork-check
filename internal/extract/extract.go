// Package extract turns attachment bytes into plain text.
//
// Contract used by source adapters:
//
//	text, err := extract.Text(ctx, extract.Input{Name, MimeType, Reader, Limit})
//
// Supported (implementations live in this package): PDF, DOCX, plain text
// (txt/md/csv/json), HTML. Google Docs/Sheets/Slides are exported to text by
// the Drive client in the classroom package before reaching this package.
package extract

import (
	"context"
	"io"
)

// Input describes one attachment to extract.
type Input struct {
	Name     string // filename, used for extension-based fallback
	MimeType string // may be empty
	Reader   io.Reader
	Limit    int // max bytes of output text; 0 means DefaultLimit
}

// Result is the extracted text and whether it hit Limit.
type Result struct {
	Text      string
	Truncated bool
}

// DefaultLimit is the default cap on extracted text, in bytes.
const DefaultLimit = 64 * 1024

// ErrUnsupported is returned when the type is not extractable (images, video, zip, ...).
type UnsupportedError struct{ MimeType, Name string }

func (e *UnsupportedError) Error() string {
	return "extract: unsupported type " + e.MimeType + " (" + e.Name + ")"
}

// Text dispatches on MimeType then filename extension.
// Implemented in text.go by the extraction agent.
var Text func(ctx context.Context, in Input) (Result, error)

// HTMLToText converts an HTML fragment (e.g. a Canvas assignment description)
// to readable plain text, preserving paragraphs, list items and link URLs.
// Implemented in html.go by the extraction agent.
var HTMLToText func(html string) string
