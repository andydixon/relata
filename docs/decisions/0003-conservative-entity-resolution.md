# 3. Entities merge only on an exact normalised key

**Status:** accepted

## Context

"Example Ltd", "Example Limited", "EXAMPLE LTD" and "Example" may all refer to one company.
Merging them makes shared-entity evidence stronger and the entity browser tidier.

## Decision

Entities are keyed on `(workspace, type, normalised_name)` and merge only on an **exact** key
match. Normalisation is shallow: case folding, whitespace, punctuation, apostrophes,
diacritics. It deliberately does **not** strip legal suffixes or expand abbreviations.

Names that look alike produce a **candidate alias** for a human to confirm. Nothing merges
automatically.

## Rationale

The failure modes are not symmetric.

Failing to merge costs a little evidence: two entity records where one would do, and a
slightly weaker shared-entity signal. Visible, harmless, fixable.

Merging wrongly is **silent data corruption**. If "Example Ltd" and "Example Holdings Ltd"
become one entity, every relationship built on that shared entity is now built on a false
premise, and nothing in the interface reveals it. Those are genuinely different companies far
more often than they are variant spellings of each other — parent and subsidiary, or two
unrelated firms with a common word.

The same asymmetry applies to people. "Dr A Chen" and "Alice Chen" are probably the same
person; "J Smith" and "John Smith" in a corpus of thousands are a coin toss.

So Relata proposes and a human disposes. The alias machinery uses a deliberately *looser* key —
suffixes stripped, plus token overlap, edit distance and acronym detection — precisely because
that key is unsafe for identity but useful for suggestion.

## Consequences

**Good:** no silent corruption of the entity graph. Shared-entity evidence means what it says.
The alias list makes near-matches visible rather than hiding them behind a merge.

**Bad:** more entity records than strictly necessary. Shared-entity signals are weaker until
someone confirms aliases. Confirmation is manual work.

**Note:** entity type is part of the key, so the place "Washington" and the person "Washington"
never collide.

**Confirming an alias does not merge anything.** It records that a person judged two names to
refer to the same thing, and that judgement becomes a `alias_entity` signal weighted at 0.10 —
below an exact name match at 0.25. The two entity records remain separate and independently
inspectable, so the decision is visible and reversible. An *unconfirmed* candidate contributes
nothing at all: treating a proposal as evidence would defeat the purpose of proposing it.
