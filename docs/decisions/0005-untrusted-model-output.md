# 5. Model output is untrusted input

**Status:** accepted

## Context

Relata asks a language model for structured data: entities, claims, relationship
classifications, answers. The convenient assumption is that a model asked for JSON returns
usable JSON.

## Decision

Model output is validated exactly as strictly as an HTTP request body from an anonymous
client: strict decoding with unknown fields rejected, trailing content refused, size bounded,
enum values checked, referenced IDs verified against what was actually supplied, and quoted
text confirmed to exist in the source. Invalid output is recorded and discarded — never
repaired, never blindly retried.

## Rationale

Three distinct failure modes, and the third is the interesting one.

**Ordinary unreliability.** Models emit trailing prose, wrap JSON in fences, invent fields and
occasionally return something that is not JSON at all. Any of these silently corrupts stored
data if accepted.

**Hallucinated references.** A model asked to cite sources will sometimes cite one that does
not exist. If citations were trusted, an answer could point at an item that has nothing to do
with it — and look authoritative doing so. So the model is shown opaque handles (`S1`, `S2`)
rather than item IDs: a fabricated citation cannot resolve to a real record, by construction.
The same reasoning applies to claim quotations: a claim whose quote cannot be located in the
source is discarded, which is what actually guarantees "a claim never loses its link to the
original source text".

**Compromise.** If the provider were malicious — or if a prompt injection succeeded — the
response is attacker-controlled. Strict schema validation means the worst it can do is
populate typed fields. It cannot invent a relationship type outside the enum, reference an
item it was not shown, or reach anything else, because the model has no tools, no database
access and no credentials.

Deliberately **no regular expressions** are used to find or repair JSON. Regex repair is how
malformed output becomes accepted-but-wrong data. A complete markdown fence is unwrapped —
a structural operation on a delimiter — and if what remains is not valid JSON it is still
rejected.

Invalid output is **not retried**, because a model that cannot follow a schema will not start
on the third attempt; it would only burn the queue. It is recorded on
`relata_model_invalid_output_total{task,reason}` so a systematically failing model is visible.

## Consequences

**Good:** a model cannot corrupt stored data by being unreliable. A fabricated citation cannot
resolve. Rejection rates are measurable, which turns "the model is bad" into a number.

**Bad:** a strict schema rejects output that a lenient parser would have salvaged, so smaller
models yield less. That is the intended trade: less data rather than wrong data.

**Consequence worth stating:** because rejection is per-row where possible — one invalid entity
does not discard the whole response — a partly-usable response is partly used.
