# 6. Duplicate detection is enforced by an index, not by a check

**Status:** accepted

## Context

Ingestion hashes the incoming content, looks for an existing item with that hash, and reports
a duplicate if it finds one. Detection was therefore a read followed by a write, with only a
non-unique index underneath.

That is a time-of-check-to-time-of-use gap. Concurrent requests carrying identical content all
perform the read before any of them performs the write, so all of them find nothing and all of
them store an item. Eight concurrent uploads of the same document produced four items, each
having reported itself as the only copy.

The gap is not exotic. A retried upload, a double-clicked form, or two workers fed from the
same source hit it without anyone trying.

## Decision

A partial unique index on `(workspace_id, content_hash)`, covering only items where
`duplicate_of = ""` and `deleted = false`. The insert's duplicate-key rejection is mapped to
`domain.ErrDuplicate` and reported as exactly the same `DuplicateError` the pre-check produces.

The pre-check stays. It is what makes the common case a clean, cheap answer that names the
existing item; the index is what makes that answer *true*.

## Rationale

A uniqueness rule that a race can defeat is not a rule. The database is the only place where
"at most one" can be decided, because it is the only place where concurrent writers are
serialised. Anything above it is advisory.

The index is **partial** for two reasons, both of which a blanket unique index would break:

- A user may deliberately keep a second copy (`allow_duplicate: true`). Those items carry a
  non-empty `duplicate_of` and fall outside the filter, so any number of them can coexist —
  the link to the original is recorded rather than the copy being refused.
- A tombstoned item still occupies its row until it is hard-deleted. Excluding
  `deleted = true` means removing an item frees its content hash immediately, rather than
  blocking re-ingestion for the length of the deletion job.

Losing the race is handled according to what the caller asked for, not according to which
code path noticed. A request with `allow_duplicate: true` that loses the insert has still
asked for a second copy to be kept: the race only revealed *which* item it duplicates, so the
link is recorded and the insert retried, which succeeds because the item is now outside the
partial filter. Refusing it would make the flag mean "unless you were unlucky".

Reporting the same error from both paths matters more than it appears. A caller that sees
`DuplicateError` from the check and an opaque write failure from the race would have to handle
two outcomes for one condition, and the seed command, the API and the web upload form would
each get it subtly differently.

`duplicate_of` is stored unconditionally rather than with `omitempty`, because a partial index
filter matches an empty string but not an absent field. Items written before that change need
a one-off backfill, which `EnsureIndexes` performs itself; it is idempotent and logs what it
touched.

## Consequences

**Good:** the guarantee holds under concurrency, and the enforcement lives next to the data
rather than in whichever caller remembered to check. Deliberate duplicates are unaffected.

**Bad:** an existing deployment whose data already violates the constraint cannot build the
index, and startup fails. That is deliberate — silently proceeding would leave the guarantee
untrue while claiming otherwise — but it means the failure message has to name the fix, and
`migrations/README.md` has to carry the query that finds the offending rows. Nothing is
deleted automatically: which copy is canonical is not a decision Relata should make for you.

**Rejected alternative:** a transaction around check-and-insert. It would work, but it makes
every ingest pay for a case that is rare, requires a replica set, and still leaves the
invariant expressed in application code where a future caller can bypass it.
