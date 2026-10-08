package dedup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"schoolwork-check/internal/enrich"
	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

// sameLLM answers "same" for every pair it is given, and records what it saw.
type sameLLM struct {
	same    bool
	err     error
	calls   int
	prompts []string
}

func (f *sameLLM) Complete(_ context.Context, _, user string) (string, error) {
	f.calls++
	f.prompts = append(f.prompts, user)
	if f.err != nil {
		return "", f.err
	}
	var ids []string
	for _, line := range strings.Split(user, "\n") {
		if strings.HasPrefix(line, "Pair ") {
			ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(line, "Pair "), ":"))
		}
	}
	type v struct {
		ID   string `json:"id"`
		Same bool   `json:"same"`
	}
	out := struct {
		Verdicts []v `json:"verdicts"`
	}{}
	for _, id := range ids {
		out.Verdicts = append(out.Verdicts, v{ID: id, Same: f.same})
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func day(d int) *time.Time {
	t := time.Date(2026, 9, d, 23, 59, 0, 0, time.UTC)
	return &t
}

func lms(id, title string, d int) model.Task {
	return model.Task{ID: "canvas:" + id, Source: model.SourceCanvas, Kind: model.KindAssignment,
		Course: "Biology 10", Title: title, DueAt: day(d)}
}

func cal(id, title string, d int) model.Task {
	return model.Task{ID: "gdoc:F:" + id, Source: model.SourceGDoc, Kind: model.KindAssignment,
		Course: "Biology", Title: title, DueAt: day(d)}
}

func opts(t *testing.T) Options {
	t.Helper()
	c, err := filecache.Open(filepath.Join(t.TempDir(), "c.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Options{Model: "test-model", FileCache: c}
}

func titles(ts []model.Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t.Source)+":"+t.Title)
	}
	return out
}

func TestDropsTheCalendarRowWhenTheModelSaysSame(t *testing.T) {
	tasks := []model.Task{
		lms("1", "Osmosis Lab Report — Unit 2", 25),
		cal("a", "Lab write-up", 25),
	}
	llm := &sameLLM{same: true}
	got, st := Run(context.Background(), llm, tasks, opts(t))

	if len(got) != 1 || got[0].Source != model.SourceCanvas {
		t.Fatalf("got %v, want only the Canvas task", titles(got))
	}
	if st.Duplicates != 1 || st.Asked != 1 {
		t.Errorf("stats = %+v, want 1 duplicate from 1 call", st)
	}
	if !strings.Contains(llm.prompts[0], "Lab write-up") || !strings.Contains(llm.prompts[0], "Osmosis Lab Report") {
		t.Errorf("the prompt does not carry both titles:\n%s", llm.prompts[0])
	}
}

func TestKeepsBothWhenTheModelSaysDifferent(t *testing.T) {
	tasks := []model.Task{
		lms("1", "Essay draft", 25),
		cal("a", "Essay final", 25),
	}
	got, st := Run(context.Background(), &sameLLM{same: false}, tasks, opts(t))
	if len(got) != 2 {
		t.Fatalf("got %v, want both kept", titles(got))
	}
	if st.Duplicates != 0 {
		t.Errorf("dropped %d, want 0", st.Duplicates)
	}
}

func TestIdenticalTitlesNeedNoModel(t *testing.T) {
	tasks := []model.Task{
		lms("1", "Genetics problem set #1", 25),
		cal("a", "Genetics Problem Set 1", 25),
	}
	llm := &sameLLM{same: false} // would say "different" if asked
	got, st := Run(context.Background(), llm, tasks, opts(t))
	if len(got) != 1 {
		t.Fatalf("got %v, want the calendar row dropped without a call", titles(got))
	}
	if llm.calls != 0 || st.Obvious != 1 {
		t.Errorf("calls = %d, obvious = %d, want 0 and 1", llm.calls, st.Obvious)
	}
}

func TestVerdictIsCachedAcrossRuns(t *testing.T) {
	tasks := []model.Task{
		lms("1", "Osmosis Lab Report", 25),
		cal("a", "Lab write-up", 25),
	}
	o := opts(t)
	llm := &sameLLM{same: true}
	if _, _ = Run(context.Background(), llm, tasks, o); llm.calls != 1 {
		t.Fatalf("first run made %d calls, want 1", llm.calls)
	}
	got, st := Run(context.Background(), llm, tasks, o)
	if llm.calls != 1 {
		t.Errorf("second run made %d calls in total, want the verdict reused", llm.calls)
	}
	if st.Cached != 1 || len(got) != 1 {
		t.Errorf("stats = %+v, tasks = %v", st, titles(got))
	}
}

func TestModelFailureKeepsEverything(t *testing.T) {
	tasks := []model.Task{
		lms("1", "Osmosis Lab Report", 25),
		cal("a", "Lab write-up", 25),
	}
	got, st := Run(context.Background(), &sameLLM{err: errors.New("boom")}, tasks, opts(t))
	if len(got) != 2 {
		t.Fatalf("got %v, want both kept when the model fails", titles(got))
	}
	if st.Failed != 1 {
		t.Errorf("failed = %d, want 1", st.Failed)
	}
}

func TestStopsAskingWhenTheProviderSaysStop(t *testing.T) {
	var tasks []model.Task
	for i := 1; i <= 28; i++ {
		tasks = append(tasks,
			lms(fmt.Sprintf("c%d", i), fmt.Sprintf("Chapter %d reading packet", i), i),
			cal(fmt.Sprintf("c%d", i), fmt.Sprintf("Reading packet for chapter %d", i), i))
	}
	llm := &sameLLM{err: errors.Join(enrich.ErrStop, errors.New("HTTP 429"))}
	got, st := Run(context.Background(), llm, tasks, opts(t))
	if len(got) != len(tasks) {
		t.Errorf("got %d tasks, want all %d kept", len(got), len(tasks))
	}
	if llm.calls != 1 {
		t.Errorf("made %d calls, want 1: a rate limit stops the check", llm.calls)
	}
	if st.Duplicates != 0 {
		t.Errorf("dropped %d rows after a failure, want 0", st.Duplicates)
	}
}

func TestOnlyCalendarRowsAreEverDropped(t *testing.T) {
	tasks := []model.Task{
		lms("1", "Osmosis Lab Report", 25),
		lms("2", "Osmosis Lab Report", 25), // two LMS rows, identical titles
	}
	got, st := Run(context.Background(), &sameLLM{same: true}, tasks, opts(t))
	if len(got) != 2 {
		t.Fatalf("got %v, want both LMS tasks kept", titles(got))
	}
	if st.Pairs != 0 {
		t.Errorf("pairs = %d, want none: neither row is a calendar row", st.Pairs)
	}
}

func TestCandidates(t *testing.T) {
	cases := []struct {
		name  string
		tasks []model.Task
		want  int
	}{
		{"nothing at all", nil, 0},
		{
			"dates too far apart",
			[]model.Task{lms("1", "Osmosis Lab Report", 25), cal("a", "Lab write-up", 3)},
			0,
		},
		{
			"a few days apart is fine",
			[]model.Task{lms("1", "Osmosis Lab Report", 25), cal("a", "Lab write-up", 23)},
			1,
		},
		{
			"no word in common",
			[]model.Task{lms("1", "Osmosis Lab Report", 25), cal("a", "Vocabulary quiz", 25)},
			0,
		},
		{
			"only stopwords in common",
			[]model.Task{lms("1", "The reading for class", 25), cal("a", "The quiz for class", 25)},
			0,
		},
		{
			"two calendar rows, the later one is the candidate",
			[]model.Task{cal("a", "Genetics problem set", 22), cal("b", "Problem set: genetics", 25)},
			1,
		},
		{
			"an undated row is never a candidate",
			[]model.Task{lms("1", "Osmosis Lab Report", 25), {ID: "gdoc:F:x", Source: model.SourceGDoc, Title: "Lab report"}},
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := candidates(tc.tasks, 5*24*time.Hour)
			if len(got) != tc.want {
				t.Errorf("got %d pairs, want %d: %+v", len(got), tc.want, got)
			}
			for _, p := range got {
				if p.b.Source != model.SourceGDoc {
					t.Errorf("pair's b side is %q, want the calendar row", p.b.Source)
				}
			}
		})
	}
}

func TestNorm(t *testing.T) {
	cases := map[string]string{
		"Genetics Problem Set #1": "genetics problem set 1",
		"genetics problem set 1":  "genetics problem set 1",
		"  Lab  write-up!  ":      "lab write up",
	}
	for in, want := range cases {
		if got := norm(in); got != want {
			t.Errorf("norm(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOverlapWeighsWordsByHowRareTheyAre(t *testing.T) {
	// A term of film work: "film" is everywhere, "osmosis" is not.
	var corpus []model.Task
	for i := 1; i <= 40; i++ {
		corpus = append(corpus, cal(fmt.Sprintf("r%d", i), fmt.Sprintf("Watch film %d and take notes", i), 1+i%28))
	}
	corpus = append(corpus,
		lms("lab", "Osmosis Lab Report — Unit 2", 25),
		cal("lab", "Lab write-up", 25),
		cal("ch9", "Read chapter 9", 20),
		lms("ch12", "Read chapter 12", 20),
	)
	df, rare := docFreq(corpus), rareIn(len(corpus))

	cases := []struct {
		name, a, b string
		want       bool
	}{
		{"one rare word in common", "Osmosis Lab Report — Unit 2", "Lab write-up", true},
		{"one everyday word in common", "Watch film 3 and take notes", "Film discussion", false},
		{"a shared number is always worth asking", "Read chapter 9", "Chapter 9 notes", true},
		{"different numbers still get asked", "Read chapter 9", "Read chapter 12", true},
		{"nothing in common", "Osmosis Lab Report", "Vocabulary quiz", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := overlap(words(tc.a), words(tc.b), df, rare); got != tc.want {
				t.Errorf("overlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestTheSameRowOnConsecutiveDaysCollapsesForFree(t *testing.T) {
	// A "Tuesday/Wednesday" cell in a calendar grid becomes two rows.
	tasks := []model.Task{
		cal("a", "Ch 9 Notes", 25),
		cal("b", "Ch 9 notes", 26),
		cal("c", "Ch 9 Notes", 27),
	}
	llm := &sameLLM{same: false}
	got, st := Run(context.Background(), llm, tasks, opts(t))
	if len(got) != 1 || got[0].DueAt.Day() != 25 {
		t.Fatalf("got %v, want only the first row", titles(got))
	}
	// Three rows make three pairs, all obvious; two rows are dropped.
	if llm.calls != 0 || st.Obvious != 3 || st.Duplicates != 2 {
		t.Errorf("calls = %d, stats = %+v, want no calls, 3 obvious, 2 dropped", llm.calls, st)
	}
}

func TestAWeeklyRepeatIsNotADuplicate(t *testing.T) {
	tasks := []model.Task{
		cal("a", "Vocabulary quiz", 4),
		cal("b", "Vocabulary quiz", 11),
		cal("c", "Vocabulary quiz", 18),
	}
	got, st := Run(context.Background(), &sameLLM{same: false}, tasks, opts(t))
	if len(got) != 3 {
		t.Fatalf("got %v, want all three weeks kept", titles(got))
	}
	if st.Pairs != 0 {
		t.Errorf("pairs = %d, want none: seven days apart is a repeat, not a duplicate", st.Pairs)
	}
}
