# 7. Unusable model output degrades the answer; it does not fail the request

**Status:** accepted

## Context

Model output is validated strictly: strict JSON decoding, enum checks, citations resolved
against the references actually supplied, quotations grounded in the source. Anything that
fails is rejected rather than repaired.

Adding degeneracy validation — rejecting free text that is a schema field name, a placeholder,
or too short to carry meaning — made a pre-existing weakness visible. `Answer` returned
`nil, err` for *any* generation failure. A model that satisfied the schema without answering
therefore produced no sources, no citations and no conflicts: the retrieval was discarded
along with the response.

`llama3.2:3b` returns `{"answer": "sufficient", "sufficient": true, "citations": []}` often
enough that this was the normal experience on a small model, not an edge case.

## Decision

Output rejected as invalid produces an answer with no prose, `sufficient = false`, the
retrieved sources intact, the recorded contradictions between them surfaced, and the rejection
reason stated in the uncertainty section.

Transport failures — a timeout, an unreachable provider — still return an error.

## Rationale

Retrieval and contradiction detection are deterministic and do not involve the model at all.
Discarding them because the model misbehaved throws away the part of the system that worked,
in precisely the situation where the user most needs to read the sources themselves.

It also matches what the surrounding code already does. "Nothing was retrieved" and "the
retrieved items had no readable text" both return an answer explaining the situation rather
than an error. A useless model response is the third case of the same kind, and it was the
only one that behaved differently.

The distinction between an invalid response and a transport failure is the distinction between
a bad answer and a broken dependency. The first is a normal outcome of asking a small model a
question, and the user can work around it by reading the sources. The second is an operational
condition: it needs to reach the operator as a failure, with the 504 that names the remedy,
because no amount of reading sources will fix Ollama being down.

Stating the rejection reason matters. Silently omitting the prose would look like the model
had nothing to say about material that plainly addresses the question.

## Consequences

**Good:** a weak model degrades the experience instead of removing it. The guarantees that do
not depend on the model — sources shown, contradictions surfaced independently of what the
model said — hold in exactly the case where the model is least trustworthy.

**Bad:** a caller can no longer treat "no error" as "an answer was generated". `Sufficient` and
the uncertainty list have to be read, which the API documents and the interface renders, but
it is more to get right than a bare error would be.

**Rejected alternative:** retrying on rejection. A permanently invalid response does not become
valid by repetition, and retrying spends the user's time — 100 seconds per attempt on CPU — to
reach the same place. Rejections are counted on
`relata_model_invalid_output_total{task,reason}` so a model that does this constantly is
visible, which is the actionable signal.
