package parsing

import (
	"strings"

	"github.com/andydixon/relata/internal/domain"
)

// TextParser handles plain text, CSV-ish content, transcripts and pasted emails.
// It preserves paragraph boundaries and, for email-shaped input, records the
// header block as a section so it stays attached to the body.
type TextParser struct{}

// Name identifies the parser.
func (TextParser) Name() string { return "text" }

// Handles reports whether p can parse mime.
func (TextParser) Handles(mime string) bool { return isTextLike(mime) }

// Parse splits text into paragraph blocks, keeping byte offsets into the
// normalised text so every derived record can point back at its exact source.
func (TextParser) Parse(in Input) (*Document, error) {
	text := normaliseText(string(in.Data))
	doc := &Document{Text: text}

	section := ""
	if hdrEnd, ok := emailHeaderEnd(text); ok {
		section = "headers"
		doc.Blocks = append(doc.Blocks, Block{
			Kind:    BlockParagraph,
			Text:    text[:hdrEnd],
			Start:   0,
			End:     hdrEnd,
			Section: "headers",
		})
	}

	for _, seg := range splitParagraphs(text) {
		if seg.start < len(doc.Text) && len(doc.Blocks) > 0 && doc.Blocks[0].Section == "headers" && seg.end <= doc.Blocks[0].End {
			continue // already captured in the header block
		}
		body := text[seg.start:seg.end]
		if strings.TrimSpace(body) == "" {
			continue
		}
		kind := BlockParagraph
		if isListBlock(body) {
			kind = BlockList
		}
		sec := section
		if sec == "headers" {
			sec = "body"
		}
		doc.Blocks = append(doc.Blocks, Block{
			Kind:    kind,
			Text:    body,
			Start:   seg.start,
			End:     seg.end,
			Section: sec,
		})
	}

	if len(doc.Blocks) == 0 && strings.TrimSpace(text) != "" {
		doc.Blocks = append(doc.Blocks, Block{
			Kind: BlockParagraph, Text: text, Start: 0, End: len(text),
		})
	}
	return doc, nil
}

type span struct{ start, end int }

// splitParagraphs returns byte spans separated by blank lines.
func splitParagraphs(text string) []span {
	var out []span
	start := 0
	i := 0
	for i < len(text) {
		// A blank-line separator is "\n" followed by optional spaces then "\n".
		if text[i] == '\n' {
			j := i + 1
			for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\r') {
				j++
			}
			if j < len(text) && text[j] == '\n' {
				if i > start {
					out = append(out, span{start, i})
				}
				for j < len(text) && (text[j] == '\n' || text[j] == ' ' || text[j] == '\t' || text[j] == '\r') {
					j++
				}
				start = j
				i = j
				continue
			}
		}
		i++
	}
	if start < len(text) {
		out = append(out, span{start, len(text)})
	}
	return out
}

func isListBlock(s string) bool {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < 2 {
		return false
	}
	marked := 0
	for _, l := range lines {
		t := strings.TrimLeft(l, " \t")
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "+ ") ||
			(len(t) > 2 && t[0] >= '0' && t[0] <= '9' && (t[1] == '.' || t[1] == ')')) {
			marked++
		}
	}
	return marked >= 2 && marked*2 >= len(lines)
}

// emailHeaderEnd finds the end of an RFC-822-style header block, if the text
// begins with one. Recognising it keeps "From:"/"Date:" lines attached to the
// message rather than scattered across chunks.
func emailHeaderEnd(text string) (int, bool) {
	if len(text) == 0 {
		return 0, false
	}
	lines := strings.SplitAfter(text, "\n")
	headerish := 0
	pos := 0
	for _, l := range lines {
		trimmed := strings.TrimRight(l, "\r\n")
		if trimmed == "" {
			if headerish >= 2 {
				return pos, true
			}
			return 0, false
		}
		if k, _, ok := strings.Cut(trimmed, ":"); ok && k != "" && !strings.ContainsAny(k, " \t") {
			headerish++
		} else if !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			return 0, false
		}
		pos += len(l)
		if headerish > 30 {
			return 0, false
		}
	}
	return 0, false
}

// normaliseText makes line endings consistent and strips a UTF-8 BOM. It does
// not otherwise alter content: the item's original text is stored separately and
// is never rewritten.
func normaliseText(s string) string {
	s = strings.TrimPrefix(s, "\uFEFF")
	if strings.ContainsRune(s, '\r') {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
	}
	return s
}

// warn builds a parse warning.
func warn(code, msg, detail string) domain.ParseWarning {
	return domain.ParseWarning{Code: code, Message: msg, Detail: detail}
}
