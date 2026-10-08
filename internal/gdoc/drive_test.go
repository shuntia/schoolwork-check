package gdoc

import (
	"strings"
	"testing"
)

// A Google Doc's Markdown export inlines every image as base64 and hangs the
// blobs off the end as reference definitions. A calendar with a dozen film
// stills in it exports as 689 KB of which 17 KB is words.
func TestStripImageDataKeepsTheWords(t *testing.T) {
	blob := strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 40)
	in := strings.Join([]string{
		"**Film Analysis**",
		"| Monday | Tuesday |",
		"| 10 First day ![still][image1] | 11 Watch *Casablanca* |",
		"| 17 Film Bio due | 18 Lighting tutorial |",
		"",
		"[image1]: <data:image/png;base64," + blob + ">",
		"[image2]: <data:image/jpeg;base64," + blob + ">",
	}, "\n")

	got := stripImageData(in)
	if strings.Contains(got, "base64") {
		t.Errorf("image data survived:\n%s", got)
	}
	for _, want := range []string{"**Film Analysis**", "| 10 First day ![still][image1] |", "17 Film Bio due"} {
		if !strings.Contains(got, want) {
			t.Errorf("stripping lost %q:\n%s", want, got)
		}
	}
	if len(got) >= len(in)/4 {
		t.Errorf("stripped text is %d bytes of %d, want most of it gone", len(got), len(in))
	}
}

func TestStripImageDataLeavesOrdinaryTextAlone(t *testing.T) {
	in := "| 10 Read chapter 3 |\n| 17 Quiz on data:image formats |\n"
	if got := stripImageData(in); got != in {
		t.Errorf("stripImageData changed text with no image data:\n%q\n%q", in, got)
	}
}

func TestStripImageDataFlattensAnInlineBlob(t *testing.T) {
	in := "| 10 Poster <data:image/png;base64," + strings.Repeat("A", 300) + "> due |\n"
	got := stripImageData(in)
	if strings.Contains(got, "base64") || !strings.Contains(got, "(image)") {
		t.Errorf("inline blob not replaced: %q", got)
	}
	if !strings.Contains(got, "10 Poster") || !strings.Contains(got, "due") {
		t.Errorf("stripping ate the row: %q", got)
	}
}

func TestClipText(t *testing.T) {
	got, truncated, err := clipText("読書感想文", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("truncated = false, want true")
	}
	if !strings.HasPrefix("読書感想文", got) || len(got) > 7 {
		t.Errorf("clipText = %q, want a prefix cut on a rune boundary", got)
	}
	if got, truncated, _ := clipText("short", 100); truncated || got != "short" {
		t.Errorf("clipText(short) = %q, %v", got, truncated)
	}
}
