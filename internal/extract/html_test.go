package extract

import (
	"strings"
	"testing"
)

func TestHTMLToTextEmptyAndMalformed(t *testing.T) {
	cases := []string{"", "   ", "\n\t", "<p>unclosed", "<<<>>>", "<div><span>a</div></span>", "&amp;"}
	for _, in := range cases {
		// Must not panic.
		_ = HTMLToText(in)
	}
	if got := HTMLToText(""); got != "" {
		t.Fatalf("empty input gave %q", got)
	}
	if got := HTMLToText("<p>unclosed"); got != "unclosed" {
		t.Fatalf("got %q", got)
	}
	if got := HTMLToText("&amp;"); got != "&" {
		t.Fatalf("entity: got %q", got)
	}
}

func TestHTMLToTextCanvasDescription(t *testing.T) {
	const in = `<div class="description">
  <p>Read <strong>chapter 4</strong> and answer:</p>
  <ul>
    <li>What is a <em>closure</em>?</li>
    <li>Why does it matter?</li>
  </ul>
  <p>Reference: <a href="https://example.edu/notes.pdf">the notes</a>.</p>
  <p><img src="/x.png" alt="rubric table"></p>
  <script>var x = 1;</script>
  <style>p { color: red }</style>
</div>`
	got := HTMLToText(in)
	want := strings.Join([]string{
		"Read chapter 4 and answer:",
		"- What is a closure?",
		"- Why does it matter?",
		"Reference: the notes (https://example.edu/notes.pdf).",
		"[image: rubric table]",
	}, "\n")
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestHTMLToTextOrderedList(t *testing.T) {
	got := HTMLToText("<ol><li>first</li><li>second</li></ol>")
	if got != "1. first\n2. second" {
		t.Fatalf("got %q", got)
	}
}

func TestHTMLToTextNestedDivsAndBreaks(t *testing.T) {
	got := HTMLToText("<div><div>one</div><div>two<br>three</div></div><p></p><p>four</p>")
	if got != "one\ntwo\nthree\nfour" {
		t.Fatalf("got %q", got)
	}
}

func TestHTMLToTextCollapsesWhitespaceAndBlankLines(t *testing.T) {
	got := HTMLToText("<p>a     b\n\n\tc</p><p><br><br><br></p><p>d</p>")
	if got != "a b c\n\nd" {
		t.Fatalf("got %q", got)
	}
}

func TestHTMLToTextLinkSameAsText(t *testing.T) {
	if got := HTMLToText(`<a href="https://example.com/a">https://example.com/a</a>`); got != "https://example.com/a" {
		t.Fatalf("got %q", got)
	}
	if got := HTMLToText(`<a href="/relative/path">local</a>`); got != "local" {
		t.Fatalf("relative href should not be shown: got %q", got)
	}
	if got := HTMLToText(`<a href="https://example.com/x"></a>`); got != "https://example.com/x" {
		t.Fatalf("empty anchor text: got %q", got)
	}
}

func TestHTMLToTextTableAndHeadings(t *testing.T) {
	got := HTMLToText("<h2>Grades</h2><table><tr><td>A</td><td>90</td></tr><tr><td>B</td><td>80</td></tr></table>")
	if got != "Grades\nA\t90\nB\t80" {
		t.Fatalf("got %q", got)
	}
}

func TestHTMLToTextPreservesPre(t *testing.T) {
	got := HTMLToText("<p>Code:</p><pre>for i in range(3):\n    print(i)</pre>")
	if got != "Code:\nfor i in range(3):\n    print(i)" {
		t.Fatalf("got %q", got)
	}
}

func TestHTMLToTextSkipsHeadAndScript(t *testing.T) {
	got := HTMLToText("<html><head><title>T</title><style>b{}</style></head><body><p>visible</p></body></html>")
	if got != "visible" {
		t.Fatalf("got %q", got)
	}
}
