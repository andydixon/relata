// Package chunking splits a parsed document into retrievable chunks.
//
// Chunking follows document structure rather than counting characters blindly:
// headings, paragraphs, list groups, tables and JSON object boundaries are
// preferred split points, and a character ceiling is applied only when a
// structural block is itself too large.
package chunking

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/andydixon/relata/internal/parsing"
)

// Options configure the chunker. Zero values are replaced by defaults.
type Options struct {
	// MaxChars is the hard ceiling on one chunk's core span.
	MaxChars int
	// TargetChars is the size the chunker aims for when merging small blocks.
	TargetChars int
	// MinChars is the size below which a chunk is merged into its neighbour
	// rather than emitted on its own.
	MinChars int
	// OverlapChars extends each chunk backwards into its predecessor, so a
	// sentence spanning a boundary is still retrievable from both sides.
	OverlapChars int
	// MaxChunks bounds output; excess content is reported via Truncated.
	MaxChunks int
	// MinSplitChars is the document size below which the whole document becomes
	// a single chunk. Splitting a short note only dilutes its embedding.
	MinSplitChars int
}

func (o Options) withDefaults() Options {
	if o.MaxChars <= 0 {
		o.MaxChars = 1800
	}
	if o.TargetChars <= 0 || o.TargetChars > o.MaxChars {
		o.TargetChars = o.MaxChars * 2 / 3
	}
	if o.MinChars <= 0 {
		o.MinChars = o.MaxChars / 9
	}
	if o.OverlapChars < 0 || o.OverlapChars >= o.MaxChars {
		o.OverlapChars = 0
	}
	if o.MaxChunks <= 0 {
		o.MaxChunks = 400
	}
	if o.MinSplitChars <= 0 {
		o.MinSplitChars = o.MaxChars / 2
	}
	return o
}

// Chunk is one produced chunk. Start and End are byte offsets into the source
// text and always satisfy Text == text[Start:End], including any overlap.
type Chunk struct {
	Sequence int
	Text     string
	Start    int
	End      int
	Hash     string
	Page     int
	Section  string
	Heading  string
	// OverlapChars is how many leading bytes were carried over from the
	// preceding chunk, so callers can tell borrowed context from new content.
	OverlapChars int
}

// Result is the chunker's output.
type Result struct {
	Chunks []Chunk
	// Truncated is true when MaxChunks stopped the chunker before the end of
	// the document.
	Truncated bool
}

// Split divides doc into chunks.
//
// The returned chunks cover the document in order. Very short documents are
// returned as a single chunk: splitting a two-sentence note produces fragments
// that embed worse than the whole.
func Split(doc *parsing.Document, opts Options) Result {
	o := opts.withDefaults()
	text := doc.Text

	if strings.TrimSpace(text) == "" {
		return Result{}
	}

	// A short document is one chunk, whatever its internal structure.
	if len(text) <= o.MinSplitChars {
		return Result{Chunks: []Chunk{finish(0, text, 0, len(text), 0, firstPage(doc), firstSection(doc), firstHeading(doc))}}
	}

	blocks := doc.Blocks
	if len(blocks) == 0 {
		blocks = []parsing.Block{{Kind: parsing.BlockParagraph, Text: text, Start: 0, End: len(text)}}
	}

	// Phase 1: group blocks into core spans that respect structure.
	spans := groupBlocks(blocks, text, o)

	// Phase 2: apply overlap and materialise chunks.
	res := Result{}
	for i, sp := range spans {
		if len(res.Chunks) >= o.MaxChunks {
			res.Truncated = true
			break
		}
		start := sp.start
		overlap := 0
		if o.OverlapChars > 0 && i > 0 {
			lowerBound := spans[i-1].start
			cand := start - o.OverlapChars
			if cand < lowerBound {
				cand = lowerBound
			}
			cand = alignRuneStart(text, cand)
			// Prefer to begin the overlap at a word boundary so the borrowed
			// context does not start mid-word.
			if b := lastBoundary(text, cand, start); b > cand {
				cand = b
			}
			if cand < start {
				overlap = start - cand
				start = cand
			}
		}
		res.Chunks = append(res.Chunks, finish(
			len(res.Chunks), text[start:sp.end], start, sp.end, overlap, sp.page, sp.section, sp.heading))
	}
	return res
}

type coreSpan struct {
	start, end int
	page       int
	section    string
	heading    string
}

// groupBlocks merges consecutive blocks into spans no larger than MaxChars,
// starting a new span at structural boundaries.
func groupBlocks(blocks []parsing.Block, text string, o Options) []coreSpan {
	var spans []coreSpan
	var cur *coreSpan
	var curHeading string

	flush := func() {
		if cur != nil {
			spans = append(spans, *cur)
			cur = nil
		}
	}

	for _, b := range blocks {
		if strings.TrimSpace(b.Text) == "" {
			continue
		}
		if b.Kind == parsing.BlockHeading {
			curHeading = strings.TrimLeft(strings.TrimSpace(b.Text), "# ")
		}

		size := b.End - b.Start

		// An oversized single block must be split internally.
		if size > o.MaxChars {
			flush()
			for _, sub := range splitLarge(text, b.Start, b.End, o) {
				spans = append(spans, coreSpan{
					start: sub.start, end: sub.end,
					page: b.Page, section: b.Section, heading: curHeading,
				})
			}
			continue
		}

		switch {
		case cur == nil:
			cur = &coreSpan{start: b.Start, end: b.End, page: b.Page, section: b.Section, heading: curHeading}

		// A heading opens a new section: start a fresh chunk once the current
		// one carries enough content to stand alone.
		case b.Kind == parsing.BlockHeading && cur.end-cur.start >= o.MinChars:
			flush()
			cur = &coreSpan{start: b.Start, end: b.End, page: b.Page, section: b.Section, heading: curHeading}

		// A page change in a paginated document is a real boundary.
		case b.Page > 0 && cur.page > 0 && b.Page != cur.page && cur.end-cur.start >= o.MinChars:
			flush()
			cur = &coreSpan{start: b.Start, end: b.End, page: b.Page, section: b.Section, heading: curHeading}

		// Adding this block would exceed the ceiling.
		case b.End-cur.start > o.MaxChars:
			flush()
			cur = &coreSpan{start: b.Start, end: b.End, page: b.Page, section: b.Section, heading: curHeading}

		// A JSON object boundary is worth respecting once the chunk is big
		// enough, so an object's fields stay together.
		case b.Kind == parsing.BlockJSONField && b.Section != cur.section &&
			cur.end-cur.start >= o.TargetChars:
			flush()
			cur = &coreSpan{start: b.Start, end: b.End, page: b.Page, section: b.Section, heading: curHeading}

		default:
			cur.end = b.End
			if cur.section == "" {
				cur.section = b.Section
			}
			if cur.page == 0 {
				cur.page = b.Page
			}
			// Reaching the target size is a good place to stop, provided the
			// next block is not mid-thought (handled by the loop restarting).
			if cur.end-cur.start >= o.TargetChars {
				flush()
			}
		}
	}
	flush()

	return mergeTiny(spans, o)
}

// mergeTiny folds spans below MinChars into an adjacent span, so the output
// does not contain fragments too small to embed usefully.
func mergeTiny(spans []coreSpan, o Options) []coreSpan {
	if len(spans) < 2 {
		return spans
	}
	out := make([]coreSpan, 0, len(spans))
	for _, sp := range spans {
		if len(out) > 0 {
			prev := &out[len(out)-1]
			tiny := sp.end-sp.start < o.MinChars
			fits := sp.end-prev.start <= o.MaxChars
			contiguous := sp.start >= prev.end
			if tiny && fits && contiguous {
				prev.end = sp.end
				if prev.section == "" {
					prev.section = sp.section
				}
				continue
			}
		}
		out = append(out, sp)
	}
	return out
}

// splitLarge divides one oversized block at the best available boundary:
// blank lines, then line breaks, then sentence ends, then a hard character cut.
func splitLarge(text string, start, end int, o Options) []span {
	var out []span
	for start < end {
		limit := start + o.MaxChars
		if limit >= end {
			out = append(out, span{start, end})
			break
		}
		limit = alignRuneStart(text, limit)
		// Look backwards from the ceiling for a boundary, but not so far back
		// that chunks become tiny.
		floor := start + o.MinChars
		if floor > limit {
			floor = start + (limit-start)/2
		}
		cut := findBoundary(text, floor, limit)
		if cut <= start {
			cut = limit
		}
		out = append(out, span{start, cut})
		start = cut
	}
	return out
}

type span struct{ start, end int }

// findBoundary searches [floor,limit) for the latest good split point.
func findBoundary(text string, floor, limit int) int {
	if floor < 0 {
		floor = 0
	}
	if limit > len(text) {
		limit = len(text)
	}
	// Blank line.
	for i := limit - 1; i > floor; i-- {
		if text[i] == '\n' && i > 0 && text[i-1] == '\n' {
			return i + 1
		}
	}
	// Sentence terminator followed by whitespace.
	for i := limit - 1; i > floor; i-- {
		if (text[i] == '.' || text[i] == '!' || text[i] == '?' || text[i] == ';') &&
			i+1 < len(text) && isSpace(text[i+1]) {
			return i + 1
		}
	}
	// Line break.
	for i := limit - 1; i > floor; i-- {
		if text[i] == '\n' {
			return i + 1
		}
	}
	// Word break.
	for i := limit - 1; i > floor; i-- {
		if isSpace(text[i]) {
			return i + 1
		}
	}
	return limit
}

// lastBoundary finds a word start within [from,upto) to begin overlap text at.
func lastBoundary(text string, from, upto int) int {
	for i := from; i < upto && i < len(text); i++ {
		if isSpace(text[i]) {
			return i + 1
		}
	}
	return from
}

func isSpace(b byte) bool { return b == ' ' || b == '\n' || b == '\t' || b == '\r' }

// alignRuneStart moves i back to the nearest UTF-8 rune boundary.
func alignRuneStart(text string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(text) {
		return len(text)
	}
	for i > 0 && !utf8.RuneStart(text[i]) {
		i--
	}
	return i
}

func finish(seq int, body string, start, end, overlap, page int, section, heading string) Chunk {
	return Chunk{
		Sequence:     seq,
		Text:         body,
		Start:        start,
		End:          end,
		Hash:         HashText(body),
		Page:         page,
		Section:      section,
		Heading:      heading,
		OverlapChars: overlap,
	}
}

// HashText returns the hex SHA-256 of s. Chunk hashes make re-chunking
// idempotent: an unchanged chunk keeps its identity across reprocessing.
func HashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func firstPage(doc *parsing.Document) int {
	if len(doc.Blocks) > 0 {
		return doc.Blocks[0].Page
	}
	return 0
}

func firstSection(doc *parsing.Document) string {
	if len(doc.Blocks) > 0 {
		return doc.Blocks[0].Section
	}
	return ""
}

func firstHeading(doc *parsing.Document) string {
	for _, b := range doc.Blocks {
		if b.Kind == parsing.BlockHeading {
			return strings.TrimLeft(strings.TrimSpace(b.Text), "# ")
		}
	}
	return ""
}
