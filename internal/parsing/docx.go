package parsing

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/andydixon/relata/internal/domain"
)

// DOCXParser extracts text from Office Open XML word documents.
//
// A DOCX is a zip. Only the single known part `word/document.xml` is read, by
// exact name — the archive is never walked and no entry path is ever joined
// with a filesystem path, so zip-slip does not apply. Decompressed size is
// capped so a compression bomb cannot exhaust memory.
type DOCXParser struct{}

// Name identifies the parser.
func (DOCXParser) Name() string { return "docx" }

// Handles reports whether p can parse mime.
func (DOCXParser) Handles(mime string) bool { return isDOCX(mime) }

// maxDOCXPartBytes caps the decompressed size of word/document.xml.
const maxDOCXPartBytes = 64 << 20

// Parse reads paragraph runs from the main document part, mapping Word's
// heading styles onto structural headings.
func (DOCXParser) Parse(in Input) (*Document, error) {
	zr, err := zip.NewReader(bytes.NewReader(in.Data), int64(len(in.Data)))
	if err != nil {
		return nil, fmt.Errorf("open docx container: %w", err)
	}

	var part *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			part = f
			break
		}
	}
	if part == nil {
		return nil, fmt.Errorf("docx is missing word/document.xml")
	}
	if part.UncompressedSize64 > maxDOCXPartBytes {
		return nil, fmt.Errorf("docx main part is too large: %d bytes", part.UncompressedSize64)
	}

	rc, err := part.Open()
	if err != nil {
		return nil, fmt.Errorf("open docx main part: %w", err)
	}
	defer rc.Close()

	// LimitReader guards against a lying uncompressed-size header.
	raw, err := io.ReadAll(io.LimitReader(rc, maxDOCXPartBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read docx main part: %w", err)
	}
	if int64(len(raw)) > maxDOCXPartBytes {
		return nil, fmt.Errorf("docx main part exceeded the %d byte limit when decompressed", int64(maxDOCXPartBytes))
	}

	paras, warns, err := parseDocumentXML(raw)
	if err != nil {
		return nil, err
	}

	doc := &Document{Warnings: warns}
	var sb strings.Builder
	var sectionPath []string

	for _, p := range paras {
		text := strings.TrimRight(p.text, " \t")
		if strings.TrimSpace(text) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		start := sb.Len()
		sb.WriteString(text)

		kind := BlockParagraph
		level := 0
		switch {
		case p.headingLevel > 0:
			kind, level = BlockHeading, p.headingLevel
			if level-1 < len(sectionPath) {
				sectionPath = sectionPath[:level-1]
			}
			for len(sectionPath) < level-1 {
				sectionPath = append(sectionPath, "")
			}
			sectionPath = append(sectionPath, text)
		case p.isList:
			kind = BlockList
		}

		doc.Blocks = append(doc.Blocks, Block{
			Kind:    kind,
			Text:    text,
			Start:   start,
			End:     start + len(text),
			Level:   level,
			Section: strings.Join(sectionPath, " > "),
		})
	}
	doc.Text = sb.String()

	if len(doc.Blocks) == 0 {
		doc.Warnings = append(doc.Warnings, warn(domain.WarnLowExtractableText,
			"No paragraph text was found in the Word document. If it contains only images, Relata does not perform OCR in this version.", ""))
	}
	return doc, nil
}

type docxPara struct {
	text         string
	headingLevel int
	isList       bool
}

// parseDocumentXML streams the WordprocessingML, concatenating text runs into
// paragraphs. Streaming (rather than unmarshalling the whole tree) keeps memory
// bounded on large documents.
func parseDocumentXML(raw []byte) ([]docxPara, []domain.ParseWarning, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	// Reject external entity references outright: this input is untrusted and
	// there is no legitimate reason for a DOCX part to pull in outside content.
	dec.Strict = false
	dec.Entity = map[string]string{}
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		// Only UTF-8 is expected; anything else is passed through as bytes
		// rather than resolved via an external charset table.
		return input, nil
	}

	var (
		paras    []docxPara
		warns    []domain.ParseWarning
		cur      strings.Builder
		inPara   bool
		inText   bool
		inStyle  bool
		curStyle string
		isList   bool
		depth    int
	)

	flush := func() {
		if !inPara {
			return
		}
		paras = append(paras, docxPara{
			text:         cur.String(),
			headingLevel: headingLevelFromStyle(curStyle),
			isList:       isList,
		})
		cur.Reset()
		curStyle = ""
		isList = false
		inPara = false
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Salvage what was parsed so far rather than losing the document.
			warns = append(warns, warn(domain.WarnParseFailed,
				"The Word document's XML ended unexpectedly; text extracted up to that point is indexed.",
				truncate(err.Error(), 200)))
			break
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > 1000 {
				return paras, warns, fmt.Errorf("docx xml nesting is implausibly deep")
			}
			switch t.Name.Local {
			case "p":
				flush()
				inPara = true
			case "t":
				inText = true
			case "pStyle":
				inStyle = true
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						curStyle = a.Value
					}
				}
			case "numPr":
				isList = true
			case "tab":
				if inPara {
					cur.WriteByte('\t')
				}
			case "br", "cr":
				if inPara {
					cur.WriteByte('\n')
				}
			}
		case xml.CharData:
			if inText && inPara {
				cur.Write(t)
			}
		case xml.EndElement:
			depth--
			switch t.Name.Local {
			case "p":
				flush()
			case "t":
				inText = false
			case "pStyle":
				inStyle = false
			}
		}
		_ = inStyle
	}
	flush()
	return paras, warns, nil
}

// headingLevelFromStyle maps Word style identifiers ("Heading1", "heading 2",
// "Title") onto a 1..6 structural level, returning 0 for body text.
func headingLevelFromStyle(style string) int {
	s := strings.ToLower(strings.ReplaceAll(style, " ", ""))
	switch s {
	case "title":
		return 1
	case "subtitle":
		return 2
	}
	if !strings.HasPrefix(s, "heading") {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "heading"))
	if err != nil || n < 1 || n > 6 {
		return 0
	}
	return n
}
