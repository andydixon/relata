package parsing

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/andydixon/relata/internal/domain"
)

func hasWarning(warns []domain.ParseWarning, code string) bool {
	for _, w := range warns {
		if w.Code == code {
			return true
		}
	}
	return false
}

// --- MIME sniffing ---------------------------------------------------------

func TestSniffPrefersContentOverDeclaredType(t *testing.T) {
	// A real PDF declared as plain text must still be parsed as a PDF.
	res, err := Sniff([]byte("%PDF-1.7\n%âãÏÓ\n"), "text/plain", "notes.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.MIME != "application/pdf" {
		t.Fatalf("content-based sniffing failed: got %q", res.MIME)
	}
	if !res.FromContent {
		t.Fatal("result should be marked as content-derived")
	}
}

func TestSniffRejectsArchivesDisguisedAsDocuments(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("payload.sh")
	_, _ = w.Write([]byte("rm -rf /"))
	_ = zw.Close()

	// Claimed as a DOCX, with a .docx name, but it has no OOXML part.
	_, err := Sniff(buf.Bytes(), docxMIME, "report.docx")
	if !errors.Is(err, ErrArchiveRejected) {
		t.Fatalf("archive masquerading as docx was accepted: %v", err)
	}
}

func TestSniffAcceptsGenuineDOCX(t *testing.T) {
	data := buildDOCX(t, `<w:p><w:r><w:t>Hello</w:t></w:r></w:p>`)
	res, err := Sniff(data, "application/octet-stream", "anything.bin")
	if err != nil {
		t.Fatalf("genuine docx rejected: %v", err)
	}
	if res.MIME != docxMIME {
		t.Fatalf("got %q, want docx", res.MIME)
	}
}

func TestSniffRejectsExecutables(t *testing.T) {
	for name, payload := range map[string][]byte{
		"elf":    {0x7f, 'E', 'L', 'F', 2, 1, 1, 0},
		"pe":     []byte("MZ\x90\x00\x03"),
		"script": []byte("#!/bin/sh\necho hi\n"),
		"macho":  {0xCA, 0xFE, 0xBA, 0xBE, 0, 0, 0, 2},
	} {
		if _, err := Sniff(payload, "text/plain", name+".txt"); !errors.Is(err, ErrExecutableRejected) {
			t.Fatalf("%s payload was not rejected: %v", name, err)
		}
	}
}

func TestSniffRejectsCompressedFormats(t *testing.T) {
	cases := map[string][]byte{
		"gzip":  {0x1f, 0x8b, 0x08},
		"bzip2": []byte("BZh9"),
		"xz":    {0xfd, '7', 'z', 'X', 'Z', 0x00},
		"rar":   []byte("Rar!\x1a\x07"),
		"7z":    {0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C},
	}
	for name, payload := range cases {
		if _, err := Sniff(payload, "application/octet-stream", name); !errors.Is(err, ErrArchiveRejected) {
			t.Fatalf("%s was not rejected: %v", name, err)
		}
	}
}

func TestSniffRejectsBinaryPosingAsText(t *testing.T) {
	binary := append([]byte("looks like text at first"), 0x00, 0x01, 0x02, 0xff, 0xfe)
	binary = append(binary, bytes.Repeat([]byte{0x00}, 200)...)
	if _, err := Sniff(binary, "text/plain", "notes.txt"); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("binary content accepted as text: %v", err)
	}
}

func TestSniffDetectsJSONByContent(t *testing.T) {
	res, err := Sniff([]byte(`{"cost": 42000, "vendor": "Example Ltd"}`), "text/plain", "data.txt")
	if err != nil {
		t.Fatal(err)
	}
	if res.MIME != "application/json" {
		t.Fatalf("got %q, want application/json", res.MIME)
	}
}

func TestSniffTreatsInvalidJSONAsText(t *testing.T) {
	// Declared JSON that does not parse must still be stored, as text.
	res, err := Sniff([]byte(`{"broken": `), "application/json", "data.json")
	if err != nil {
		t.Fatal(err)
	}
	if res.MIME != "text/plain" {
		t.Fatalf("got %q, want text/plain fallback", res.MIME)
	}
}

// --- Text ------------------------------------------------------------------

func TestTextParagraphOffsetsAddressTheirOwnText(t *testing.T) {
	body := "First paragraph here.\n\nSecond paragraph here.\n\nThird one."
	doc, err := Parse(Input{Data: []byte(body), MIMEType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Blocks) != 3 {
		t.Fatalf("expected 3 paragraphs, got %d", len(doc.Blocks))
	}
	for i, b := range doc.Blocks {
		if doc.Text[b.Start:b.End] != b.Text {
			t.Fatalf("block %d offsets do not match its text", i)
		}
	}
}

func TestTextPreservesContentExactly(t *testing.T) {
	body := "Line one\nLine two\n\nAfter blank"
	doc, _ := Parse(Input{Data: []byte(body), MIMEType: "text/plain"})
	if doc.Text != body {
		t.Fatalf("text was rewritten:\n got %q\nwant %q", doc.Text, body)
	}
}

func TestEmailHeadersBecomeTheirOwnBlock(t *testing.T) {
	body := "From: alice@example.com\nTo: bob@example.com\nSubject: Delivery date\nDate: Mon, 3 Mar 2025 09:00:00 +0000\n\nThe date has moved to 14 March."
	doc, err := Parse(Input{Data: []byte(body), MIMEType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Blocks) == 0 || doc.Blocks[0].Section != "headers" {
		t.Fatal("email headers were not recognised as a header block")
	}
	if !strings.Contains(doc.Blocks[0].Text, "Subject: Delivery date") {
		t.Fatal("header block lost the subject line")
	}
}

// --- Markdown --------------------------------------------------------------

func TestMarkdownBuildsSectionPaths(t *testing.T) {
	body := "# Policy\n\nIntro text.\n\n## Delivery\n\nDelivery text.\n\n### Exceptions\n\nException text.\n"
	doc, err := Parse(Input{Data: []byte(body), MIMEType: "text/markdown"})
	if err != nil {
		t.Fatal(err)
	}
	var deepest string
	for _, b := range doc.Blocks {
		if strings.Contains(b.Text, "Exception text") {
			deepest = b.Section
		}
	}
	if deepest != "Policy > Delivery > Exceptions" {
		t.Fatalf("section path wrong: %q", deepest)
	}
}

func TestMarkdownIsNeverConvertedToHTML(t *testing.T) {
	// Markdown containing raw HTML must come out as literal text; Relata never
	// renders uploaded markup.
	body := "# Title\n\n<script>alert(1)</script>\n\n[link](javascript:alert(2))\n"
	doc, err := Parse(Input{Data: []byte(body), MIMEType: "text/markdown"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Text, "<script>alert(1)</script>") {
		t.Fatal("content was altered by the parser")
	}
	for _, b := range doc.Blocks {
		if b.Kind == BlockCode {
			continue
		}
		if strings.Contains(b.Text, "<b>") {
			t.Fatal("parser produced markup")
		}
	}
}

func TestMarkdownListGroupsStayTogether(t *testing.T) {
	body := "Intro.\n\n- first item\n- second item\n- third item\n\nOutro.\n"
	doc, err := Parse(Input{Data: []byte(body), MIMEType: "text/markdown"})
	if err != nil {
		t.Fatal(err)
	}
	var list *Block
	for i := range doc.Blocks {
		if doc.Blocks[i].Kind == BlockList {
			list = &doc.Blocks[i]
		}
	}
	if list == nil {
		t.Fatal("no list block produced")
	}
	for _, want := range []string{"first item", "second item", "third item"} {
		if !strings.Contains(list.Text, want) {
			t.Fatalf("list block missing %q", want)
		}
	}
}

// --- JSON ------------------------------------------------------------------

func TestJSONFlattensToReadablePaths(t *testing.T) {
	body := `{"project":{"name":"Atlas","cost":42000},"vendor":"Example Ltd"}`
	doc, err := Parse(Input{Data: []byte(body), MIMEType: "application/json"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"project.name: Atlas", "project.cost: 42000", "vendor: Example Ltd"} {
		if !strings.Contains(doc.Text, want) {
			t.Fatalf("flattened output missing %q:\n%s", want, doc.Text)
		}
	}
	for i, b := range doc.Blocks {
		if doc.Text[b.Start:b.End] != b.Text {
			t.Fatalf("json block %d offsets do not match its text", i)
		}
	}
}

func TestJSONDepthIsBounded(t *testing.T) {
	// Deeply nested input must not recurse without limit.
	deep := strings.Repeat(`{"a":`, 500) + `1` + strings.Repeat(`}`, 500)
	doc, err := Parse(Input{Data: []byte(deep), MIMEType: "application/json", MaxJSONDepth: 10})
	if err != nil {
		t.Fatalf("deep json should parse with a warning, not fail: %v", err)
	}
	if !hasWarning(doc.Warnings, domain.WarnJSONDepthExceeded) {
		t.Fatal("depth limit was hit but not reported to the user")
	}
}

func TestJSONRejectsTrailingData(t *testing.T) {
	_, err := Parse(Input{Data: []byte(`{"a":1} {"b":2}`), MIMEType: "application/json"})
	if err == nil {
		t.Fatal("two concatenated documents were accepted as one")
	}
}

func TestJSONOutputIsDeterministic(t *testing.T) {
	body := `{"z":1,"a":2,"m":3}`
	first, _ := Parse(Input{Data: []byte(body), MIMEType: "application/json"})
	second, _ := Parse(Input{Data: []byte(body), MIMEType: "application/json"})
	if first.Text != second.Text {
		t.Fatal("flattening is not deterministic, so content hashes would be unstable")
	}
}

func TestJSONKeysCannotInjectLineBreaks(t *testing.T) {
	body := "{\"evil\\nkey: injected\": \"x\"}"
	doc, err := Parse(Input{Data: []byte(body), MIMEType: "application/json"})
	if err != nil {
		t.Fatal(err)
	}
	// The crafted key must not create a second logical line.
	if strings.Count(doc.Text, "\n") != 0 {
		t.Fatalf("newline injected via object key:\n%q", doc.Text)
	}
}

// --- PDF -------------------------------------------------------------------

func TestMalformedPDFFailsCleanlyWithoutPanic(t *testing.T) {
	cases := map[string][]byte{
		"header only":     []byte("%PDF-1.4\n"),
		"truncated":       []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog"),
		"garbage body":    append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte{0xff, 0x00, 0xAB}, 500)...),
		"fake xref":       []byte("%PDF-1.4\ntrailer\n<< /Size 99999999 >>\nstartxref\n999999\n%%EOF"),
		"negative length": []byte("%PDF-1.5\n1 0 obj\n<< /Length -5 >>\nstream\nxx\nendstream\nendobj\n"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse(Input{Data: data, MIMEType: "application/pdf"})
			// Either outcome is acceptable; a panic or hang is not.
			if err != nil {
				if doc == nil || !hasWarning(doc.Warnings, domain.WarnParseFailed) {
					t.Fatal("parse failure was not recorded as a warning")
				}
				return
			}
			if doc == nil {
				t.Fatal("nil document with nil error")
			}
		})
	}
}

// --- DOCX ------------------------------------------------------------------

func buildDOCX(t *testing.T, bodyXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		bodyXML + `</w:body></w:document>`
	if _, err := w.Write([]byte(xml)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDOCXExtractsParagraphsAndHeadings(t *testing.T) {
	body := `<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Delivery policy</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>The revised date is 14 March 2025.</w:t></w:r></w:p>` +
		`<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:t>Exceptions</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Contact operations.</w:t></w:r></w:p>`

	doc, err := Parse(Input{Data: buildDOCX(t, body), MIMEType: docxMIME})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Text, "14 March 2025") {
		t.Fatalf("body text missing:\n%s", doc.Text)
	}

	var headings int
	var deepSection string
	for _, b := range doc.Blocks {
		if b.Kind == BlockHeading {
			headings++
		}
		if strings.Contains(b.Text, "Contact operations") {
			deepSection = b.Section
		}
	}
	if headings != 2 {
		t.Fatalf("expected 2 headings, got %d", headings)
	}
	if deepSection != "Delivery policy > Exceptions" {
		t.Fatalf("section path wrong: %q", deepSection)
	}
	for i, b := range doc.Blocks {
		if doc.Text[b.Start:b.End] != b.Text {
			t.Fatalf("docx block %d offsets do not match its text", i)
		}
	}
}

func TestDOCXSplitRunsAreJoined(t *testing.T) {
	// Word routinely splits a sentence across runs; they must rejoin.
	body := `<w:p><w:r><w:t>The cost is </w:t></w:r><w:r><w:t>£42,000</w:t></w:r><w:r><w:t> excluding VAT.</w:t></w:r></w:p>`
	doc, err := Parse(Input{Data: buildDOCX(t, body), MIMEType: docxMIME})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Text, "The cost is £42,000 excluding VAT.") {
		t.Fatalf("runs were not joined: %q", doc.Text)
	}
}

func TestDOCXWithoutMainPartIsRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/other.xml")
	_, _ = w.Write([]byte("<x/>"))
	_ = zw.Close()

	if _, err := Parse(Input{Data: buf.Bytes(), MIMEType: docxMIME}); err == nil {
		t.Fatal("a zip without word/document.xml was accepted as a docx")
	}
}

func TestDOCXDoesNotResolveExternalEntities(t *testing.T) {
	// An XXE attempt must not read a local file or make a network call.
	body := `<w:p><w:r><w:t>&xxe;</w:t></w:r></w:p>`
	xml := `<?xml version="1.0"?><!DOCTYPE d [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body + `</w:body></w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	_, _ = w.Write([]byte(xml))
	_ = zw.Close()

	doc, err := Parse(Input{Data: buf.Bytes(), MIMEType: docxMIME})
	if err == nil && doc != nil {
		if strings.Contains(doc.Text, "root:") || strings.Contains(doc.Text, "/bin/") {
			t.Fatal("external entity was resolved: local file contents leaked into extracted text")
		}
	}
}

// --- Limits and quality ----------------------------------------------------

func TestTextTruncationIsReportedAndOffsetsStayValid(t *testing.T) {
	body := strings.Repeat("Long content that goes on and on. ", 500)
	doc, err := Parse(Input{Data: []byte(body), MIMEType: "text/plain", MaxTextBytes: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Text) > 500 {
		t.Fatalf("text not truncated: %d bytes", len(doc.Text))
	}
	if !hasWarning(doc.Warnings, domain.WarnTruncatedText) {
		t.Fatal("truncation was not reported")
	}
	for i, b := range doc.Blocks {
		if b.End > len(doc.Text) || doc.Text[b.Start:b.End] != b.Text {
			t.Fatalf("block %d survived truncation with bad offsets", i)
		}
	}
}

func TestLowExtractableTextIsReported(t *testing.T) {
	// A large file yielding almost no letters looks like a scan.
	doc := &Document{Text: "  \n  x  \n "}
	checkExtractionQuality(doc, 500_000)
	if !hasWarning(doc.Warnings, domain.WarnLowExtractableText) {
		t.Fatal("a scan-like document was not flagged")
	}
}

func TestUnsupportedTypeIsRejected(t *testing.T) {
	_, err := Parse(Input{Data: []byte("data"), MIMEType: "image/png"})
	if !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("expected ErrUnsupportedType, got %v", err)
	}
}

// --- Fuzzing ---------------------------------------------------------------

// FuzzParseText exercises the text and markdown paths for panics and for
// offsets that do not address their own text.
func FuzzParseText(f *testing.F) {
	f.Add("hello world")
	f.Add("# heading\n\n- a\n- b\n\n```\ncode\n```\n")
	f.Add("From: a@b\nTo: c@d\n\nbody")
	f.Add("\x00\x01binary-ish")
	f.Add(strings.Repeat("a\n\n", 100))

	f.Fuzz(func(t *testing.T, s string) {
		for _, mime := range []string{"text/plain", "text/markdown"} {
			doc, err := Parse(Input{Data: []byte(s), MIMEType: mime, MaxTextBytes: 4096})
			if err != nil {
				continue
			}
			for _, b := range doc.Blocks {
				if b.Start < 0 || b.End > len(doc.Text) || b.Start > b.End {
					t.Fatalf("block offsets [%d,%d) out of range for %d bytes (mime %s)",
						b.Start, b.End, len(doc.Text), mime)
				}
				if doc.Text[b.Start:b.End] != b.Text {
					t.Fatalf("block text/offset mismatch (mime %s)", mime)
				}
			}
		}
	})
}

// FuzzParseJSON exercises the JSON flattener, which handles the most
// structurally adversarial input Relata accepts as a first-class format.
func FuzzParseJSON(f *testing.F) {
	f.Add(`{"a":1}`)
	f.Add(`[1,[2,[3,[4]]]]`)
	f.Add(`{"":{"":{"":null}}}`)
	f.Add(`{"a":"\u0000\n\r"}`)

	f.Fuzz(func(t *testing.T, s string) {
		doc, err := Parse(Input{Data: []byte(s), MIMEType: "application/json", MaxJSONDepth: 8, MaxTextBytes: 8192})
		if err != nil {
			return
		}
		for _, b := range doc.Blocks {
			if b.Start < 0 || b.End > len(doc.Text) || b.Start > b.End {
				t.Fatalf("json block offsets [%d,%d) out of range for %d bytes", b.Start, b.End, len(doc.Text))
			}
			if doc.Text[b.Start:b.End] != b.Text {
				t.Fatal("json block text/offset mismatch")
			}
		}
	})
}

// FuzzSniff checks that type detection never panics and never accepts an
// archive or executable, whatever the input.
func FuzzSniff(f *testing.F) {
	f.Add([]byte("%PDF-1.4"), "application/pdf", "a.pdf")
	f.Add([]byte("PK\x03\x04"), "application/zip", "a.zip")
	f.Add([]byte("{}"), "application/json", "a.json")

	f.Fuzz(func(t *testing.T, data []byte, declared, name string) {
		res, err := Sniff(data, declared, name)
		if err != nil {
			return
		}
		if res.MIME != "" && !Supported(res.MIME) {
			t.Fatalf("sniff accepted an unsupported type: %q", res.MIME)
		}
	})
}
