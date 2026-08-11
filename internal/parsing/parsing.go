// Package parsing extracts text from supported source formats.
//
// Every parser treats its input as attacker-controlled. Parsers never execute
// content, never follow URLs found inside documents, and never fetch remote
// resources. When extraction fails or degrades, the failure is recorded as a
// warning and the original bytes are preserved by the caller — content is never
// silently discarded.
package parsing

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/andydixon/relata/internal/domain"
)

// Version identifies the parser generation. It is stamped onto every item so
// that a parser change can be detected and the item reprocessed.
const Version = "parser-v1"

// BlockKind describes the structural role of a text block. The chunker uses it
// to prefer semantically meaningful boundaries.
type BlockKind string

const (
	BlockHeading   BlockKind = "heading"
	BlockParagraph BlockKind = "paragraph"
	BlockList      BlockKind = "list"
	BlockCode      BlockKind = "code"
	BlockTable     BlockKind = "table"
	BlockQuote     BlockKind = "quote"
	BlockJSONField BlockKind = "json_field"
)

// Block is a contiguous run of extracted text with its structural context.
// Start and End are byte offsets into Document.Text.
type Block struct {
	Kind    BlockKind
	Text    string
	Start   int
	End     int
	Level   int    // heading depth, 1..6
	Page    int    // 1-based; 0 when unknown
	Section string // nearest enclosing heading path
}

// Document is a parser's output.
type Document struct {
	// Text is the extracted plain text. Block offsets index into this string.
	Text     string
	Blocks   []Block
	Warnings []domain.ParseWarning
	// PageCount is set for paginated formats.
	PageCount int
	// Detected is the MIME type the parser actually handled, which may differ
	// from what the client claimed.
	Detected string
	Version  string
}

// Input is a document to parse.
type Input struct {
	Data []byte
	// MIMEType is the resolved type. Callers should sniff before parsing.
	MIMEType string
	Filename string
	// MaxTextBytes truncates extracted output; 0 means unlimited.
	MaxTextBytes int
	// MaxJSONDepth bounds JSON nesting; 0 uses the default.
	MaxJSONDepth int
}

// ErrUnsupportedType is returned for a MIME type no parser handles.
var ErrUnsupportedType = errors.New("unsupported content type")

// Parser extracts a Document from raw bytes.
type Parser interface {
	Parse(in Input) (*Document, error)
	Handles(mime string) bool
	Name() string
}

// Parse dispatches to the parser matching in.MIMEType.
//
// A parser failure is not fatal: the caller is expected to preserve the original
// bytes and surface the returned warning. Parse returns a Document with a
// parse_failed warning and empty text rather than nothing at all, so that the
// item still exists and can be reprocessed later.
func Parse(in Input) (*Document, error) {
	var p Parser
	switch {
	case isTextLike(in.MIMEType):
		p = TextParser{}
	case isMarkdown(in.MIMEType, in.Filename):
		p = MarkdownParser{}
	case isJSON(in.MIMEType):
		p = JSONParser{}
	case isPDF(in.MIMEType):
		p = PDFParser{}
	case isDOCX(in.MIMEType):
		p = DOCXParser{}
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, in.MIMEType)
	}

	doc, err := p.Parse(in)
	if err != nil {
		return &Document{
			Detected: in.MIMEType,
			Version:  Version,
			Warnings: []domain.ParseWarning{{
				Code:    domain.WarnParseFailed,
				Message: "Text could not be extracted from this file. The original is preserved unchanged.",
				Detail:  truncate(err.Error(), 400),
			}},
		}, err
	}
	doc.Version = Version
	if doc.Detected == "" {
		doc.Detected = in.MIMEType
	}
	applyLimits(doc, in.MaxTextBytes)
	checkExtractionQuality(doc, len(in.Data))
	return doc, nil
}

func isMarkdown(mime, filename string) bool {
	if mime == "text/markdown" || mime == "text/x-markdown" {
		return true
	}
	lf := strings.ToLower(filename)
	return strings.HasSuffix(lf, ".md") || strings.HasSuffix(lf, ".markdown")
}

func isTextLike(mime string) bool {
	switch mime {
	case "text/plain", "text/csv", "message/rfc822", "text/x-log":
		return true
	}
	return false
}

func isJSON(mime string) bool {
	return mime == "application/json" || mime == "text/json" || strings.HasSuffix(mime, "+json")
}

func isPDF(mime string) bool { return mime == "application/pdf" }

func isDOCX(mime string) bool {
	return mime == "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
}

// applyLimits truncates extracted text to max bytes, dropping any blocks that
// fall entirely beyond the cut and clamping the one that straddles it.
func applyLimits(doc *Document, max int) {
	if max <= 0 || len(doc.Text) <= max {
		return
	}
	cut := max
	// Do not split a UTF-8 rune.
	for cut > 0 && !utf8.RuneStart(doc.Text[cut]) {
		cut--
	}
	doc.Text = doc.Text[:cut]

	kept := doc.Blocks[:0]
	for _, b := range doc.Blocks {
		if b.Start >= cut {
			continue
		}
		if b.End > cut {
			b.End = cut
			b.Text = doc.Text[b.Start:cut]
		}
		kept = append(kept, b)
	}
	doc.Blocks = kept
	doc.Warnings = append(doc.Warnings, domain.ParseWarning{
		Code:    domain.WarnTruncatedText,
		Message: "Extracted text exceeded the configured limit and was truncated for indexing.",
		Detail:  fmt.Sprintf("truncated to %d bytes; the original file is stored unchanged", max),
	})
}

// checkExtractionQuality flags documents that yielded implausibly little text
// for their size — most often a scanned PDF. Relata does not OCR in this
// version, so the condition is reported rather than worked around.
func checkExtractionQuality(doc *Document, rawSize int) {
	if rawSize < 4096 {
		return
	}
	printable := 0
	for _, r := range doc.Text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			printable++
		}
	}
	// A text-bearing document of this size should yield far more than this.
	minExpected := rawSize / 100
	if minExpected < 80 {
		minExpected = 80
	}
	if printable < minExpected {
		doc.Warnings = append(doc.Warnings, domain.ParseWarning{
			Code: domain.WarnLowExtractableText,
			Message: "Very little machine-readable text was extracted. " +
				"If this is a scanned document, Relata does not perform OCR in this version, " +
				"so its contents will not be searchable.",
			Detail: fmt.Sprintf("%d alphanumeric characters extracted from %d bytes", printable, rawSize),
		})
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
