package ingest

import (
	"strings"
	"testing"
)

func TestSanitiseFilenameStripsPathTraversal(t *testing.T) {
	cases := map[string]string{
		"../../../etc/passwd":                    "passwd",
		"..\\..\\..\\windows\\system32\\cmd.exe": "cmd.exe",
		"/etc/shadow":                            "shadow",
		"foo/../../bar.txt":                      "bar.txt",
		"normal.pdf":                             "normal.pdf",
		"with spaces.docx":                       "with spaces.docx",
		"unicode-ünïcödé.txt":                    "unicode-ünïcödé.txt",
	}
	for in, want := range cases {
		if got := SanitiseFilename(in); got != want {
			t.Errorf("SanitiseFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitiseFilenameRemovesDangerousCharacters(t *testing.T) {
	hostile := []string{
		"file\x00.txt", "file\nname.txt", "file\rname.txt",
		"a:b.txt", "a*b.txt", "a?b.txt", `a"b.txt`, "a<b>.txt", "a|b.txt",
	}
	for _, in := range hostile {
		got := SanitiseFilename(in)
		for _, bad := range []string{"\x00", "\n", "\r", "/", "\\", ":", "*", "?", `"`, "<", ">", "|"} {
			if strings.Contains(got, bad) {
				t.Errorf("SanitiseFilename(%q) = %q, which still contains %q", in, got, bad)
			}
		}
	}
}

func TestSanitiseFilenameRefusesHiddenAndReservedNames(t *testing.T) {
	// A leading dot creates a hidden file and can conceal the real extension.
	for _, in := range []string{".hidden", "...", ". leading space.txt"} {
		if got := SanitiseFilename(in); strings.HasPrefix(got, ".") {
			t.Errorf("SanitiseFilename(%q) = %q, still hidden", in, got)
		}
	}
	// Windows device names cause surprising behaviour if the file is ever
	// copied to such a system.
	for _, in := range []string{"CON", "PRN", "AUX", "NUL", "COM1", "LPT1", "con.txt", "nul.pdf"} {
		got := SanitiseFilename(in)
		if strings.EqualFold(got, in) {
			t.Errorf("reserved device name %q was passed through unchanged", in)
		}
	}
}

func TestSanitiseFilenameHandlesDegenerateInput(t *testing.T) {
	for _, in := range []string{"", ".", "..", "/", "//", "   ", "...."} {
		got := SanitiseFilename(in)
		if strings.ContainsAny(got, "/\\") || got == ".." || got == "." {
			t.Errorf("SanitiseFilename(%q) = %q, which is not a safe base name", in, got)
		}
	}
}

func TestSanitiseFilenameIsBounded(t *testing.T) {
	long := strings.Repeat("a", 5000) + ".txt"
	if got := SanitiseFilename(long); len(got) > 200 {
		t.Fatalf("a %d-character name produced %d characters", len(long), len(got))
	}
}

func TestSanitiseFilenameIsIdempotent(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", "CON", ".hidden", "normal.pdf", "a:b*c.txt"} {
		once := SanitiseFilename(in)
		if twice := SanitiseFilename(once); once != twice {
			t.Errorf("not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func FuzzSanitiseFilename(f *testing.F) {
	f.Add("../../etc/passwd")
	f.Add("normal.pdf")
	f.Add("CON")
	f.Add("\x00\xff")
	f.Add(strings.Repeat("../", 100) + "x")

	f.Fuzz(func(t *testing.T, name string) {
		got := SanitiseFilename(name)

		// Whatever the input, the result must be usable as a bare base name.
		for _, forbidden := range []string{"/", "\\", "\x00"} {
			if strings.Contains(got, forbidden) {
				t.Fatalf("SanitiseFilename(%q) = %q, containing %q", name, got, forbidden)
			}
		}
		if got == "." || got == ".." || strings.HasPrefix(got, ".") {
			t.Fatalf("SanitiseFilename(%q) = %q, which is a relative or hidden path", name, got)
		}
		if len(got) > 200 {
			t.Fatalf("SanitiseFilename(%q) produced %d characters", name, len(got))
		}
	})
}

func TestStripControlRemovesInvisibleCharacters(t *testing.T) {
	got := stripControl("  hello\x00\x07world\x1b  ")
	if strings.ContainsAny(got, "\x00\x07\x1b") {
		t.Fatalf("control characters survived: %q", got)
	}
	if got != "helloworld" {
		t.Fatalf("stripControl produced %q", got)
	}
	// Newlines and tabs are legitimate in a pasted note and must survive.
	if stripControl("line one\nline two\tindented") != "line one\nline two\tindented" {
		t.Fatal("stripControl removed a legitimate newline or tab")
	}
}

func TestMetadataKeyPatternRejectsOperatorShapedKeys(t *testing.T) {
	valid := []string{"project", "department", "a", "a1", "a.b", "a-b", "a_b", strings.Repeat("k", 64)}
	for _, k := range valid {
		if !metadataKeyPattern.MatchString(k) {
			t.Errorf("valid metadata key %q was rejected", k)
		}
	}

	// A key MongoDB would treat specially, or that carries markup, must not
	// reach storage.
	invalid := []string{
		"", "$ne", "$where", "$gt", ".leading", "-leading", "_leading",
		"has space", "has\nnewline", "<script>", "a/b", "a\\b",
		strings.Repeat("k", 65), "键", "a$b",
	}
	for _, k := range invalid {
		if metadataKeyPattern.MatchString(k) {
			t.Errorf("invalid metadata key %q was accepted", k)
		}
	}
}

func TestSourceTypeInference(t *testing.T) {
	cases := map[string]string{
		"application/json": "json",
		"text/markdown":    "markdown",
		"application/pdf":  "document",
		"text/plain":       "note",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "document",
	}
	for mime, want := range cases {
		if got := string(inferSourceType(mime)); got != want {
			t.Errorf("inferSourceType(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestTextualTypesAreStoredInline(t *testing.T) {
	// Text is kept on the item; binary formats go to disk. Getting this wrong
	// would either lose content or put arbitrary bytes in a database field.
	for _, mime := range []string{"text/plain", "text/markdown", "application/json"} {
		if !isTextual(mime) {
			t.Errorf("%q should be stored inline", mime)
		}
	}
	for _, mime := range []string{"application/pdf",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document"} {
		if isTextual(mime) {
			t.Errorf("%q should be stored on disk, not inline", mime)
		}
	}
}

func TestTruncateRespectsRuneBoundaries(t *testing.T) {
	s := strings.Repeat("é", 100) // two bytes each
	got := truncate(s, 15)
	for _, r := range got {
		if r == '�' {
			t.Fatalf("truncation split a rune: %q", got)
		}
	}
	if len(got) > 20 {
		t.Fatalf("truncate produced %d bytes for a limit of 15", len(got))
	}
}
