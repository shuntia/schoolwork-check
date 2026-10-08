package extract

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func run(t *testing.T, in Input) (Result, error) {
	t.Helper()
	return Text(context.Background(), in)
}

func TestVarsAssigned(t *testing.T) {
	if Text == nil {
		t.Fatal("extract.Text is nil")
	}
	if HTMLToText == nil {
		t.Fatal("extract.HTMLToText is nil")
	}
}

func TestPlainTextWithMIMEParameters(t *testing.T) {
	res, err := run(t, Input{
		Name:     "notes.txt",
		MimeType: "text/plain; charset=utf-8",
		Reader:   strings.NewReader("line one\r\nline two\r\n"),
	})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if res.Text != "line one\nline two" {
		t.Fatalf("got %q", res.Text)
	}
	if res.Truncated {
		t.Fatal("unexpected truncation")
	}
}

func TestNormalizeMIME(t *testing.T) {
	cases := map[string]string{
		"text/plain; charset=utf-8":    "text/plain",
		"TEXT/HTML;charset=UTF-8":      "text/html",
		"application/pdf":              "application/pdf",
		"application/pdf ; name=a.pdf": "application/pdf",
		"":                             "",
		"garbage;;;":                   "garbage",
	}
	for in, want := range cases {
		if got := normalizeMIME(in); got != want {
			t.Errorf("normalizeMIME(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtensionFallbackWhenMIMEEmpty(t *testing.T) {
	for _, name := range []string{"hw.md", "main.go", "data.csv", "solution.py", "run.sh"} {
		res, err := run(t, Input{Name: name, Reader: strings.NewReader("content of " + name)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Text != "content of "+name {
			t.Fatalf("%s: got %q", name, res.Text)
		}
	}
}

func TestExtensionFallbackForOctetStream(t *testing.T) {
	res, err := run(t, Input{
		Name:     "readme.txt",
		MimeType: "application/octet-stream",
		Reader:   strings.NewReader("hi"),
	})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if res.Text != "hi" {
		t.Fatalf("got %q", res.Text)
	}
}

func TestUnsupportedTypes(t *testing.T) {
	cases := []Input{
		{Name: "diagram.png", MimeType: "image/png", Reader: strings.NewReader("\x89PNG")},
		{Name: "clip.mp4", MimeType: "video/mp4", Reader: strings.NewReader("x")},
		{Name: "bundle.zip", MimeType: "application/zip", Reader: strings.NewReader("PK")},
		{Name: "sheet.xlsx", Reader: strings.NewReader("PK")},
		{Name: "doc", MimeType: "application/vnd.google-apps.document", Reader: strings.NewReader("")},
		{Name: "mystery", Reader: strings.NewReader("")},
	}
	for _, in := range cases {
		_, err := run(t, in)
		var ue *UnsupportedError
		if !errors.As(err, &ue) {
			t.Fatalf("%s/%s: want UnsupportedError, got %v", in.Name, in.MimeType, err)
		}
		if ue.Name != in.Name || ue.MimeType != in.MimeType {
			t.Fatalf("UnsupportedError fields = %q/%q", ue.MimeType, ue.Name)
		}
	}
}

func TestLimitTruncatesOnRuneBoundary(t *testing.T) {
	// Each "日" is 3 bytes; a 10 byte limit lands inside the fourth rune.
	body := strings.Repeat("日", 8)
	res, err := run(t, Input{Name: "kanji.txt", MimeType: "text/plain", Reader: strings.NewReader(body), Limit: 10})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if !res.Truncated {
		t.Fatal("want Truncated")
	}
	if !utf8.ValidString(res.Text) {
		t.Fatalf("invalid UTF-8 after truncation: %q", res.Text)
	}
	if res.Text != strings.Repeat("日", 3) {
		t.Fatalf("got %q", res.Text)
	}
	if len(res.Text) > 10 {
		t.Fatalf("len %d > limit", len(res.Text))
	}
}

func TestNoTruncationAtExactLimit(t *testing.T) {
	res, err := run(t, Input{Name: "a.txt", MimeType: "text/plain", Reader: strings.NewReader("abcde"), Limit: 5})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if res.Truncated || res.Text != "abcde" {
		t.Fatalf("got %q truncated=%v", res.Text, res.Truncated)
	}
}

func TestCleanDropsNULsAndInvalidUTF8(t *testing.T) {
	res, err := run(t, Input{
		Name:   "dirty.txt",
		Reader: bytes.NewReader([]byte("ok\x00ay \xff\xfe done")),
	})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if strings.ContainsRune(res.Text, 0) {
		t.Fatalf("NUL survived: %q", res.Text)
	}
	if !utf8.ValidString(res.Text) {
		t.Fatalf("invalid UTF-8: %q", res.Text)
	}
	if res.Text != "okay  done" {
		t.Fatalf("got %q", res.Text)
	}
}

func TestHTMLAttachmentGoesThroughHTMLToText(t *testing.T) {
	res, err := run(t, Input{
		Name:     "page.html",
		MimeType: "text/html; charset=utf-8",
		Reader:   strings.NewReader("<html><body><h1>Title</h1><p>Body &amp; more</p></body></html>"),
	})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if res.Text != "Title\nBody & more" {
		t.Fatalf("got %q", res.Text)
	}
}

func TestHTMByExtension(t *testing.T) {
	res, err := run(t, Input{Name: "page.htm", Reader: strings.NewReader("<p>hi</p>")})
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if res.Text != "hi" {
		t.Fatalf("got %q", res.Text)
	}
}

func TestNilReader(t *testing.T) {
	if _, err := run(t, Input{Name: "a.txt"}); err == nil {
		t.Fatal("want error for nil reader")
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Text(ctx, Input{Name: "a.txt", Reader: strings.NewReader("x")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
