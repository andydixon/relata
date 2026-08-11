package entities

import (
	"regexp"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/domain"
)

// Mention is one occurrence of an entity in a piece of text.
type Mention struct {
	Name string
	Type domain.EntityType
	// Start and End are byte offsets into the text the mention was found in.
	Start, End int
	Quote      string
	Method     domain.ExtractionMethod
	// Normalised is the ISO-8601 form for date mentions.
	Normalised string
}

// Deterministic patterns.
//
// These are kept narrow on purpose. A pattern that fires on ordinary prose
// would flood the entity index with noise and make shared-entity evidence
// meaningless, so only high-precision shapes are matched here. Everything
// broader is left to the model stage, whose output is marked as such.
var (
	// A capitalised run terminated by a legal suffix: "Example Ltd",
	// "Northern Rail Holdings PLC".
	orgPattern = regexp.MustCompile(
		`\b((?:[A-Z][\p{L}&.'-]*\s+){0,5}[A-Z][\p{L}&.'-]*)\s+(Ltd\.?|Limited|PLC|plc|LLP|LLC|Inc\.?|Incorporated|Corp\.?|Corporation|GmbH|AG|B\.?V\.?|N\.?V\.?|Pty|Co\.)(?:\s|$|[.,;:!?)])`)

	// An honorific followed by capitalised names: "Dr Alice Chen".
	personPattern = regexp.MustCompile(
		`\b(Mr|Mrs|Ms|Miss|Dr|Prof|Professor|Sir|Dame|Lord|Lady)\.?\s+((?:[A-Z][\p{L}'-]+\s+){0,2}[A-Z][\p{L}'-]+)\b`)

	// Email addresses identify a correspondent unambiguously.
	emailPattern = regexp.MustCompile(`\b[\w.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[\p{L}\d](?:[\p{L}\d-]{0,61}[\p{L}\d])?(?:\.[\p{L}\d](?:[\p{L}\d-]{0,61}[\p{L}\d])?)+\b`)

	// Dates in the formats Relata can normalise with confidence.
	isoDatePattern = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	dmyPattern     = regexp.MustCompile(`\b(\d{1,2})(?:st|nd|rd|th)?\s+(January|February|March|April|May|June|July|August|September|October|November|December)\s+(\d{4})\b`)
	mdyPattern     = regexp.MustCompile(`\b(January|February|March|April|May|June|July|August|September|October|November|December)\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`)
	slashDate      = regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})/(\d{4})\b`)
)

// maxDeterministicMentions bounds output from a pathological document.
const maxDeterministicMentions = 500

// ExtractDeterministic finds entities that can be identified by rule alone.
//
// Every mention carries exact source offsets, so the caller can attach a
// verifiable source reference. Mentions are returned in document order.
func ExtractDeterministic(text string) []Mention {
	var out []Mention

	add := func(m Mention) {
		if len(out) >= maxDeterministicMentions {
			return
		}
		m.Method = domain.MethodDeterministic
		m.Quote = safeSlice(text, m.Start, m.End)
		out = append(out, m)
	}

	for _, loc := range orgPattern.FindAllStringSubmatchIndex(text, maxDeterministicMentions) {
		// Group 1 is the name body, group 2 the suffix; the entity is both.
		start, end := loc[2], loc[5]
		if start < 0 || end < 0 || end <= start {
			continue
		}
		name := strings.TrimSpace(text[start:end])
		if !plausibleName(name) {
			continue
		}
		add(Mention{Name: name, Type: domain.EntityOrganisation, Start: start, End: end})
	}

	for _, loc := range personPattern.FindAllStringSubmatchIndex(text, maxDeterministicMentions) {
		start, end := loc[4], loc[5] // the name without the honorific
		if start < 0 || end < 0 {
			continue
		}
		name := strings.TrimSpace(text[start:end])
		if !plausibleName(name) {
			continue
		}
		add(Mention{Name: name, Type: domain.EntityPerson, Start: start, End: end})
	}

	for _, loc := range emailPattern.FindAllStringIndex(text, maxDeterministicMentions) {
		addr := text[loc[0]:loc[1]]
		if len(addr) > 254 {
			continue
		}
		add(Mention{Name: strings.ToLower(addr), Type: domain.EntityPerson, Start: loc[0], End: loc[1]})
	}

	out = append(out, extractDates(text)...)
	sortMentions(out)
	return dedupeOverlapping(out)
}

// extractDates finds and normalises dates to ISO-8601.
//
// Ambiguous numeric formats are resolved as day/month/year, matching the
// application's British English conventions. The normalised value is what
// temporal comparisons use, so getting this wrong would silently distort
// relationship evidence — hence only unambiguous or documented formats.
func extractDates(text string) []Mention {
	var out []Mention

	emit := func(start, end int, t time.Time) {
		out = append(out, Mention{
			Name:       t.Format("2006-01-02"),
			Type:       domain.EntityDate,
			Start:      start,
			End:        end,
			Quote:      safeSlice(text, start, end),
			Method:     domain.MethodDeterministic,
			Normalised: t.Format("2006-01-02"),
		})
	}

	for _, loc := range isoDatePattern.FindAllStringSubmatchIndex(text, 200) {
		if t, err := time.Parse("2006-01-02", text[loc[0]:loc[1]]); err == nil {
			emit(loc[0], loc[1], t)
		}
	}
	for _, loc := range dmyPattern.FindAllStringSubmatchIndex(text, 200) {
		day := text[loc[2]:loc[3]]
		month := text[loc[4]:loc[5]]
		year := text[loc[6]:loc[7]]
		if t, err := time.Parse("2 January 2006", day+" "+month+" "+year); err == nil {
			emit(loc[0], loc[1], t)
		}
	}
	for _, loc := range mdyPattern.FindAllStringSubmatchIndex(text, 200) {
		month := text[loc[2]:loc[3]]
		day := text[loc[4]:loc[5]]
		year := text[loc[6]:loc[7]]
		if t, err := time.Parse("2 January 2006", day+" "+month+" "+year); err == nil {
			emit(loc[0], loc[1], t)
		}
	}
	for _, loc := range slashDate.FindAllStringSubmatchIndex(text, 200) {
		d, m, y := text[loc[2]:loc[3]], text[loc[4]:loc[5]], text[loc[6]:loc[7]]
		// Day/month/year. A value above 12 in the first position confirms the
		// reading; otherwise the convention is applied as documented.
		if t, err := time.Parse("2/1/2006", d+"/"+m+"/"+y); err == nil {
			emit(loc[0], loc[1], t)
		}
	}
	return out
}

// plausibleName filters out matches that are almost certainly not names:
// sentence-initial words, all-caps shouting, and absurd lengths.
func plausibleName(name string) bool {
	if len(name) < 2 || len(name) > 120 {
		return false
	}
	words := strings.Fields(name)
	if len(words) == 0 || len(words) > 8 {
		return false
	}
	// A single very common word that happens to be capitalised is not a name.
	if len(words) == 1 && commonWords[strings.ToLower(strings.Trim(words[0], ".,"))] {
		return false
	}
	return true
}

// commonWords are capitalised sentence-openers that would otherwise be matched.
var commonWords = map[string]bool{
	"the": true, "this": true, "that": true, "these": true, "those": true,
	"a": true, "an": true, "it": true, "we": true, "they": true, "he": true,
	"she": true, "i": true, "our": true, "their": true, "his": true, "her": true,
	"in": true, "on": true, "at": true, "to": true, "for": true, "and": true,
	"but": true, "if": true, "as": true, "by": true, "from": true, "with": true,
	"all": true, "any": true, "each": true, "no": true, "not": true, "one": true,
	"there": true, "here": true, "when": true, "where": true, "while": true,
	"after": true, "before": true, "during": true, "since": true, "until": true,
}

func safeSlice(s string, start, end int) string {
	if start < 0 || end > len(s) || start >= end {
		return ""
	}
	return s[start:end]
}

func sortMentions(ms []Mention) {
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && ms[j].Start < ms[j-1].Start; j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}
}

// dedupeOverlapping removes mentions contained within an earlier, longer
// mention at the same position, so "Example Ltd" is not also recorded as the
// person "Example".
func dedupeOverlapping(ms []Mention) []Mention {
	out := ms[:0]
	var lastEnd int
	var lastType domain.EntityType
	for _, m := range ms {
		if m.Start < lastEnd && m.Type != lastType {
			continue
		}
		out = append(out, m)
		if m.End > lastEnd {
			lastEnd = m.End
			lastType = m.Type
		}
	}
	return out
}
