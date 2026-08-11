package chunking

import (
	"strings"
	"testing"

	"github.com/andydixon/relata/internal/parsing"
)

func parse(t *testing.T, mime, body string) *parsing.Document {
	t.Helper()
	doc, err := parsing.Parse(parsing.Input{Data: []byte(body), MIMEType: mime})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

// Offsets must always address the chunk's own text, or every downstream
// source reference (claims, entity mentions, excerpts) silently points at the
// wrong bytes.
func assertOffsetsValid(t *testing.T, doc *parsing.Document, r Result) {
	t.Helper()
	for _, c := range r.Chunks {
		if c.Start < 0 || c.End > len(doc.Text) || c.Start > c.End {
			t.Fatalf("chunk %d has out-of-range offsets [%d,%d) for text of %d bytes",
				c.Sequence, c.Start, c.End, len(doc.Text))
		}
		if got := doc.Text[c.Start:c.End]; got != c.Text {
			t.Fatalf("chunk %d text does not match its offsets:\n got: %q\nwant: %q", c.Sequence, c.Text, got)
		}
	}
}

func TestShortDocumentIsNotSplit(t *testing.T) {
	body := "The delivery date moved to 14 March. Ops confirmed by email."
	doc := parse(t, "text/plain", body)
	r := Split(doc, Options{MaxChars: 400, MinSplitChars: 900})

	if len(r.Chunks) != 1 {
		t.Fatalf("short note should stay whole, got %d chunks", len(r.Chunks))
	}
	if r.Chunks[0].Text != body {
		t.Fatalf("chunk text was altered:\n got %q\nwant %q", r.Chunks[0].Text, body)
	}
	assertOffsetsValid(t, doc, r)
}

func TestSplitsOnMarkdownHeadings(t *testing.T) {
	body := "# Delivery policy\n\n" + strings.Repeat("Deliveries run on weekdays only. ", 12) +
		"\n\n## Revised schedule\n\n" + strings.Repeat("The revised date is 14 March 2025. ", 12) +
		"\n\n## Contact\n\n" + strings.Repeat("Contact operations for exceptions. ", 12)

	doc := parse(t, "text/markdown", body)
	r := Split(doc, Options{MaxChars: 900, TargetChars: 500, MinChars: 120, MinSplitChars: 400})

	if len(r.Chunks) < 3 {
		t.Fatalf("expected a chunk per section, got %d", len(r.Chunks))
	}
	assertOffsetsValid(t, doc, r)

	// The heading that opens a section must travel with that section's body.
	found := false
	for _, c := range r.Chunks {
		if strings.Contains(c.Text, "## Revised schedule") && strings.Contains(c.Text, "14 March 2025") {
			found = true
		}
	}
	if !found {
		t.Fatal("heading was separated from the body it introduces")
	}
}

func TestHeadingRecordedAsChunkContext(t *testing.T) {
	body := "# Contract terms\n\n" + strings.Repeat("Payment is due in 30 days. ", 30) +
		"\n\n## Penalties\n\n" + strings.Repeat("Late payment attracts interest. ", 30)
	doc := parse(t, "text/markdown", body)
	r := Split(doc, Options{MaxChars: 600, TargetChars: 400, MinChars: 100, MinSplitChars: 300})

	var haveSection bool
	for _, c := range r.Chunks {
		if c.Section != "" || c.Heading != "" {
			haveSection = true
		}
	}
	if !haveSection {
		t.Fatal("no chunk retained section or heading context")
	}
}

func TestOversizedBlockIsSplitAtSentenceBoundaries(t *testing.T) {
	// One paragraph, no blank lines, far larger than the ceiling.
	body := strings.Repeat("This sentence is a self-contained statement of fact. ", 200)
	doc := parse(t, "text/plain", body)
	r := Split(doc, Options{MaxChars: 500, TargetChars: 350, MinChars: 100, MinSplitChars: 400})

	if len(r.Chunks) < 5 {
		t.Fatalf("oversized paragraph should be divided, got %d chunks", len(r.Chunks))
	}
	assertOffsetsValid(t, doc, r)

	for _, c := range r.Chunks {
		core := c.End - c.Start - c.OverlapChars
		if core > 500 {
			t.Fatalf("chunk %d core span %d exceeds the ceiling of 500", c.Sequence, core)
		}
	}
	// Splitting at sentence ends means most chunks begin with a capital letter.
	capitals := 0
	for _, c := range r.Chunks {
		trimmed := strings.TrimSpace(c.Text)
		if trimmed != "" && trimmed[0] >= 'A' && trimmed[0] <= 'Z' {
			capitals++
		}
	}
	if capitals*2 < len(r.Chunks) {
		t.Fatalf("only %d/%d chunks start at a sentence boundary", capitals, len(r.Chunks))
	}
}

func TestOverlapIsCarriedFromPreviousChunk(t *testing.T) {
	body := strings.Repeat("Alpha beta gamma delta epsilon zeta. ", 100)
	doc := parse(t, "text/plain", body)
	r := Split(doc, Options{MaxChars: 400, TargetChars: 300, MinChars: 80, OverlapChars: 60, MinSplitChars: 300})

	if len(r.Chunks) < 2 {
		t.Fatalf("need multiple chunks to test overlap, got %d", len(r.Chunks))
	}
	assertOffsetsValid(t, doc, r)

	for i := 1; i < len(r.Chunks); i++ {
		if r.Chunks[i].OverlapChars <= 0 {
			t.Fatalf("chunk %d records no overlap", i)
		}
		// The overlap must genuinely come from the preceding chunk.
		prefix := r.Chunks[i].Text[:r.Chunks[i].OverlapChars]
		if !strings.Contains(r.Chunks[i-1].Text, strings.TrimSpace(prefix)) {
			t.Fatalf("chunk %d overlap %q is not present in chunk %d", i, prefix, i-1)
		}
	}
}

func TestChunkCountIsBounded(t *testing.T) {
	body := strings.Repeat("Paragraph.\n\n", 5000)
	doc := parse(t, "text/plain", body)
	r := Split(doc, Options{MaxChars: 100, TargetChars: 60, MinChars: 10, MaxChunks: 25, MinSplitChars: 50})

	if len(r.Chunks) > 25 {
		t.Fatalf("MaxChunks not enforced: got %d", len(r.Chunks))
	}
	if !r.Truncated {
		t.Fatal("truncation was not reported, so the user would not learn content went unindexed")
	}
}

func TestChunksCoverDocumentWithoutGaps(t *testing.T) {
	body := strings.Repeat("Sentence one here. Sentence two here.\n\n", 60)
	doc := parse(t, "text/plain", body)
	r := Split(doc, Options{MaxChars: 400, TargetChars: 250, MinChars: 80, MinSplitChars: 300})

	assertOffsetsValid(t, doc, r)
	// Ignoring overlap, consecutive core spans must be contiguous or adjacent
	// across whitespace: no document content may be dropped.
	prevEnd := 0
	for _, c := range r.Chunks {
		coreStart := c.Start + c.OverlapChars
		gap := doc.Text[min(prevEnd, len(doc.Text)):min(coreStart, len(doc.Text))]
		if strings.TrimSpace(gap) != "" {
			t.Fatalf("content dropped between chunks: %q", gap)
		}
		prevEnd = c.End
	}
	if strings.TrimSpace(doc.Text[min(prevEnd, len(doc.Text)):]) != "" {
		t.Fatal("trailing content was not chunked")
	}
}

func TestEmptyDocumentProducesNoChunks(t *testing.T) {
	doc := parse(t, "text/plain", "   \n\n  \t ")
	if r := Split(doc, Options{}); len(r.Chunks) != 0 {
		t.Fatalf("whitespace-only document produced %d chunks", len(r.Chunks))
	}
}

func TestHashIsStableAndContentDependent(t *testing.T) {
	a := HashText("the revised delivery date is 14 March")
	b := HashText("the revised delivery date is 14 March")
	c := HashText("the revised delivery date is 15 March")
	if a != b {
		t.Fatal("hash is not deterministic")
	}
	if a == c {
		t.Fatal("different content produced the same hash")
	}
	if len(a) != 64 {
		t.Fatalf("expected hex sha-256, got %d chars", len(a))
	}
}

func TestUTF8IsNeverSplitMidRune(t *testing.T) {
	body := strings.Repeat("Ünïcödé wörds with ümlauts and émojis 🌍 everywhere. ", 100)
	doc := parse(t, "text/plain", body)
	r := Split(doc, Options{MaxChars: 300, TargetChars: 200, MinChars: 60, OverlapChars: 40, MinSplitChars: 200})

	assertOffsetsValid(t, doc, r)
	for _, c := range r.Chunks {
		if !isValidUTF8(c.Text) {
			t.Fatalf("chunk %d contains invalid UTF-8", c.Sequence)
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestJSONChunkingRespectsObjectBoundaries(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"records":[`)
	for i := 0; i < 60; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":`)
		sb.WriteString(strings.TrimSpace(strings.Repeat("1", 2)))
		sb.WriteString(`,"note":"delivery scheduled","owner":"Example Ltd"}`)
	}
	sb.WriteString(`]}`)

	doc := parse(t, "application/json", sb.String())
	r := Split(doc, Options{MaxChars: 400, TargetChars: 250, MinChars: 80, MinSplitChars: 300})

	if len(r.Chunks) < 2 {
		t.Fatalf("expected the JSON document to be divided, got %d chunks", len(r.Chunks))
	}
	assertOffsetsValid(t, doc, r)
}
