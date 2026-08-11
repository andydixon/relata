package search

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/andydixon/relata/internal/domain"
)

// Probe the pure helpers with adversarial input.
func TestExcerptHelpersOnAdversarialInput(t *testing.T) {
	inputs := []string{"", " ", "\x00", "é", "ééééééééééé", "a\nb\tc", "🌍🌍🌍"}
	for _, in := range inputs {
		for _, n := range []int{0, 1, 2, 5, 1000} {
			got := truncateWords(in, n)
			for _, r := range got {
				if r == '�' {
					t.Fatalf("truncateWords(%q,%d) produced a replacement char: %q", in, n, got)
				}
			}
		}
	}
}

func TestFindTermWindowOnAdversarialInput(t *testing.T) {
	texts := []string{"", "short", "ééééé match ééééé", "🌍 match 🌍"}
	for _, txt := range texts {
		for _, terms := range [][]string{nil, {}, {""}, {"match"}, {"zzz"}, {"é"}} {
			got := findTermWindow(txt, terms, 20)
			for _, r := range got {
				if r == '�' {
					t.Fatalf("findTermWindow(%q,%v) produced a replacement char: %q", txt, terms, got)
				}
			}
		}
	}
}

func TestMatchesFilterHandlesZeroValues(t *testing.T) {
	it := &domain.Item{}
	if !matchesFilter(it, domain.ItemFilter{}, Query{}) {
		t.Fatal("an empty filter rejected an item")
	}
	future := time.Now().Add(time.Hour)
	if matchesFilter(it, domain.ItemFilter{CreatedFrom: &future}, Query{}) {
		t.Fatal("a zero-time item passed a future CreatedFrom filter")
	}
}

// Excerpt building slices UTF-8 text at computed offsets, so it is exactly the
// kind of code that produces mangled output on unusual input.
func FuzzExcerptHelpers(f *testing.F) {
	f.Add("The revised delivery date is 14 March 2025.", "delivery", 40)
	f.Add("ééééé", "é", 3)
	f.Add("🌍🌍🌍", "🌍", 2)
	f.Add("", "", 0)

	f.Fuzz(func(t *testing.T, text, term string, width int) {
		if width < 0 || width > 1<<16 {
			return
		}
		// Slicing valid UTF-8 must not produce invalid UTF-8.
		if !utf8.ValidString(text) {
			return
		}
		for name, got := range map[string]string{
			"truncateWords":  truncateWords(text, width),
			"findTermWindow": findTermWindow(text, []string{term}, width),
			"collapse":       collapse(text),
		} {
			if !utf8.ValidString(got) {
				t.Fatalf("%s produced invalid UTF-8 from text=%q term=%q width=%d -> %q",
					name, text, term, width, got)
			}
		}
	})
}
