package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
)

// errEncryptedPDF is the canonical error for a PDF we cannot read.
var errEncryptedPDF = errors.New("pdf: encrypted or unreadable")

// pdfText extracts text from a PDF held entirely in memory.
// The parser needs io.ReaderAt, so the bytes must already be buffered.
//
// github.com/ledongthuc/pdf panics on some malformed files; every call into it
// is wrapped in a recover so a bad attachment can never take down the process.
func pdfText(ctx context.Context, data []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text = ""
			err = fmt.Errorf("pdf: cannot parse document: %v", r)
		}
	}()

	if len(data) == 0 {
		return "", errors.New("pdf: empty document")
	}

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if isEncryptedErr(err) {
			return "", errEncryptedPDF
		}
		return "", fmt.Errorf("pdf: %w", err)
	}
	if r == nil {
		return "", errEncryptedPDF
	}

	numPages := r.NumPage()
	if numPages <= 0 {
		return "", nil
	}

	var (
		out      strings.Builder
		pageErrs int
	)
	for i := 1; i <= numPages; i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		page, perr := pdfPageText(r, i)
		if perr != nil {
			pageErrs++
			continue
		}
		if page == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(page)
	}
	if out.Len() == 0 && pageErrs > 0 {
		return "", errEncryptedPDF
	}
	return out.String(), nil
}

// pdfPageText renders one page, recovering from parser panics so a single bad
// page does not lose the rest of the document.
func pdfPageText(r *pdf.Reader, num int) (text string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			text = ""
			err = fmt.Errorf("pdf: page %d: %v", num, rec)
		}
	}()

	p := r.Page(num)
	if p.V.IsNull() {
		return "", nil
	}

	rows, err := p.GetTextByRow()
	if err == nil && len(rows) > 0 {
		var b strings.Builder
		for _, row := range rows {
			line := joinRow(row)
			if strings.TrimSpace(line) == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
		}
		if b.Len() > 0 {
			return b.String(), nil
		}
	}

	// Fall back to the unstructured extractor.
	plain, perr := p.GetPlainText(nil)
	if perr != nil {
		if err != nil {
			return "", err
		}
		return "", perr
	}
	return strings.TrimSpace(plain), nil
}

// joinRow concatenates the text runs of one row. Producers vary: some emit one
// run per word, some one run per glyph. Inserting a space between two runs that
// are both single characters would shred words, so that case is concatenated.
func joinRow(row *pdf.Row) string {
	var b strings.Builder
	for _, t := range row.Content {
		s := t.S
		if s == "" {
			continue
		}
		if b.Len() > 0 && needsSpace(b.String(), s) {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	return strings.TrimRight(b.String(), " ")
}

func needsSpace(prev, next string) bool {
	if prev == "" || next == "" {
		return false
	}
	if strings.HasSuffix(prev, " ") || strings.HasPrefix(next, " ") {
		return false
	}
	// Single-character runs on both sides: almost certainly per-glyph output.
	lastWord := prev
	if i := strings.LastIndexByte(prev, ' '); i >= 0 {
		lastWord = prev[i+1:]
	}
	if len([]rune(lastWord)) <= 1 && len([]rune(next)) <= 1 {
		return false
	}
	return true
}

func isEncryptedErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, pdf.ErrInvalidPassword) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "encrypt") || strings.Contains(msg, "password")
}
