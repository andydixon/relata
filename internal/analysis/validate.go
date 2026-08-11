package analysis

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Rejection reasons, used as metric labels and audit detail. They are a closed
// set so that cardinality stays bounded.
const (
	ReasonNotJSON      = "not_json"
	ReasonUnknownField = "unknown_field"
	ReasonTrailingData = "trailing_data"
	ReasonTooLarge     = "too_large"
	ReasonBadEnum      = "bad_enum"
	ReasonUnknownRef   = "unknown_reference"
	ReasonMissingField = "missing_field"
	ReasonOutOfRange   = "out_of_range"
	ReasonTooManyItems = "too_many_items"
	ReasonEmpty        = "empty_response"
	ReasonNotGrounded  = "not_grounded"
)

// InvalidOutputError describes model output that failed validation.
//
// It is deliberately specific: a permanently invalid response must not be
// retried, and knowing why lets the caller decide that without guessing.
type InvalidOutputError struct {
	Task   string
	Reason string
	Detail string
	// Sample is a short, truncated excerpt of the offending output, kept for
	// diagnostics. It is bounded because model output is untrusted.
	Sample string
}

func (e *InvalidOutputError) Error() string {
	return fmt.Sprintf("model output rejected for task %s (%s): %s", e.Task, e.Reason, e.Detail)
}

// Is lets callers match with errors.Is(err, domain.ErrModelInvalid) via Unwrap.
func (e *InvalidOutputError) Unwrap() error { return errInvalidOutput }

var errInvalidOutput = errors.New("invalid model output")

// ErrInvalidOutput is the sentinel all validation failures unwrap to.
var ErrInvalidOutput = errInvalidOutput

// Reject builds an InvalidOutputError with a bounded sample.
func Reject(task, reason, detail, raw string) *InvalidOutputError {
	return &InvalidOutputError{
		Task:   task,
		Reason: reason,
		Detail: detail,
		Sample: sample(raw, 300),
	}
}

// maxModelOutputBytes bounds any single structured response before decoding.
const maxModelOutputBytes = 256 << 10

// DecodeStrict parses raw model output into dst.
//
// It is strict on purpose:
//   - unknown fields are an error, so a model that invents structure is caught
//     rather than silently ignored;
//   - trailing content after the JSON value is an error, so a model that
//     appends commentary or a second object cannot smuggle it past;
//   - the payload is size-bounded before parsing.
//
// No regular expressions are used to find or repair JSON. If the response is
// not a single well-formed JSON value of the expected shape, it is rejected.
func DecodeStrict(task string, raw string, dst any) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Reject(task, ReasonEmpty, "the model returned an empty response", raw)
	}
	if len(trimmed) > maxModelOutputBytes {
		return Reject(task, ReasonTooLarge,
			fmt.Sprintf("response of %d bytes exceeds the %d byte limit", len(trimmed), maxModelOutputBytes), raw)
	}
	if !utf8.ValidString(trimmed) {
		return Reject(task, ReasonNotJSON, "response is not valid UTF-8", raw)
	}

	// Some models wrap JSON in a markdown fence despite being asked not to.
	// Unwrapping a complete, well-formed fence is a structural operation on a
	// delimiter, not a regex-based repair of malformed JSON: if what remains is
	// not valid JSON it is still rejected below.
	trimmed = unwrapFence(trimmed)

	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	dec.UseNumber()

	if err := dec.Decode(dst); err != nil {
		reason := ReasonNotJSON
		if strings.Contains(err.Error(), "unknown field") {
			reason = ReasonUnknownField
		}
		return Reject(task, reason, err.Error(), raw)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Reject(task, ReasonTrailingData,
			"the response contains additional content after the JSON value", raw)
	}
	return nil
}

// unwrapFence removes a single complete ```/```json fence around the payload.
func unwrapFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line.
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s
	}
	rest := s[nl+1:]
	end := strings.LastIndex(rest, "```")
	if end < 0 {
		return s
	}
	return strings.TrimSpace(rest[:end])
}

// sample truncates untrusted text to n bytes on a rune boundary, for logging.
func sample(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Reference and value checking
// ---------------------------------------------------------------------------

// RefSet is the set of identifiers a model was actually shown. Any ID in the
// response that is not in this set is a hallucination and invalidates the
// record that references it.
type RefSet map[string]struct{}

// NewRefSet builds a reference set from the IDs supplied to the model.
func NewRefSet(ids ...string) RefSet {
	s := make(RefSet, len(ids))
	for _, id := range ids {
		if id != "" {
			s[id] = struct{}{}
		}
	}
	return s
}

// Add registers an additional valid identifier.
func (s RefSet) Add(id string) {
	if id != "" {
		s[id] = struct{}{}
	}
}

// Has reports whether id was shown to the model.
func (s RefSet) Has(id string) bool {
	_, ok := s[id]
	return ok
}

// CheckEnum validates that value is one of allowed, returning a rejection
// otherwise. Enum checking is what stops a model inventing a relationship type.
func CheckEnum[T ~string](task, field string, value T, allowed []T, raw string) error {
	for _, a := range allowed {
		if a == value {
			return nil
		}
	}
	names := make([]string, len(allowed))
	for i, a := range allowed {
		names[i] = string(a)
	}
	return Reject(task, ReasonBadEnum,
		fmt.Sprintf("%s=%q is not one of: %s", field, string(value), strings.Join(names, ", ")), raw)
}

// CheckRange validates a numeric field.
func CheckRange(task, field string, v, lo, hi float64, raw string) error {
	if v < lo || v > hi {
		return Reject(task, ReasonOutOfRange,
			fmt.Sprintf("%s=%v is outside the range %v..%v", field, v, lo, hi), raw)
	}
	return nil
}

// CheckCount enforces an upper bound on array length before the items are used.
func CheckCount(task, field string, n, maxN int, raw string) error {
	if n > maxN {
		return Reject(task, ReasonTooManyItems,
			fmt.Sprintf("%s contains %d entries, more than the %d permitted", field, n, maxN), raw)
	}
	return nil
}

// CleanText normalises a model-supplied string for storage: control characters
// removed, whitespace collapsed, length bounded.
//
// Model output is rendered as escaped HTML everywhere in Relata, so this is not
// the XSS defence; it exists so that stored text stays displayable and bounded.
func CleanText(s string, maxLen int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if maxLen > 0 && len(s) > maxLen {
		s = sample(s, maxLen)
	}
	return s
}

// CompactJSON re-encodes v without whitespace, used to embed schemas in
// requests.
func CompactJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Degenerate output
// ---------------------------------------------------------------------------

// ReasonDegenerate marks output that is structurally valid but carries no
// content — the model filled the schema without answering.
const ReasonDegenerate = "degenerate_content"

// schemaEchoes are tokens a struggling model emits in place of real content,
// usually by echoing a field name or a boolean from the schema it was given.
var schemaEchoes = map[string]bool{
	"answer": true, "sufficient": true, "insufficient": true, "statement": true,
	"citations": true, "conflicts": true, "rationale": true, "name": true,
	"type": true, "quote": true, "subject": true, "value": true, "text": true,
	"true": true, "false": true, "null": true, "nil": true, "none": true,
	"n/a": true, "na": true, "unknown": true, "undefined": true, "string": true,
	"todo": true, "example": true, "...": true, "-": true,
}

// Degenerate reports whether s is too empty to be usable content.
//
// A schema constrains a response's *shape*, not its substance: a model can
// satisfy every structural rule and still return the word "sufficient" where an
// answer belongs. That is invalid output, and treating it as an answer would
// present the model's failure to the user as knowledge.
func Degenerate(s string, minLength int) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.Trim(t, ".,;:!?\"'`*_ ")

	if t == "" {
		return true
	}
	if schemaEchoes[t] {
		return true
	}
	// The length floor applies only to output carrying no digits.
	//
	// "3" is a complete answer to "how many depots?", while "x" is a
	// placeholder — the difference is that a number is substance and a lone
	// letter is not. Without this, the floor that catches "x" also throws away
	// correct numeric answers.
	if !strings.ContainsFunc(t, unicode.IsDigit) && utf8.RuneCountInString(t) < minLength {
		return true
	}

	// Nothing but punctuation — "...", "-", "??" and the like.
	//
	// Digits count as content. An earlier version of this required a letter,
	// which rejected "£42,000" as an answer to "What is the total project
	// cost?" — a correct answer, and the best possible one. Terse factual
	// answers are exactly what a factual question should produce, so the only
	// thing worth rejecting here is output with no substance at all.
	for _, r := range t {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// RejectDegenerate builds the rejection for empty-but-well-formed output.
func RejectDegenerate(task, field, got string) *InvalidOutputError {
	return Reject(task, ReasonDegenerate,
		fmt.Sprintf("%s contains no usable content — the model satisfied the schema without answering", field),
		got)
}
