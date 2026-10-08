package extract

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// buildPDF assembles a minimal one-page uncompressed PDF whose single text
// object shows the given lines, with a correct xref table.
func buildPDF(t *testing.T, lines ...string) []byte {
	t.Helper()

	var content strings.Builder
	for i, line := range lines {
		y := 700 - 30*i
		fmt.Fprintf(&content, "BT\n/F1 24 Tf\n1 0 0 1 72 %d Tm\n(%s) Tj\nET\n",
			y, escapePDFString(line))
	}
	stream := content.String()

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, body := range objects {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xref)
	return buf.Bytes()
}

func escapePDFString(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)")
	return r.Replace(s)
}

// buildZip writes a zip archive from name -> content.
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

const docxDocumentXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>Essay: The Great Gatsby</w:t></w:r></w:p>
    <w:p><w:r><w:t xml:space="preserve">First </w:t></w:r><w:r><w:t>paragraph.</w:t></w:r></w:p>
    <w:p><w:r><w:t>Tabbed</w:t><w:tab/><w:t>value</w:t><w:br/><w:t>after break</w:t></w:r></w:p>
    <w:p/>
    <w:p><w:r><w:t>Last line</w:t></w:r></w:p>
  </w:body>
</w:document>`

func buildDOCX(t *testing.T) []byte {
	t.Helper()
	return buildZip(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   docxDocumentXML,
	})
}

func slideXML(paragraphs ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" ` +
		`xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree>`)
	for _, p := range paragraphs {
		b.WriteString(`<a:p><a:r><a:t>` + p + `</a:t></a:r></a:p>`)
	}
	b.WriteString(`</p:spTree></p:cSld></p:sld>`)
	return b.String()
}

func buildPPTX(t *testing.T) []byte {
	t.Helper()
	return buildZip(t, map[string]string{
		"ppt/slides/slide1.xml":  slideXML("Title slide", "Subtitle here"),
		"ppt/slides/slide2.xml":  slideXML("Second slide"),
		"ppt/slides/slide10.xml": slideXML("Tenth slide"),
	})
}
