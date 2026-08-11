# Ingestion pipeline

From "a user pastes a note" to "the note has evidence-backed relationships".

The governing rule: **the original is preserved.** Whatever a user supplies is stored verbatim
before any parsing is attempted. If parsing later fails, the failure is recorded as a warning
on the item and the original remains retrievable and reprocessable.

---

## Overview

```
   HTTP  ──▶  ingest.Service.Ingest  ──▶  items collection + parse_item job
                                                      │
   ┌──────────────────────────────────────────────────┘
   ▼
 parse_item ─▶ chunk_item ─▶ generate_embeddings ─▶ extract_entities
                                                          │
                          discover_relationships ◀── extract_claims
```

Each stage is a **separate job type**, so progress is visible on the queue page and a failure
is attributable to a specific stage. Stages hand off by enqueuing the next.

---

## Stage 0 — Acceptance

`internal/ingest/service.go`

Before anything is written:

1. **Admission control.** A semaphore bounds concurrent ingestion, because each in-flight
   request holds its content in memory.
2. **Size check** against `RELATA_MAX_UPLOAD_BYTES`.
3. **Content-based type detection.** `parsing.Sniff` decides from magic bytes. The browser's
   `Content-Type` and the filename are hints, never authorities.
   - PDF: `%PDF-`
   - DOCX: a zip containing `word/document.xml` — checked by reading the central directory
   - Archives (gzip, bzip2, xz, rar, 7z, plain zip): **rejected**
   - Executables (ELF, PE, Mach-O, `#!`): **rejected**
   - Text: must be valid UTF-8, no NUL bytes, under 2% control characters
   - JSON: parses as a single well-formed value
   - Markdown vs plain text: decided by filename or declared type, then by content signals
4. **Validation** of title length, metadata keys and values, and the user-asserted creation
   date (a date far in the future is refused, since it would distort every temporal signal).
5. **Exact-duplicate detection.** SHA-256 over the supplied bytes, looked up against
   `(workspace, content_hash)`. An identical item returns `409` with `existing_item_id`
   unless `allow_duplicate` is set, in which case `duplicate_of` records the link.
6. **Original storage.** Binary formats are written to
   `{upload_dir}/{workspace_id}/{item_id}.{ext}` with mode `0600`. The filename is the item's
   own UUID and the extension comes from a fixed allow-list — **the user's filename never
   influences the path.** Text formats are kept inline on the item.
7. **Item created**, then `parse_item` enqueued.

If the declared type and the detected type disagree, a `mime_mismatch` warning is stored. It
may be innocent, or it may be an attempt to smuggle one format past a check for another; either
way it is visible.

---

## Stage 1 — Parse

`internal/parsing`, handler in `pipeline.go`

Opens a **processing run**, then extracts text from the preserved original.

| Format | Approach | Retains |
|---|---|---|
| Plain text | Paragraph splitting on blank lines; RFC-822 headers recognised as a block | Paragraph offsets |
| Markdown | Line-based structural parse: headings, lists, fences, quotes, tables | Heading hierarchy as a section path |
| JSON | Depth-bounded streaming scan, then flattening to `path: value` lines, keys sorted | Object boundaries as sections |
| PDF | Page by page; a page that fails is skipped with a warning | Page numbers |
| DOCX | Streaming XML over `word/document.xml`; Word heading styles → structural headings | Heading hierarchy |

Every block records **byte offsets into the extracted text**, and the invariant
`block.Text == doc.Text[block.Start:block.End]` holds. Every derived record's source
reference depends on that identity; it is asserted in unit tests and fuzz-tested.

**Failure behaviour.** A parse failure is *not* a job failure. The warning is recorded, the
item is marked `failed`, the original is untouched, and the job returns successfully — the job
did its work correctly, and retrying would only repeat the same failure. The user sees the
warning on the item and can reprocess after fixing the cause.

**Quality warnings** raised here:
- `low_extractable_text` — a large file yielding almost no letters, usually a scan. Relata does
  not OCR, and says so rather than silently indexing nothing.
- `truncated_text` — output exceeded the configured limit; blocks past the cut are dropped and
  the straddling one clamped, so offsets stay valid.
- `json_depth_exceeded` — nesting or width beyond the limit.
- `parse_failed` — recorded per page for PDFs, or for the whole document.

---

## Stage 2 — Chunk

`internal/chunking`

Structure first, character counts only as a ceiling.

**Rules, in order:**

1. A document below `RELATA_CHUNK_MIN_SPLIT_CHARS` becomes **one chunk**. Splitting a
   two-sentence note produces fragments that embed worse than the whole.
2. A heading starts a new chunk, once the current one is large enough to stand alone.
3. A page change (PDF) is a real boundary.
4. A JSON object boundary is respected once the chunk has reached its target size.
5. Adding a block that would exceed `RELATA_CHUNK_MAX_CHARS` starts a new chunk.
6. A single block larger than the ceiling is split internally, preferring blank lines, then
   sentence terminators, then line breaks, then word breaks.
7. Chunks below `RELATA_CHUNK_MIN_CHARS` are merged into a neighbour.

**Overlap.** Each chunk after the first extends backwards by up to
`RELATA_CHUNK_OVERLAP_CHARS`, aligned to a word boundary, so a sentence spanning a boundary is
retrievable from both sides. The chunk's `Start` moves back with it, so
`chunk.Text == text[Start:End]` still holds; `OverlapChars` records how much was borrowed.

Each chunk stores: parent item, sequence, source offsets, page, section, heading, text,
content hash and (after embedding) the embedding model.

**Chunk IDs are deterministic** — derived from `(workspace, item, sequence, content_hash)` — so
re-chunking unchanged content reuses the same Qdrant point IDs and overwrites vectors rather
than orphaning them. Before writing new chunks, the item's existing vectors are deleted.

---

## Stage 3 — Embed

`internal/embeddings`

- Chunks are embedded in batches of `RELATA_EMBED_BATCH_SIZE`.
- An **AI semaphore** is held for the duration of the provider call, so concurrent model usage
  stays within `RELATA_AI_CONCURRENCY` regardless of worker count.
- **Dimension validation.** The first response fixes the width; a later mismatch is a
  *permanent* error, because writing differently-sized vectors into one collection would
  corrupt it. The error names the fix.
- **Bounded retry.** Only transport failures and explicitly transient HTTP statuses retry, with
  exponential backoff and full jitter, within `RELATA_AI_MAX_RETRIES`. A 400 or an unknown
  model never retries. Jitter matters because the worker pool dispatches in parallel: without
  it, a provider restart would be met by every worker retrying in lockstep.
- Vectors go to Qdrant with the minimal payload; chunks are marked embedded with the model name.
- **Document text is never logged.**

---

## Stage 4 — Entities

`internal/entities`

Two stages, separately labelled so a user can tell a matched pattern from a model's guess.

**Deterministic** (`method = deterministic`) — narrow by design, because a pattern that fires
on ordinary prose would flood the index and make shared-entity evidence meaningless:

- organisations ending in a legal suffix (`Ltd`, `PLC`, `GmbH`, `Inc`, …)
- people preceded by an honorific (`Dr`, `Prof`, `Ms`, …)
- email addresses
- dates in ISO, `14 March 2025`, `March 14, 2025` and `14/03/2025` forms, **normalised to
  ISO-8601** (day/month/year for the ambiguous numeric form, matching the application's
  British English conventions)

**Model-assisted** (`method = model`) — broader concepts, over a bounded sample of chunks.
Output is enum-validated per row; one bad row is skipped rather than invalidating the response.
A claimed evidence quotation that does not appear in the source is **dropped**, because a
fabricated quotation would break the promise that source references point at real text.

**Resolution.** Entities are keyed on `(workspace, type, normalised_name)` and merged only on
an **exact** key match. Normalisation is shallow — case folding, whitespace, punctuation,
apostrophes, diacritics — and deliberately does *not* strip legal suffixes: "Example Ltd" and
"Example Holdings Ltd" are different companies far more often than they are typos.

Names that look alike produce a **candidate alias** for a human to confirm, using a looser key
plus token overlap, edit distance and acronym detection. Nothing is merged automatically.

Every mention carries item-relative byte offsets and the verbatim quote.

---

## Stage 5 — Claims

A claim records that a source **said** something. It is not a fact, and it never loses its link
to the original text.

**Deterministic** — sentences carrying a figure or a date, because those are the statements
that can actually agree or conflict with another source. Sentence splitting knows common
abbreviations, so "Ltd. reported" stays one sentence.

For each, Relata derives comparison keys:
- **subject** — the content word governing the figure ("cost" in "the total project cost is
  £42,000")
- **numbers** — currency- and separator-aware
- **dates** — normalised to ISO-8601
- **polarity** — +1 affirmative, −1 negated

**Model-assisted** — broader claims, with attribution and a `quoted`/`paraphrased`/`inferred`
nature. A claim whose quotation cannot be located in the source is **discarded**; whitespace
reflow is tolerated, because that locates the same span rather than inventing one.

---

## Stage 6 — Discover relationships

`internal/relationships/discover.go`

**Candidates never come from an all-pairs comparison.** Three bounded, indexed sources:

1. **Vector neighbours** — a sample of the item's chunks is embedded and searched, filtered to
   the workspace and excluding the item itself.
2. **Shared entities** — an indexed aggregation over `entities.item_ids`, ordered by how many
   entities are shared. Concepts are excluded as a *source* (too broad) while still
   contributing a signal.
3. **Shared claim subjects** — an indexed lookup on `claims.subject`.

The union is capped at `RELATA_REL_MAX_CANDIDATES` before any scoring or model call.

**Evidence gathering** per candidate: shared entities (discounted by corpus frequency),
concept overlap, temporal proximity, shared metadata, claim comparison and content-hash match.

**Claim comparison** is what makes "which sources disagree about the cost?" answerable without
a language model: two claims about the same subject with materially different figures (beyond
a 0.1% tolerance) or different dates are a **contradiction**, recorded with both quotations
attached. Matching figures are **support**.

**Scoring**, then a bounded number of model classification calls — only for candidates that
already have real evidence, so model time is not spent on noise. The model contributes one
signal; a maximally confident opinion cannot on its own reach the storage threshold.

**Storage.** Candidates below `RELATA_REL_STORE_SCORE` are discarded. The rest are stored as
`pending` review with full evidence. Non-directional types are stored in a canonical endpoint
order so A~B and B~A are one relationship. A near-duplicate (similarity above
`RELATA_NEAR_DUPLICATE_SIMILARITY`) is flagged with an explicit note — **never merged**.

---

## Failure and recovery

| Failure | Behaviour |
|---|---|
| Parse fails | Warning recorded, item marked `failed`, original preserved, job succeeds |
| Embedding provider down | Transient — retried with backoff, then the job fails and can be retried from the queue page |
| Model returns invalid JSON | Recorded with a reason, **not** retried; deterministic results are kept |
| Model unavailable | Entity and claim extraction degrade to deterministic only; the pipeline continues |
| Worker crashes | The lease expires and the job returns to the queue |
| Job exceeds its attempts | Marked failed; retryable manually |
| Item deleted mid-pipeline | Permanent failure — the stage stops rather than resurrecting derived data |

---

## Reprocessing

`RequestReprocess` opens a **new processing run** and restarts the chain at parsing, with the
run ID in the job's idempotency key so it is never suppressed as a duplicate of the original
ingest.

Nothing multiplies:

| Records | Why not |
|---|---|
| Chunks | Replaced wholesale |
| Vectors | Deterministic IDs; the item's vectors are cleared before re-chunking |
| Claims | Replaced wholesale; unique dedupe key |
| Entities | Upserted on the normalised key; observations accumulate |
| Relationships | Upserted on the dedupe key — and **a human review decision is never overwritten** |

Previous processing runs are **retained**, recording the parser, embedding model, analysis
model and scoring version that produced each generation. That is the audit trail for "what did
the old parser make of this?"

Reprocess when a parser changes, the embedding model changes, the analysis model changes, or
the relationship scoring algorithm changes. For an embedding-model change across the whole
corpus, use **Settings → Re-embed workspace**, which processes in bounded batches and
re-enqueues itself rather than holding one lease for hours.
