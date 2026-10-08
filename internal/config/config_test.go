package config

import (
	"os"
	"testing"
)

func TestParseCalendarDocs(t *testing.T) {
	const id = "1AbC-dEfG_hIjK"
	cases := []struct {
		name, in string
		want     []CalendarDoc
		wantErr  bool
	}{
		{name: "empty"},
		{
			name: "bare id",
			in:   id,
			want: []CalendarDoc{{ID: id}},
		},
		{
			name: "doc url with a course name",
			in:   "https://docs.google.com/document/d/" + id + "/edit?usp=sharing | Biology",
			want: []CalendarDoc{{ID: id, Course: "Biology"}},
		},
		{
			name: "several, comma separated",
			in:   "https://docs.google.com/spreadsheets/d/" + id + "/edit#gid=0|AP Gov, " + id + "2",
			want: []CalendarDoc{{ID: id, Course: "AP Gov"}, {ID: id + "2"}},
		},
		{
			name: "newline separated, with blanks",
			in:   "\n" + id + "|Chem\n\n" + id + "2|\n",
			want: []CalendarDoc{{ID: id, Course: "Chem"}, {ID: id + "2"}},
		},
		{
			name: "drive file link",
			in:   "https://drive.google.com/file/d/" + id + "/view",
			want: []CalendarDoc{{ID: id}},
		},
		{
			name: "open?id= link",
			in:   "https://drive.google.com/open?id=" + id,
			want: []CalendarDoc{{ID: id}},
		},
		{
			name:    "a url with no file id in it",
			in:      "https://docs.google.com/document/u/0/",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCalendarDocs(tc.in)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("doc %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCalendarEnabledNeedsAGoogleLogin(t *testing.T) {
	c := Config{CalendarDocs: []CalendarDoc{{ID: "x"}}, GoogleCredentialsFile: "/nonexistent/google.json"}
	if c.CalendarEnabled() {
		t.Error("CalendarEnabled with no credentials file, want false")
	}

	f := t.TempDir() + "/google.json"
	if err := writeFile(f); err != nil {
		t.Fatal(err)
	}
	c.GoogleCredentialsFile = f
	if !c.CalendarEnabled() {
		t.Error("CalendarEnabled with docs and credentials, want true")
	}
	c.Sources = []string{"canvas"}
	if c.CalendarEnabled() {
		t.Error("CalendarEnabled when SOURCES leaves gdoc out, want false")
	}
	c.Sources = []string{"gdoc"}
	c.CalendarDocs = nil
	if c.CalendarEnabled() {
		t.Error("CalendarEnabled with no documents, want false")
	}
}

func writeFile(path string) error {
	return os.WriteFile(path, []byte("{}"), 0o600)
}

func TestTokensComeFromFilesWhenTheVariableIsUnset(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("HOME", dir)
	t.Setenv("CANVAS_TOKEN", "")
	t.Setenv("CANVAS_TOKEN_FILE", write("canvas", "canvas-secret\n"))
	t.Setenv("NOTE_TOKEN", "from-env")
	t.Setenv("NOTE_TOKEN_FILE", write("note", "ignored\n"))
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_API_KEY_FILE", dir+"/absent")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.CanvasToken != "canvas-secret" || c.NoteToken != "from-env" || c.LLMAPIKey != "" {
		t.Fatalf("got canvas %q, note %q, llm %q", c.CanvasToken, c.NoteToken, c.LLMAPIKey)
	}
}
