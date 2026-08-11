// Package entities extracts and resolves identifiable subjects from source
// material.
//
// Resolution is deliberately conservative. Two entities are treated as the same
// only when their normalised keys match exactly. Names that merely look alike
// produce a *candidate alias* awaiting human confirmation — they are never
// merged automatically, because "Example Ltd" and "Example Holdings Ltd" are
// different companies far more often than they are typos of each other.
package entities

import (
	"sort"
	"strings"
	"unicode"

	"github.com/andydixon/relata/internal/domain"
)

// Normalise produces the exact-match key for an entity name.
//
// The transformation is intentionally shallow: case folding, whitespace
// collapsing, punctuation and diacritic removal. It does not strip company
// suffixes or expand abbreviations, because doing so would silently merge
// distinct entities — exactly what the design forbids.
func Normalise(name string) string {
	var b strings.Builder
	b.Grow(len(name))

	prevSpace := true // leading space suppressed
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(foldDiacritic(r))
			prevSpace = false
		case r == '\'' || r == '’' || r == 'ʼ':
			// Apostrophes are dropped rather than turned into a break, so
			// "O'Brien" and "OBrien" share a key while "O Brien" (two words)
			// is a different thing.

		case r == '&':
			// "&" carries meaning in company names ("Marks & Spencer").
			if !prevSpace {
				b.WriteByte(' ')
			}
			b.WriteString("and")
			b.WriteByte(' ')
			prevSpace = true
		case unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// Key builds the storage key that determines entity identity. Type is part of
// the key: a place called "Washington" is not the person "Washington".
func Key(t domain.EntityType, name string) string {
	return string(t) + ":" + Normalise(name)
}

// diacriticFolds maps accented Latin letters onto their base form so that
// "Zoë" and "Zoe" share a key. It covers Latin-1 and Latin Extended-A rather
// than every script, which is honest about its reach: names in scripts outside
// this table are normalised by case and punctuation only.
var diacriticFolds = func() map[rune]rune {
	const from = "àáâãäåāăąèéêëēĕėęěìíîïĩīĭįòóôõöøōŏőùúûüũūŭůűçćĉċčñńņňśŝşšýÿŷžźżðþßæœ"
	const to = "aaaaaaaaaeeeeeeeeeiiiiiiiiooooooooouuuuuuuuucccccnnnnsssssyyyzzzdtsao"

	f := []rune(from)
	t := []rune(to)
	m := make(map[rune]rune, len(f))
	for i, r := range f {
		if i < len(t) {
			m[r] = t[i]
		}
	}
	return m
}()

// foldDiacritic returns the base form of an accented letter, or r unchanged.
func foldDiacritic(r rune) rune {
	if r < 128 {
		return r
	}
	if base, ok := diacriticFolds[r]; ok {
		return base
	}
	return r
}

// LooseKey is a more aggressive normalisation used ONLY to propose alias
// candidates for human review. It is never used for storage identity.
//
// It strips legal-form suffixes and honorifics, which is precisely why it is
// unsafe as an identity key: "Example Ltd" and "Example Inc" collapse together
// under it, and those may well be different companies.
func LooseKey(t domain.EntityType, name string) string {
	n := Normalise(name)
	stripped := stripNoiseWords(n)
	if stripped == "" {
		stripped = n
	}
	return string(t) + ":" + stripped
}

var legalSuffixes = map[string]bool{
	"ltd": true, "limited": true, "plc": true, "llp": true, "llc": true,
	"inc": true, "incorporated": true, "corp": true, "corporation": true,
	"gmbh": true, "ag": true, "sa": true, "bv": true, "nv": true, "pty": true,
	"co": true, "company": true, "group": true, "holdings": false,
}

var honorifics = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "miss": true, "dr": true,
	"prof": true, "professor": true, "sir": true, "dame": true, "lord": true,
}

// AliasSimilarity scores how alike two names are, in 0..1.
//
// It is used only to decide whether an alias is worth proposing to a human.
// A high score never causes a merge on its own.
func AliasSimilarity(a, b string) float64 {
	na, nb := Normalise(a), Normalise(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}

	// Token overlap (Jaccard) catches reordering and extra words.
	ta, tb := tokenSet(na), tokenSet(nb)
	inter := 0
	for w := range ta {
		if tb[w] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	jaccard := 0.0
	if union > 0 {
		jaccard = float64(inter) / float64(union)
	}

	// Character-level similarity catches typos and abbreviations.
	edit := 1 - float64(levenshtein(na, nb))/float64(maxInt(len(na), len(nb)))
	if edit < 0 {
		edit = 0
	}

	// An acronym match is strong evidence worth a human's attention.
	//
	// The comparison also uses the suffix-stripped forms, because a legal form
	// adds a word that breaks the letter count: "BBC" is an acronym of
	// "British Broadcasting Corporation" but not of "British Broadcasting
	// Corporation Ltd".
	la, lb := stripNoiseWords(na), stripNoiseWords(nb)
	if isAcronymOf(na, nb) || isAcronymOf(nb, na) ||
		isAcronymOf(la, lb) || isAcronymOf(lb, la) {
		return maxFloat(0.85, (jaccard+edit)/2)
	}

	// Names differing only by a legal form are worth proposing, but they are
	// NOT merged: "Example Ltd" and "Example Inc" may be different companies.
	if la != "" && la == lb {
		return maxFloat(0.85, jaccard)
	}
	return 0.6*jaccard + 0.4*edit
}

// stripNoiseWords removes TRAILING legal forms and LEADING honorifics from a
// normalised name.
//
// Only the edges are trimmed. "Corporation" is a legal form at the end of
// "Example Corporation" but part of the name in "British Broadcasting
// Corporation Ltd", and stripping every occurrence would lose the word that
// makes the acronym work.
func stripNoiseWords(normalised string) string {
	words := strings.Fields(normalised)

	// At most one of each. Stripping repeatedly would take "Corporation" off
	// "British Broadcasting Corporation Ltd" once "Ltd" had gone, losing the
	// word the acronym depends on.
	if len(words) > 1 && honorifics[words[0]] {
		words = words[1:]
	}
	if len(words) > 1 && legalSuffixes[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
}

func tokenSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// isAcronymOf reports whether short is the initialism of long.
func isAcronymOf(short, long string) bool {
	short = strings.ReplaceAll(short, " ", "")
	words := strings.Fields(long)
	if len(words) < 2 || len(short) != len(words) {
		return false
	}
	for i, w := range words {
		if len(w) == 0 || w[0] != short[i] {
			return false
		}
	}
	return true
}

// levenshtein computes edit distance with a two-row buffer.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = minInt(minInt(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// Canonicalise picks the display name for an entity seen under several spellings.
//
// The most frequent spelling wins; ties are broken by preferring the form with
// more capital letters (usually the properly cased one) and then alphabetically,
// so the choice is deterministic across runs.
func Canonicalise(observed []string) string {
	if len(observed) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, o := range observed {
		if t := strings.TrimSpace(o); t != "" {
			counts[t]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		ci, cj := capitals(names[i]), capitals(names[j])
		if ci != cj {
			return ci > cj
		}
		return names[i] < names[j]
	})
	return names[0]
}

func capitals(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsUpper(r) {
			n++
		}
	}
	return n
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
