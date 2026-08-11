package entities

import (
	"regexp"
	"strconv"
	"strings"
)

// DeterministicClaim is a claim identified by rule rather than by a model.
type DeterministicClaim struct {
	Statement string
	Quote     string
	Start     int
	End       int
	// Subject is a coarse normalised key naming what the claim is about. Claims
	// sharing a subject are the ones worth comparing for agreement or conflict.
	Subject   string
	Predicate string
	Numbers   []float64
	Dates     []string
	// Polarity is +1 for an affirmative statement, -1 when negated.
	Polarity int
}

// maxSentencesScanned bounds work on a very long chunk.
const maxSentencesScanned = 400

// ExtractDeterministicClaims finds sentences that assert something checkable.
//
// The rule is narrow by design: a sentence qualifies when it carries a figure
// or a date, because those are the statements that can actually agree or
// conflict with another source. A sentence of pure prose is left to the model
// stage rather than recorded as a claim on weak grounds.
func ExtractDeterministicClaims(text string) []DeterministicClaim {
	var out []DeterministicClaim

	for i, sp := range splitSentences(text) {
		if i >= maxSentencesScanned {
			break
		}
		sentence := text[sp.start:sp.end]
		trimmed := strings.TrimSpace(sentence)
		if len(trimmed) < 12 || len(trimmed) > 600 {
			continue
		}

		subject, predicate, numbers, dates, polarity := AnalyseStatement(sentence)
		if len(numbers) == 0 && len(dates) == 0 {
			continue
		}
		if subject == "" {
			continue
		}

		out = append(out, DeterministicClaim{
			Statement: collapse(trimmed),
			Quote:     sentence,
			Start:     sp.start,
			End:       sp.end,
			Subject:   subject,
			Predicate: predicate,
			Numbers:   numbers,
			Dates:     dates,
			Polarity:  polarity,
		})
	}
	return out
}

// AnalyseStatement derives the comparison keys for a statement.
//
// The subject is the content word immediately governing the figure or date —
// "cost" in "the total project cost is £42,000", "date" in "the delivery date
// is 14 March". Two claims sharing a subject are candidates for comparison; the
// comparison itself only ever happens between items already linked by other
// evidence, so a broad subject cannot manufacture spurious pairs.
func AnalyseStatement(sentence string) (subject, predicate string, numbers []float64, dates []string, polarity int) {
	polarity = 1
	lower := strings.ToLower(sentence)
	for _, neg := range []string{" not ", " no ", "n't ", " never ", " cancelled", " withdrawn"} {
		if strings.Contains(lower, neg) {
			polarity = -1
			break
		}
	}

	// Dates first, so a year is not also counted as a bare number.
	dateSpans := map[int]bool{}
	for _, m := range ExtractDeterministic(sentence) {
		if m.Type == "date" && m.Normalised != "" {
			dates = append(dates, m.Normalised)
			for i := m.Start; i < m.End; i++ {
				dateSpans[i] = true
			}
		}
	}

	var anchor = -1
	for _, loc := range numberPattern.FindAllStringIndex(sentence, 40) {
		// Skip any match that OVERLAPS a date, not merely one that starts on
		// it: the number pattern can begin at the whitespace before a date and
		// take "202" out of "2025-02-10", which would then be compared against
		// another document's figures as if it were a quantity.
		if overlapsSpan(dateSpans, loc[0], loc[1]) {
			continue
		}
		raw := sentence[loc[0]:loc[1]]
		v, ok := parseNumber(raw)
		if !ok {
			continue
		}
		numbers = append(numbers, v)
		if anchor < 0 {
			anchor = loc[0]
		}
	}
	if anchor < 0 && len(dates) > 0 {
		for i := 0; i < len(sentence); i++ {
			if dateSpans[i] {
				anchor = i
				break
			}
		}
	}
	if anchor < 0 {
		return "", "", numbers, dates, polarity
	}

	// Decide which side of the figure the subject sits on.
	//
	// "the total project cost is £42,000" — the subject precedes the figure.
	// "will ship 240 units"              — the figure is a quantity and the
	//                                      unit noun follows it.
	// The distinguishing signal is what sits immediately before the number: a
	// verb means the figure is a direct object, so the noun after it is what
	// the claim is about.
	before := sentence[:anchor]
	words := wordPattern.FindAllString(before, -1)

	precededByVerb := false
	for i := len(words) - 1; i >= 0; i-- {
		w := strings.ToLower(words[i])
		if stopWords[w] {
			continue // skip "will", "is", "are"…
		}
		precededByVerb = badSubjects[w]
		break
	}

	var content []string
	if !precededByVerb {
		for i := len(words) - 1; i >= 0 && len(content) < 3; i-- {
			w := strings.ToLower(words[i])
			if stopWords[w] || badSubjects[w] || len(w) < 3 {
				continue
			}
			content = append([]string{w}, content...)
		}
	}
	if len(content) == 0 {
		// Either the figure is a quantity, or nothing meaningful precedes it.
		for _, w := range wordPattern.FindAllString(sentence[anchor:], 8) {
			lw := strings.ToLower(w)
			if !stopWords[lw] && !badSubjects[lw] && len(lw) >= 3 {
				content = append(content, lw)
				break
			}
		}
	}
	if len(content) == 0 {
		return "", "", numbers, dates, polarity
	}

	// The last content word is the subject key; keeping it single-word makes
	// "the cost is" and "the total project cost is" agree on "cost".
	subject = Normalise(content[len(content)-1])
	predicate = Normalise(strings.Join(content, " "))
	return subject, predicate, numbers, dates, polarity
}

var (
	// Currency-aware number matching: "£42,000", "42000", "3.5", "42%".
	numberPattern = regexp.MustCompile(`[£$€]?\s?\d{1,3}(?:,\d{3})*(?:\.\d+)?%?|[£$€]?\s?\d+(?:\.\d+)?%?`)
	wordPattern   = regexp.MustCompile(`[\p{L}][\p{L}'-]*`)
)

// parseNumber converts a matched numeric string to a float.
func parseNumber(raw string) (float64, bool) {
	s := strings.TrimSpace(raw)
	s = strings.NewReplacer("£", "", "$", "", "€", "", ",", "", "%", "", " ", "").Replace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// stopWords are function words that never make a useful subject key.
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"being": true, "will": true, "would": true, "shall": true, "should": true,
	"can": true, "could": true, "may": true, "might": true, "must": true,
	"has": true, "have": true, "had": true, "do": true, "does": true, "did": true,
	"of": true, "to": true, "in": true, "on": true, "at": true, "by": true,
	"for": true, "with": true, "from": true, "as": true, "that": true, "this": true,
	"these": true, "those": true, "it": true, "its": true, "we": true, "our": true,
	"they": true, "their": true, "he": true, "she": true, "his": true, "her": true,
	"there": true, "here": true, "now": true, "then": true, "than": true,
	"about": true, "into": true, "within": true, "per": true, "over": true, "under": true, "up": true, "down": true,
	"out": true, "off": true, "all": true, "any": true, "some": true, "no": true,
	"not": true, "only": true, "just": true, "also": true, "very": true, "more": true,
	"most": true, "such": true, "which": true, "who": true, "whom": true, "whose": true,
	"what": true, "when": true, "where": true, "how": true, "why": true,
	"total": true, "approximately": true, "around": true, "about-": true,
	"new": true, "old": true, "current": true, "previous": true,
}

// badSubjects are words that make a useless comparison key.
//
// A subject is what decides which claims get compared for agreement or
// conflict, so a verb or a company suffix here produces spurious pairs: two
// documents both containing "will ship 240 units" and "will ship 500 units"
// would otherwise be compared under the subject "ship" rather than "units".
var badSubjects = map[string]bool{
	// Legal forms — part of a company's name, never what a figure is about.
	"ltd": true, "limited": true, "plc": true, "llp": true, "llc": true,
	"inc": true, "incorporated": true, "corp": true, "corporation": true,
	"gmbh": true, "pty": true, "holdings": true,
	// Common verbs that sit immediately before a quantity.
	"ship": true, "ships": true, "shipped": true, "send": true, "sent": true,
	"deliver": true, "delivers": true, "delivered": true, "cover": true,
	"covers": true, "covered": true, "issue": true, "issued": true,
	"report": true, "reported": true, "state": true, "stated": true,
	"say": true, "says": true, "said": true, "give": true, "given": true,
	"take": true, "taken": true, "make": true, "made": true, "pay": true,
	"paid": true, "run": true, "runs": true, "use": true, "used": true,
	"include": true, "includes": true, "included": true,
	"attracts": true, "agreed": true, "prepared": true, "conducted": true,
	// Honorifics.
	"mr": true, "mrs": true, "dr": true, "prof": true, "professor": true,
}

// overlapsSpan reports whether [start,end) intersects any offset in spans.
func overlapsSpan(spans map[int]bool, start, end int) bool {
	for i := start; i < end; i++ {
		if spans[i] {
			return true
		}
	}
	return false
}

type sentenceSpan struct{ start, end int }

// splitSentences divides text at sentence terminators, leaving offsets intact.
//
// Common abbreviations are not treated as terminators, so "Ltd. reported" stays
// one sentence rather than becoming two fragments whose claim is lost.
func splitSentences(text string) []sentenceSpan {
	var out []sentenceSpan
	start := 0

	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '.' && c != '!' && c != '?' && c != '\n' {
			continue
		}
		if c == '.' && isAbbreviation(text, i) {
			continue
		}
		// A terminator must be followed by whitespace or the end of the text.
		if i+1 < len(text) && !isSpaceByte(text[i+1]) {
			continue
		}
		end := i + 1
		if strings.TrimSpace(text[start:end]) != "" {
			out = append(out, sentenceSpan{start, end})
		}
		// Skip the whitespace that follows.
		j := end
		for j < len(text) && isSpaceByte(text[j]) {
			j++
		}
		start = j
		i = j - 1
	}
	if start < len(text) && strings.TrimSpace(text[start:]) != "" {
		out = append(out, sentenceSpan{start, len(text)})
	}
	return out
}

// abbreviations that end in a full stop without ending a sentence.
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "st": true,
	"ltd": true, "inc": true, "co": true, "corp": true, "plc": true,
	"e.g": true, "i.e": true, "etc": true, "vs": true, "no": true,
	"jan": true, "feb": true, "mar": true, "apr": true, "jun": true,
	"jul": true, "aug": true, "sep": true, "sept": true, "oct": true,
	"nov": true, "dec": true, "approx": true, "dept": true, "est": true,
}

func isAbbreviation(text string, dot int) bool {
	// Walk back over the word preceding the dot.
	start := dot
	for start > 0 && !isSpaceByte(text[start-1]) {
		start--
	}
	word := strings.ToLower(strings.Trim(text[start:dot], ".,;:!?()"))
	if word == "" {
		return false
	}
	if abbreviations[word] {
		return true
	}
	// A single letter followed by a dot is an initial ("J. Smith").
	return len([]rune(word)) == 1
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
