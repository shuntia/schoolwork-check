package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// docxText extracts the body text of a .docx (OOXML word processing document).
func docxText(ctx context.Context, data []byte) (string, error) {
	zr, err := openZip(data, "docx")
	if err != nil {
		return "", err
	}
	f := zipFile(zr, "word/document.xml")
	if f == nil {
		return "", errors.New("docx: missing word/document.xml")
	}
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("docx: %w", err)
	}
	defer rc.Close()

	text, err := wordprocessingText(ctx, rc)
	if err != nil {
		return "", fmt.Errorf("docx: %w", err)
	}
	return text, nil
}

// wordprocessingText walks word/document.xml: w:t holds the text, w:p ends a
// paragraph, w:tab is a tab, w:br / w:cr are line breaks, w:tr ends a table row.
func wordprocessingText(ctx context.Context, r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false

	var (
		out    strings.Builder
		inText int
		nodes  int
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Salvage whatever was collected before the malformed part.
			break
		}
		nodes++
		if nodes%2048 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText++
			case "tab":
				out.WriteByte('\t')
			case "br", "cr":
				out.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				if inText > 0 {
					inText--
				}
			case "p", "tr":
				out.WriteByte('\n')
			case "tc":
				out.WriteByte('\t')
			}
		case xml.CharData:
			if inText > 0 {
				out.Write(t)
			}
		}
	}
	return out.String(), nil
}

// pptxText extracts the text runs of every slide of a .pptx, in slide order.
func pptxText(ctx context.Context, data []byte) (string, error) {
	zr, err := openZip(data, "pptx")
	if err != nil {
		return "", err
	}

	type slide struct {
		num  int
		file *zip.File
	}
	var slides []slide
	for _, f := range zr.File {
		name := f.Name
		if !strings.HasPrefix(name, "ppt/slides/slide") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		digits := strings.TrimSuffix(strings.TrimPrefix(name, "ppt/slides/slide"), ".xml")
		n, err := strconv.Atoi(digits)
		if err != nil {
			continue
		}
		slides = append(slides, slide{num: n, file: f})
	}
	if len(slides) == 0 {
		return "", errors.New("pptx: no slides found")
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].num < slides[j].num })

	var out strings.Builder
	for _, s := range slides {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		rc, err := s.file.Open()
		if err != nil {
			return "", fmt.Errorf("pptx: %w", err)
		}
		text, err := drawingMLText(ctx, rc)
		rc.Close()
		if err != nil {
			return "", fmt.Errorf("pptx: %w", err)
		}
		text = strings.Trim(text, "\n")
		if text == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(text)
	}
	return out.String(), nil
}

// drawingMLText walks a slide part: a:t holds the text, a:p ends a paragraph,
// a:br is a line break.
func drawingMLText(ctx context.Context, r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false

	var (
		out    strings.Builder
		inText int
		nodes  int
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		nodes++
		if nodes%2048 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText++
			case "br":
				out.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				if inText > 0 {
					inText--
				}
			case "p":
				out.WriteByte('\n')
			}
		case xml.CharData:
			if inText > 0 {
				out.Write(t)
			}
		}
	}
	return out.String(), nil
}

func openZip(data []byte, kind string) (*zip.Reader, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%s: empty document", kind)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%s: not a valid archive: %w", kind, err)
	}
	return zr, nil
}

func zipFile(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}
