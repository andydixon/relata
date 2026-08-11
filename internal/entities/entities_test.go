package entities

import (
	"strings"
	"testing"

	"github.com/andydixon/relata/internal/domain"
)

func TestNormaliseFoldsCasePunctuationAndAccents(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Example Ltd", "example ltd"},
		{"  EXAMPLE   LTD.  ", "example ltd"},
		{"Example, Ltd.", "example ltd"},
		{"Zoë Müller", "zoe muller"},
		{"Marks & Spencer", "marks and spencer"},
		{"O'Brien", "obrien"},
		{"ACME—Corp", "acme corp"},
	}
	for _, c := range cases {
		if got := Normalise(c.in); got != c.want {
			t.Errorf("Normalise(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormaliseIsIdempotent(t *testing.T) {
	for _, in := range []string{"Example Ltd.", "Zoë  Müller", "A & B"} {
		once := Normalise(in)
		if twice := Normalise(once); once != twice {
			t.Errorf("Normalise is not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

// The key must distinguish types: a place and a person sharing a name are not
// the same entity.
func TestKeyIncludesType(t *testing.T) {
	person := Key(domain.EntityPerson, "Washington")
	place := Key(domain.EntityLocation, "Washington")
	if person == place {
		t.Fatal("entities of different types collapsed to the same key")
	}
}

// This is the central safety property: normalisation must not silently merge
// distinct organisations.
func TestNormalisationDoesNotMergeDistinctCompanies(t *testing.T) {
	distinct := [][2]string{
		{"Example Ltd", "Example Holdings Ltd"},
		{"Example Ltd", "Example Inc"},
		{"Northern Rail", "Northern Rail Freight"},
		{"Acme Systems", "Acme Systems Europe"},
	}
	for _, pair := range distinct {
		a := Key(domain.EntityOrganisation, pair[0])
		b := Key(domain.EntityOrganisation, pair[1])
		if a == b {
			t.Errorf("%q and %q were merged into one entity key %q", pair[0], pair[1], a)
		}
	}
}

func TestLooseKeyIsOnlyForAliasProposals(t *testing.T) {
	// The loose key deliberately collapses legal forms, which is why it must
	// never be used for storage identity.
	if LooseKey(domain.EntityOrganisation, "Example Ltd") != LooseKey(domain.EntityOrganisation, "Example Limited") {
		t.Fatal("loose key should collapse equivalent legal forms")
	}
	if Key(domain.EntityOrganisation, "Example Ltd") == Key(domain.EntityOrganisation, "Example Limited") {
		t.Fatal("storage key must NOT collapse them without human confirmation")
	}
}

func TestAliasSimilarityRanksSensibly(t *testing.T) {
	identical := AliasSimilarity("Example Ltd", "example ltd")
	if identical != 1 {
		t.Fatalf("case-only difference scored %v, want 1", identical)
	}
	typo := AliasSimilarity("Example Ltd", "Exampel Ltd")
	unrelated := AliasSimilarity("Example Ltd", "Northern Rail Freight")
	if typo <= unrelated {
		t.Fatalf("typo (%v) did not score above an unrelated name (%v)", typo, unrelated)
	}
	if unrelated > 0.5 {
		t.Fatalf("unrelated names scored implausibly high: %v", unrelated)
	}
}

func TestAliasSimilarityRecognisesAcronyms(t *testing.T) {
	got := AliasSimilarity("BBC", "British Broadcasting Corporation")
	if got < 0.8 {
		t.Fatalf("acronym match scored only %v", got)
	}
}

func TestCanonicaliseIsDeterministic(t *testing.T) {
	observed := []string{"example ltd", "Example Ltd", "Example Ltd", "EXAMPLE LTD"}
	first := Canonicalise(observed)
	if first != "Example Ltd" {
		t.Fatalf("canonical form = %q, want the most frequent properly-cased spelling", first)
	}
	// Re-running with a different input order must give the same answer.
	shuffled := []string{"EXAMPLE LTD", "Example Ltd", "example ltd", "Example Ltd"}
	if second := Canonicalise(shuffled); second != first {
		t.Fatalf("canonicalisation depends on input order: %q vs %q", first, second)
	}
}

// --- Deterministic extraction ---------------------------------------------

func findMention(ms []Mention, name string) *Mention {
	for i := range ms {
		if ms[i].Name == name {
			return &ms[i]
		}
	}
	return nil
}

func TestExtractsOrganisationsWithLegalSuffixes(t *testing.T) {
	text := "The contract was signed by Example Ltd and Northern Rail Holdings PLC on Tuesday."
	ms := ExtractDeterministic(text)

	for _, want := range []string{"Example Ltd", "Northern Rail Holdings PLC"} {
		m := findMention(ms, want)
		if m == nil {
			t.Fatalf("did not extract %q from %q", want, text)
		}
		if m.Type != domain.EntityOrganisation {
			t.Errorf("%q typed as %s, want organisation", want, m.Type)
		}
		if text[m.Start:m.End] != want {
			t.Errorf("%q offsets point at %q", want, text[m.Start:m.End])
		}
	}
}

func TestExtractsPeopleWithHonorifics(t *testing.T) {
	text := "Dr Alice Chen approved it, and Prof Raymond Okafor countersigned."
	ms := ExtractDeterministic(text)

	for _, want := range []string{"Alice Chen", "Raymond Okafor"} {
		m := findMention(ms, want)
		if m == nil {
			t.Fatalf("did not extract %q", want)
		}
		if m.Type != domain.EntityPerson {
			t.Errorf("%q typed as %s, want person", want, m.Type)
		}
		if text[m.Start:m.End] != want {
			t.Errorf("%q offsets point at %q", want, text[m.Start:m.End])
		}
	}
}

func TestExtractsEmailAddresses(t *testing.T) {
	text := "Contact Ops.Team@example.co.uk for details."
	ms := ExtractDeterministic(text)
	if m := findMention(ms, "ops.team@example.co.uk"); m == nil {
		t.Fatalf("email not extracted from %q; got %+v", text, ms)
	}
}

func TestNormalisesDatesToISO(t *testing.T) {
	cases := map[string]string{
		"Due on 2025-03-14 sharp.":      "2025-03-14",
		"Due on 14 March 2025 sharp.":   "2025-03-14",
		"Due on 14th March 2025 sharp.": "2025-03-14",
		"Due on March 14, 2025 sharp.":  "2025-03-14",
		"Due on 14/03/2025 sharp.":      "2025-03-14",
	}
	for text, want := range cases {
		ms := ExtractDeterministic(text)
		m := findMention(ms, want)
		if m == nil {
			t.Errorf("did not normalise a date in %q to %s (got %+v)", text, want, ms)
			continue
		}
		if m.Type != domain.EntityDate {
			t.Errorf("%q typed as %s, want date", text, m.Type)
		}
		if m.Normalised != want {
			t.Errorf("normalised value %q, want %q", m.Normalised, want)
		}
	}
}

func TestDoesNotExtractCommonWordsAsEntities(t *testing.T) {
	text := "The Ltd. This Inc. And Corp."
	ms := ExtractDeterministic(text)
	for _, m := range ms {
		if m.Name == "The Ltd." || m.Name == "This Inc." || m.Name == "And Corp." {
			t.Errorf("sentence-opening word extracted as an entity: %q", m.Name)
		}
	}
}

func TestMentionOffsetsAlwaysAddressTheirQuote(t *testing.T) {
	text := "Dr Alice Chen of Example Ltd wrote to ops@example.com on 2025-03-14 about Northern Rail Holdings PLC."
	for _, m := range ExtractDeterministic(text) {
		if m.Start < 0 || m.End > len(text) || m.Start >= m.End {
			t.Fatalf("mention %q has invalid offsets [%d,%d)", m.Name, m.Start, m.End)
		}
		if m.Quote != text[m.Start:m.End] {
			t.Fatalf("mention %q quote %q does not match its offsets %q", m.Name, m.Quote, text[m.Start:m.End])
		}
	}
}

func TestExtractionIsBounded(t *testing.T) {
	var text string
	for i := 0; i < 5000; i++ {
		text += "Example Ltd and Dr Alice Chen on 2025-03-14. "
	}
	ms := ExtractDeterministic(text)
	if len(ms) > maxDeterministicMentions+600 {
		t.Fatalf("extraction is not bounded: %d mentions", len(ms))
	}
}

func TestExtractionOnEmptyAndOddInput(t *testing.T) {
	for _, in := range []string{"", "   ", "\x00\x01", "aaaa", "Ltd", "@@@"} {
		if ms := ExtractDeterministic(in); len(ms) > 5 {
			t.Errorf("input %q produced %d mentions", in, len(ms))
		}
	}
}

func FuzzExtractDeterministic(f *testing.F) {
	f.Add("Example Ltd on 2025-03-14")
	f.Add("Dr A B")
	f.Add("a@b.co")
	f.Add("14/03/2025")

	f.Fuzz(func(t *testing.T, text string) {
		for _, m := range ExtractDeterministic(text) {
			if m.Start < 0 || m.End > len(text) || m.Start > m.End {
				t.Fatalf("mention offsets [%d,%d) out of range for %d bytes", m.Start, m.End, len(text))
			}
			if m.Quote != text[m.Start:m.End] {
				t.Fatal("mention quote does not match its offsets")
			}
		}
	})
}

func FuzzNormalise(f *testing.F) {
	f.Add("Example Ltd")
	f.Add("Zoë")
	f.Add("\x00\xff")

	f.Fuzz(func(t *testing.T, s string) {
		once := Normalise(s)
		if twice := Normalise(once); once != twice {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, once, twice)
		}
	})
}

// --- Deterministic claim extraction ---------------------------------------

// A date must not also be read as a bare number. The number pattern can begin
// at the whitespace before a date and take "202" out of "2025-02-10"; that
// leaked figure was then compared against other documents' quantities and
// produced spurious contradictions.
func TestDatesAreNotAlsoExtractedAsNumbers(t *testing.T) {
	cases := []string{
		"Prepared by Dr Alice Chen for Example Ltd on 2025-02-10.",
		"The report was filed on 14 March 2025.",
		"Signed 03/04/2025 by the assessor.",
	}
	for _, text := range cases {
		_, _, nums, dates, _ := AnalyseStatement(text)
		if len(dates) == 0 {
			t.Errorf("%q: no date detected", text)
		}
		for _, n := range nums {
			// Any fragment of a year would land in this range.
			if n >= 19 && n <= 2100 {
				t.Errorf("%q: a date fragment leaked into numbers as %v (numbers=%v)", text, n, nums)
			}
		}
	}
}

func TestGenuineFiguresAreStillExtracted(t *testing.T) {
	subject, _, nums, _, _ := AnalyseStatement("The total project cost is £42,000 excluding VAT.")
	if len(nums) != 1 || nums[0] != 42000 {
		t.Fatalf("figure not extracted: %v", nums)
	}
	if subject != "cost" {
		t.Fatalf("subject = %q, want %q", subject, "cost")
	}
}

// A subject decides which claims get compared, so a verb or a company suffix
// there manufactures spurious pairs.
func TestSubjectsAreNotVerbsOrLegalSuffixes(t *testing.T) {
	// A quantity's subject is the unit noun that follows it; a value's subject
	// is the noun that precedes it.
	cases := map[string]string{
		"Example Ltd will ship 240 units to the Northfield depot.": "units",
		"The returns window is 60 days from delivery.":             "window",
		"Refunds are issued within 14 days.":                       "days",
		"The total project cost is £42,000 excluding VAT.":         "cost",
	}
	for text, want := range cases {
		got, _, _, _, _ := AnalyseStatement(text)
		if got != want {
			t.Errorf("AnalyseStatement(%q) subject = %q, want %q", text, got, want)
		}
		if badSubjects[got] {
			t.Errorf("%q produced a blocked subject %q", text, got)
		}
	}
}

func TestClaimExtractionRequiresAFigureOrDate(t *testing.T) {
	// Pure prose is left to the model stage rather than recorded on weak grounds.
	prose := "The team discussed the matter at length and agreed to reflect further."
	if got := ExtractDeterministicClaims(prose); len(got) != 0 {
		t.Fatalf("prose without a figure or date produced %d claims: %+v", len(got), got)
	}

	withFigure := "The total project cost is £42,000 excluding VAT."
	got := ExtractDeterministicClaims(withFigure)
	if len(got) != 1 {
		t.Fatalf("expected 1 claim, got %d", len(got))
	}
	if got[0].Quote != withFigure {
		t.Fatalf("claim quote %q does not match the source sentence", got[0].Quote)
	}
	if got[0].Subject != "cost" {
		t.Fatalf("subject = %q, want cost", got[0].Subject)
	}
}

func TestClaimOffsetsAddressTheirQuote(t *testing.T) {
	text := "Intro sentence here. The total project cost is £42,000. The date is 14 March 2025."
	for _, c := range ExtractDeterministicClaims(text) {
		if text[c.Start:c.End] != c.Quote {
			t.Fatalf("claim offsets [%d,%d) give %q, but the quote is %q",
				c.Start, c.End, text[c.Start:c.End], c.Quote)
		}
	}
}

func TestSentenceSplittingKnowsAbbreviations(t *testing.T) {
	// "Ltd." must not end the sentence, or the claim would be lost.
	text := "Example Ltd. reported a total cost of £42,000 for the work."
	claims := ExtractDeterministicClaims(text)
	if len(claims) != 1 {
		t.Fatalf("expected 1 claim, got %d: %+v", len(claims), claims)
	}
	if !strings.Contains(claims[0].Quote, "42,000") {
		t.Fatalf("the figure was split away from its sentence: %q", claims[0].Quote)
	}
}

func TestNegationIsDetected(t *testing.T) {
	_, _, _, _, pol := AnalyseStatement("The order was not delivered on 14 March 2025.")
	if pol != -1 {
		t.Fatalf("polarity = %d, want -1 for a negated statement", pol)
	}
	_, _, _, _, pol = AnalyseStatement("The order was delivered on 14 March 2025.")
	if pol != 1 {
		t.Fatalf("polarity = %d, want 1", pol)
	}
}

func FuzzExtractDeterministicClaims(f *testing.F) {
	f.Add("The cost is £42,000.")
	f.Add("Due 2025-03-14.")
	f.Add("Ltd. Inc. Dr. 1.2.3")

	f.Fuzz(func(t *testing.T, text string) {
		for _, c := range ExtractDeterministicClaims(text) {
			if c.Start < 0 || c.End > len(text) || c.Start > c.End {
				t.Fatalf("claim offsets [%d,%d) out of range for %d bytes", c.Start, c.End, len(text))
			}
			if text[c.Start:c.End] != c.Quote {
				t.Fatal("claim quote does not match its offsets")
			}
		}
	})
}

// A legal suffix adds a word that breaks the acronym letter count, so the
// comparison must also consider the suffix-stripped forms. Without this, the
// BBC example the documentation gives would never be proposed.
func TestAcronymMatchingIgnoresLegalSuffixes(t *testing.T) {
	cases := [][2]string{
		{"BBC", "British Broadcasting Corporation Ltd"},
		{"BBC", "British Broadcasting Corporation"},
		{"NRF", "Northern Rail Freight PLC"},
	}
	for _, c := range cases {
		if got := AliasSimilarity(c[0], c[1]); got < aliasProposalThreshold {
			t.Errorf("AliasSimilarity(%q, %q) = %.2f, below the %.2f proposal threshold",
				c[0], c[1], got, aliasProposalThreshold)
		}
	}
}

// Names differing only by legal form are worth a human's attention, but the
// storage keys must still differ so nothing is merged automatically.
func TestLegalFormVariantsAreProposedButNotMerged(t *testing.T) {
	if got := AliasSimilarity("Example Ltd", "Example Limited"); got < aliasProposalThreshold {
		t.Errorf("Example Ltd / Example Limited scored %.2f, too low to be proposed", got)
	}
	if Key(domain.EntityOrganisation, "Example Ltd") == Key(domain.EntityOrganisation, "Example Limited") {
		t.Fatal("the two names share a storage key, so they were merged without a human decision")
	}
}

// The looser matching must not start proposing genuinely different companies.
func TestLooseMatchingStillSeparatesDistinctCompanies(t *testing.T) {
	distinct := [][2]string{
		{"Example Ltd", "Example Holdings Ltd"},
		{"Northern Rail", "Southern Rail"},
		{"Acme Systems", "Acme Logistics"},
		{"Example Ltd", "Northern Rail Freight"},
	}
	for _, c := range distinct {
		if got := AliasSimilarity(c[0], c[1]); got >= aliasProposalThreshold {
			t.Errorf("AliasSimilarity(%q, %q) = %.2f, high enough to propose merging different companies",
				c[0], c[1], got)
		}
	}
}
