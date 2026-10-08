package extract

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPDFExtraction(t *testing.T) {
	data := buildPDF(t, "Hello PDF", "Second line")
	res, err := run(t, Input{Name: "hw.pdf", MimeType: "application/pdf", Reader: bytes.NewReader(data)})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if res.Text != "Hello PDF\nSecond line" {
		t.Fatalf("got %q", res.Text)
	}
}

func TestPDFByExtensionOnly(t *testing.T) {
	data := buildPDF(t, "Hello PDF")
	res, err := run(t, Input{Name: "hw.pdf", Reader: bytes.NewReader(data)})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if !strings.Contains(res.Text, "Hello PDF") {
		t.Fatalf("got %q", res.Text)
	}
}

func TestPDFTruncation(t *testing.T) {
	data := buildPDF(t, "Hello PDF", "Second line")
	res, err := run(t, Input{Name: "hw.pdf", MimeType: "application/pdf", Reader: bytes.NewReader(data), Limit: 5})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if !res.Truncated || res.Text != "Hello" {
		t.Fatalf("got %q truncated=%v", res.Text, res.Truncated)
	}
}

func TestGarbagePDFErrorsWithoutPanic(t *testing.T) {
	cases := map[string][]byte{
		"not a pdf at all": []byte("this is definitely not a pdf"),
		"truncated header": []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\n"),
		"random bytes":     bytes.Repeat([]byte{0x00, 0xff, 0x10, 0x9a}, 64),
		"empty":            {},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := run(t, Input{Name: "bad.pdf", MimeType: "application/pdf", Reader: bytes.NewReader(data)})
			if err == nil {
				t.Fatalf("want error, got text %q", res.Text)
			}
			if !strings.HasPrefix(err.Error(), "pdf:") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCorruptedPDFBodyDoesNotPanic(t *testing.T) {
	// Valid trailer, shredded body: this is the shape that makes the parser panic.
	data := buildPDF(t, "Hello PDF")
	corrupt := append([]byte(nil), data...)
	for i := 20; i < len(corrupt)-120; i++ {
		corrupt[i] = 0x7f
	}
	if _, err := pdfText(context.Background(), corrupt); err == nil {
		t.Log("corrupt PDF still parsed; no panic is what matters")
	}
}

func TestDOCXExtraction(t *testing.T) {
	res, err := run(t, Input{
		Name:     "essay.docx",
		MimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Reader:   bytes.NewReader(buildDOCX(t)),
	})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	want := "Essay: The Great Gatsby\nFirst paragraph.\nTabbed\tvalue\nafter break\n\nLast line"
	if res.Text != want {
		t.Fatalf("got %q\nwant %q", res.Text, want)
	}
}

func TestDOCXByExtensionOnly(t *testing.T) {
	res, err := run(t, Input{Name: "essay.docx", Reader: bytes.NewReader(buildDOCX(t))})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if !strings.Contains(res.Text, "Great Gatsby") {
		t.Fatalf("got %q", res.Text)
	}
}

func TestDOCXInvalid(t *testing.T) {
	_, err := run(t, Input{Name: "essay.docx", Reader: strings.NewReader("not a zip")})
	if err == nil || !strings.HasPrefix(err.Error(), "docx:") {
		t.Fatalf("want docx error, got %v", err)
	}

	empty := buildZip(t, map[string]string{"other.xml": "<x/>"})
	_, err = run(t, Input{Name: "essay.docx", Reader: bytes.NewReader(empty)})
	if err == nil || !strings.Contains(err.Error(), "word/document.xml") {
		t.Fatalf("want missing-part error, got %v", err)
	}
}

func TestPPTXExtractionSlideOrder(t *testing.T) {
	res, err := run(t, Input{
		Name:     "deck.pptx",
		MimeType: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		Reader:   bytes.NewReader(buildPPTX(t)),
	})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	want := "Title slide\nSubtitle here\n\nSecond slide\n\nTenth slide"
	if res.Text != want {
		t.Fatalf("got %q\nwant %q", res.Text, want)
	}
}

func TestPPTXNoSlides(t *testing.T) {
	data := buildZip(t, map[string]string{"ppt/presentation.xml": "<x/>"})
	_, err := run(t, Input{Name: "deck.pptx", Reader: bytes.NewReader(data)})
	if err == nil || !strings.Contains(err.Error(), "no slides") {
		t.Fatalf("want no-slides error, got %v", err)
	}
}
