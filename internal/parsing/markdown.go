package parsing

import (
	"strings"
)

// MarkdownParser extracts text from Markdown while preserving heading structure.
//
// It is deliberately not a full CommonMark implementation: Relata needs
// paragraph and heading boundaries for chunking, not rendering. Markdown is
// never converted to HTML anywhere in Relata — stored content is escaped on
// output, so there is no path from a document to executable markup.
type MarkdownParser struct{}

// Name identifies the parser.
func (MarkdownParser) Name() string { return "markdown" }

// Handles reports whether p can parse mime.
func (MarkdownParser) Handles(mime string) bool { return isMarkdown(mime, "") }

// Parse walks the document line by line, emitting a block per heading,
// paragraph, list group, fenced code block and block quote.
func (MarkdownParser) Parse(in Input) (*Document, error) {
	text := normaliseText(string(in.Data))
	doc := &Document{Text: text}

	// sectionPath tracks the enclosing heading trail, e.g. "Policy > Delivery".
	var sectionPath []string

	lines := lineSpans(text)
	i := 0
	for i < len(lines) {
		ln := lines[i]
		raw := text[ln.start:ln.end]
		trimmed := strings.TrimSpace(raw)

		switch {
		case trimmed == "":
			i++

		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fence := trimmed[:3]
			start := ln.start
			i++
			for i < len(lines) {
				t := strings.TrimSpace(text[lines[i].start:lines[i].end])
				i++
				if strings.HasPrefix(t, fence) {
					break
				}
			}
			end := ln.end
			if i-1 < len(lines) {
				end = lines[min(i-1, len(lines)-1)].end
			}
			doc.Blocks = append(doc.Blocks, Block{
				Kind: BlockCode, Text: text[start:end], Start: start, End: end,
				Section: strings.Join(sectionPath, " > "),
			})

		case headingLevel(trimmed) > 0:
			lvl := headingLevel(trimmed)
			title := strings.TrimSpace(strings.TrimLeft(trimmed, "# "))
			// Truncate the path to the parent level, then push this heading.
			if lvl-1 < len(sectionPath) {
				sectionPath = sectionPath[:lvl-1]
			}
			for len(sectionPath) < lvl-1 {
				sectionPath = append(sectionPath, "")
			}
			sectionPath = append(sectionPath, title)
			hs, he := trimSpan(text, ln.start, ln.end)
			doc.Blocks = append(doc.Blocks, Block{
				Kind: BlockHeading, Text: text[hs:he], Start: hs, End: he,
				Level: lvl, Section: strings.Join(sectionPath, " > "),
			})
			i++

		case isSetextUnderline(text, lines, i):
			// "Title" followed by "=====" or "-----".
			lvl := 1
			if strings.HasPrefix(strings.TrimSpace(text[lines[i+1].start:lines[i+1].end]), "-") {
				lvl = 2
			}
			if lvl-1 < len(sectionPath) {
				sectionPath = sectionPath[:lvl-1]
			}
			sectionPath = append(sectionPath, trimmed)
			hs, he := trimSpan(text, ln.start, ln.end)
			doc.Blocks = append(doc.Blocks, Block{
				Kind: BlockHeading, Text: text[hs:he], Start: hs, End: he,
				Level: lvl, Section: strings.Join(sectionPath, " > "),
			})
			i += 2

		case isListLine(trimmed):
			start := ln.start
			end := ln.end
			for i < len(lines) {
				t := strings.TrimSpace(text[lines[i].start:lines[i].end])
				if t == "" || (!isListLine(t) && !strings.HasPrefix(text[lines[i].start:lines[i].end], "  ")) {
					break
				}
				end = lines[i].end
				i++
			}
			doc.Blocks = append(doc.Blocks, Block{
				Kind: BlockList, Text: text[start:end], Start: start, End: end,
				Section: strings.Join(sectionPath, " > "),
			})

		case strings.HasPrefix(trimmed, ">"):
			start := ln.start
			end := ln.end
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(text[lines[i].start:lines[i].end]), ">") {
				end = lines[i].end
				i++
			}
			doc.Blocks = append(doc.Blocks, Block{
				Kind: BlockQuote, Text: text[start:end], Start: start, End: end,
				Section: strings.Join(sectionPath, " > "),
			})

		case strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2:
			start := ln.start
			end := ln.end
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(text[lines[i].start:lines[i].end]), "|") {
				end = lines[i].end
				i++
			}
			doc.Blocks = append(doc.Blocks, Block{
				Kind: BlockTable, Text: text[start:end], Start: start, End: end,
				Section: strings.Join(sectionPath, " > "),
			})

		default:
			start := ln.start
			end := ln.end
			for i < len(lines) {
				t := strings.TrimSpace(text[lines[i].start:lines[i].end])
				if t == "" || headingLevel(t) > 0 || isListLine(t) || strings.HasPrefix(t, "```") {
					break
				}
				end = lines[i].end
				i++
			}
			doc.Blocks = append(doc.Blocks, Block{
				Kind: BlockParagraph, Text: text[start:end], Start: start, End: end,
				Section: strings.Join(sectionPath, " > "),
			})
		}
	}

	if len(doc.Blocks) == 0 && strings.TrimSpace(text) != "" {
		doc.Blocks = append(doc.Blocks, Block{Kind: BlockParagraph, Text: text, Start: 0, End: len(text)})
	}
	return doc, nil
}

// trimSpan narrows [start,end) past surrounding whitespace, so that a block's
// Text always equals text[Start:End]. Every derived record's source reference
// depends on that identity holding.
func trimSpan(text string, start, end int) (int, int) {
	for start < end && isSpaceByte(text[start]) {
		start++
	}
	for end > start && isSpaceByte(text[end-1]) {
		end--
	}
	return start, end
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

func lineSpans(text string) []span {
	var out []span
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			out = append(out, span{start, i})
			start = i + 1
		}
	}
	if start <= len(text) {
		out = append(out, span{start, len(text)})
	}
	return out
}

func headingLevel(trimmed string) int {
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0
	}
	if n >= len(trimmed) || (trimmed[n] != ' ' && trimmed[n] != '\t') {
		return 0
	}
	return n
}

func isListLine(t string) bool {
	if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "+ ") {
		return true
	}
	for i := 0; i < len(t) && i < 4; i++ {
		if t[i] >= '0' && t[i] <= '9' {
			continue
		}
		return i > 0 && (t[i] == '.' || t[i] == ')') && i+1 < len(t) && t[i+1] == ' '
	}
	return false
}

func isSetextUnderline(text string, lines []span, i int) bool {
	if i+1 >= len(lines) {
		return false
	}
	cur := strings.TrimSpace(text[lines[i].start:lines[i].end])
	next := strings.TrimSpace(text[lines[i+1].start:lines[i+1].end])
	if cur == "" || len(next) < 2 {
		return false
	}
	return strings.Trim(next, "=") == "" || strings.Trim(next, "-") == ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
