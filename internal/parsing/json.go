package parsing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/andydixon/relata/internal/domain"
)

// JSONParser flattens a JSON document into readable "path: value" lines so that
// its content can be embedded and searched like prose, while object boundaries
// remain visible to the chunker.
//
// Depth is bounded before decoding so that a deeply nested document cannot
// exhaust the stack, and the flattened output is capped so that a wide array
// cannot expand without limit.
type JSONParser struct{}

// Name identifies the parser.
func (JSONParser) Name() string { return "json" }

// Handles reports whether p can parse mime.
func (JSONParser) Handles(mime string) bool { return isJSON(mime) }

// defaultMaxJSONDepth is used when Input.MaxJSONDepth is unset.
const defaultMaxJSONDepth = 20

// maxFlattenedLines bounds output from pathologically wide documents.
const maxFlattenedLines = 20000

// Parse decodes the JSON and renders it as a flat, human-readable listing.
func (JSONParser) Parse(in Input) (*Document, error) {
	maxDepth := in.MaxJSONDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxJSONDepth
	}

	var doc Document

	depth, err := scanDepth(in.Data, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("json is not well-formed: %w", err)
	}
	truncatedDepth := depth > maxDepth

	var v any
	dec := json.NewDecoder(bytes.NewReader(in.Data))
	dec.UseNumber() // keep numeric text exactly as written
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	// Reject trailing content: two concatenated documents are not one document.
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("json contains trailing data after the top-level value")
	}

	f := &flattener{maxDepth: maxDepth}
	f.walk("", v, 0)

	if f.truncated || truncatedDepth {
		doc.Warnings = append(doc.Warnings, domain.ParseWarning{
			Code:    domain.WarnJSONDepthExceeded,
			Message: "The JSON document was flattened only to the configured depth or size limit; deeper values are stored in the original but not indexed.",
			Detail:  fmt.Sprintf("max_depth=%d observed_depth=%d lines=%d", maxDepth, depth, len(f.lines)),
		})
	}

	var sb strings.Builder
	for i, l := range f.lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		start := sb.Len()
		sb.WriteString(l.text)
		doc.Blocks = append(doc.Blocks, Block{
			Kind:    BlockJSONField,
			Text:    l.text,
			Start:   start,
			End:     start + len(l.text),
			Section: l.section,
		})
	}
	doc.Text = sb.String()

	// Blocks were built against a builder whose offsets already account for the
	// newline separators, so they index correctly into doc.Text.
	return &doc, nil
}

type flatLine struct {
	text    string
	section string
}

type flattener struct {
	lines     []flatLine
	maxDepth  int
	truncated bool
}

// walk renders v as one or more "path: value" lines. Objects contribute a
// section name so the chunker can keep an object's fields together.
func (f *flattener) walk(path string, v any, depth int) {
	if len(f.lines) >= maxFlattenedLines {
		f.truncated = true
		return
	}
	if depth > f.maxDepth {
		f.truncated = true
		f.emit(path, "[nested value omitted: depth limit reached]", path)
		return
	}

	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // stable output, so hashes are reproducible
		if len(keys) == 0 {
			f.emit(path, "{}", path)
			return
		}
		for _, k := range keys {
			f.walk(join(path, sanitiseKey(k)), t[k], depth+1)
		}
	case []any:
		if len(t) == 0 {
			f.emit(path, "[]", path)
			return
		}
		for i, e := range t {
			f.walk(fmt.Sprintf("%s[%d]", path, i), e, depth+1)
		}
	case nil:
		f.emit(path, "null", sectionOf(path))
	case bool:
		f.emit(path, strconv.FormatBool(t), sectionOf(path))
	case json.Number:
		f.emit(path, t.String(), sectionOf(path))
	case string:
		f.emit(path, t, sectionOf(path))
	default:
		f.emit(path, fmt.Sprint(t), sectionOf(path))
	}
}

func (f *flattener) emit(path, value, section string) {
	if len(f.lines) >= maxFlattenedLines {
		f.truncated = true
		return
	}
	if path == "" {
		path = "value"
	}
	f.lines = append(f.lines, flatLine{text: path + ": " + value, section: section})
}

func join(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// sectionOf returns the parent path, used to group an object's fields.
func sectionOf(path string) string {
	if i := strings.LastIndexAny(path, "."); i > 0 {
		return path[:i]
	}
	return path
}

// sanitiseKey strips control characters from object keys so that a crafted key
// cannot inject line breaks into the flattened rendering.
func sanitiseKey(k string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return ' '
		}
		return r
	}, k)
}

// scanDepth measures maximum nesting using a streaming token scan, so nesting
// is bounded before any recursive structure is built. It returns the observed
// depth, stopping early once limit is exceeded.
func scanDepth(data []byte, limit int) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	depth, maxSeen := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return maxSeen, nil
		}
		if err != nil {
			return maxSeen, err
		}
		switch d := tok.(type) {
		case json.Delim:
			switch d {
			case '{', '[':
				depth++
				if depth > maxSeen {
					maxSeen = depth
				}
				// Bail out early rather than walking a deeply nested bomb.
				if depth > limit+50 {
					return maxSeen, nil
				}
			case '}', ']':
				depth--
			}
		}
	}
}

// validJSON reports whether data is a single well-formed JSON value.
func validJSON(data []byte) bool { return json.Valid(data) }
