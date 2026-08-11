package parsing

import (
	"archive/zip"
	"bytes"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Errors returned by MIME resolution.
var (
	// ErrArchiveRejected is returned for archive formats, which are out of scope
	// for the MVP. Accepting them would mean walking untrusted archive entries,
	// with the zip-slip and decompression-bomb handling that implies.
	ErrArchiveRejected = errors.New("archive formats are not accepted")
	// ErrExecutableRejected is returned for executable content.
	ErrExecutableRejected = errors.New("executable content is not accepted")
)

// SupportedMIMETypes are the types Relata will ingest.
var SupportedMIMETypes = []string{
	"text/plain",
	"text/markdown",
	"application/json",
	"application/pdf",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
}

const docxMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// SniffResult is the outcome of content-based type detection.
type SniffResult struct {
	// MIME is the resolved type, decided primarily from file content.
	MIME string
	// Declared is what the client claimed, retained for auditing.
	Declared string
	// FromContent reports whether MIME came from magic bytes rather than a hint.
	FromContent bool
}

// Sniff resolves the MIME type of data, preferring content inspection over the
// client-supplied type and filename. A browser-supplied Content-Type is a hint,
// never an authority: a .pdf named file containing a zip must be seen as a zip.
func Sniff(data []byte, declared, filename string) (SniffResult, error) {
	res := SniffResult{Declared: declared}

	// 1. Unambiguous magic numbers win outright.
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		res.MIME, res.FromContent = "application/pdf", true
		return res, nil
	case isELFOrPE(data):
		return res, ErrExecutableRejected
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}): // gzip
		return res, ErrArchiveRejected
	case bytes.HasPrefix(data, []byte("BZh")): // bzip2
		return res, ErrArchiveRejected
	case bytes.HasPrefix(data, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}): // xz
		return res, ErrArchiveRejected
	case bytes.HasPrefix(data, []byte("Rar!")): // rar
		return res, ErrArchiveRejected
	case bytes.HasPrefix(data, []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}): // 7z
		return res, ErrArchiveRejected
	}

	// 2. ZIP containers: DOCX is a zip, but so is every archive we reject.
	// Distinguish by looking for the OOXML word part, not by extension.
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) || bytes.HasPrefix(data, []byte("PK\x05\x06")) {
		if isDOCXZip(data) {
			res.MIME, res.FromContent = docxMIME, true
			return res, nil
		}
		return res, ErrArchiveRejected
	}

	// 3. Textual formats. Require valid UTF-8 without NUL bytes before treating
	// anything as text, so binary content cannot masquerade as a note.
	if !looksTextual(data) {
		detected := http.DetectContentType(data)
		res.MIME = detected
		res.FromContent = true
		return res, ErrUnsupportedType
	}

	trimmed := bytes.TrimSpace(data)
	if looksJSON(trimmed) {
		res.MIME, res.FromContent = "application/json", true
		return res, nil
	}

	// Between plain text and markdown the distinction is stylistic, so the
	// filename and declared type are allowed to decide.
	ext := strings.ToLower(filepath.Ext(filename))
	switch {
	case ext == ".md" || ext == ".markdown":
		res.MIME = "text/markdown"
	case declared == "text/markdown" || declared == "text/x-markdown":
		res.MIME = "text/markdown"
	case ext == ".json" || declared == "application/json":
		// Declared JSON that does not parse is still stored, but as text, with
		// the mismatch visible to the user.
		res.MIME = "text/plain"
	case looksMarkdown(trimmed):
		res.MIME, res.FromContent = "text/markdown", true
	default:
		res.MIME = "text/plain"
	}
	return res, nil
}

// Supported reports whether Relata has a parser for mime.
func Supported(mime string) bool {
	for _, m := range SupportedMIMETypes {
		if m == mime {
			return true
		}
	}
	return mime == "text/x-markdown" || mime == "text/json"
}

func isELFOrPE(data []byte) bool {
	return bytes.HasPrefix(data, []byte{0x7f, 'E', 'L', 'F'}) ||
		bytes.HasPrefix(data, []byte("MZ")) ||
		bytes.HasPrefix(data, []byte("#!")) ||
		bytes.HasPrefix(data, []byte{0xCA, 0xFE, 0xBA, 0xBE})
}

// isDOCXZip reports whether the zip contains the OOXML main document part.
// It reads only the central directory, so a malformed archive costs little.
func isDOCXZip(data []byte) bool {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			return true
		}
	}
	return false
}

// looksTextual reports whether data is plausibly text: valid UTF-8, no NULs,
// and few control characters.
func looksTextual(data []byte) bool {
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
		// Trim to a rune boundary so a split multi-byte rune is not misread.
		for len(sample) > 0 && !utf8.RuneStart(sample[len(sample)-1]) {
			sample = sample[:len(sample)-1]
		}
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	if !utf8.Valid(sample) {
		return false
	}
	ctrl := 0
	for _, b := range sample {
		if b < 0x09 || (b > 0x0d && b < 0x20) {
			ctrl++
		}
	}
	return len(sample) == 0 || ctrl*100/max(len(sample), 1) < 2
}

func looksJSON(t []byte) bool {
	if len(t) == 0 {
		return false
	}
	if t[0] != '{' && t[0] != '[' {
		return false
	}
	return validJSON(t)
}

var mdSignals = regexp.MustCompile(`(?m)^(#{1,6}\s+\S|[-*+]\s+\S|\d+\.\s+\S|>\s+\S|` + "```" + `)`)

func looksMarkdown(t []byte) bool {
	sample := t
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	return len(mdSignals.FindAll(sample, 3)) >= 2
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
