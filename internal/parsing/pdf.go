package parsing

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/andydixon/relata/internal/domain"
	"github.com/ledongthuc/pdf"
)

// PDFParser extracts text from PDFs, recording the page each block came from.
//
// No OCR is performed in this version. A scanned PDF yields little or no text;
// that condition is detected and reported rather than hidden, and the original
// file is retained so it can be reprocessed if OCR is added later.
type PDFParser struct{}

// Name identifies the parser.
func (PDFParser) Name() string { return "pdf" }

// Handles reports whether p can parse mime.
func (PDFParser) Handles(mime string) bool { return isPDF(mime) }

// maxPDFPages bounds work on a document with an absurd page count.
const maxPDFPages = 2000

// Parse reads the PDF page by page. A page that fails to decode is skipped with
// a warning; one bad page does not discard the rest of the document.
func (PDFParser) Parse(in Input) (doc *Document, err error) {
	// The PDF library panics on some malformed files. Recover so that a hostile
	// or corrupt document becomes a recorded parse failure, not a crashed worker.
	defer func() {
		if recover() != nil {
			// Third-party panic values may contain fragments of attacker-supplied
			// input. Return a stable diagnostic rather than storing or logging them.
			doc, err = nil, fmt.Errorf("pdf structure is malformed")
		}
	}()

	r, perr := pdf.NewReader(bytes.NewReader(in.Data), int64(len(in.Data)))
	if perr != nil {
		return nil, fmt.Errorf("open pdf: %w", perr)
	}

	pages := r.NumPage()
	if pages < 0 {
		pages = 0
	}
	d := &Document{PageCount: pages}
	if pages > maxPDFPages {
		d.Warnings = append(d.Warnings, warn(domain.WarnTruncatedText,
			"The PDF has more pages than Relata will index; later pages were skipped.",
			fmt.Sprintf("page_count=%d indexed=%d", pages, maxPDFPages)))
		pages = maxPDFPages
	}

	var sb strings.Builder
	failedPages := 0

	for pageNo := 1; pageNo <= pages; pageNo++ {
		text, perr := extractPage(r, pageNo)
		if perr != nil {
			failedPages++
			continue
		}
		text = normaliseText(text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		// Emit one block per paragraph so that page and paragraph structure both
		// survive into chunking.
		base := sb.Len()
		if base > 0 {
			sb.WriteString("\n\n")
			base = sb.Len()
		}
		sb.WriteString(text)
		section := fmt.Sprintf("page %d", pageNo)
		for _, seg := range splitParagraphs(text) {
			body := text[seg.start:seg.end]
			if strings.TrimSpace(body) == "" {
				continue
			}
			d.Blocks = append(d.Blocks, Block{
				Kind:    BlockParagraph,
				Text:    body,
				Start:   base + seg.start,
				End:     base + seg.end,
				Page:    pageNo,
				Section: section,
			})
		}
	}

	d.Text = sb.String()
	if failedPages > 0 {
		d.Warnings = append(d.Warnings, warn(domain.WarnParseFailed,
			"Some pages could not be decoded and were skipped. The original file is preserved.",
			fmt.Sprintf("failed_pages=%d of %d", failedPages, pages)))
	}
	if pages == 0 {
		d.Warnings = append(d.Warnings, warn(domain.WarnLowExtractableText,
			"The PDF reported no pages, so no text could be extracted.", ""))
	}
	return d, nil
}

// extractPage pulls the text of one page, isolating panics to that page.
func extractPage(r *pdf.Reader, pageNo int) (text string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			text, err = "", fmt.Errorf("page %d: %v", pageNo, rec)
		}
	}()
	p := r.Page(pageNo)
	if p.V.IsNull() {
		return "", fmt.Errorf("page %d is null", pageNo)
	}
	// GetPlainText with a nil font map uses the page's own fonts.
	s, err := p.GetPlainText(nil)
	if err != nil {
		return "", err
	}
	return s, nil
}
