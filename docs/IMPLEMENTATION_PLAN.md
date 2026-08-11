# Relata — Implementation Plan

Status: living document. Written before implementation, updated as decisions were made.

## Goal

A self-organising semantic document database: ingest arbitrary information, discover
**evidence-backed** relationships between stored items, and retrieve related knowledge with
an explanation of *why* each result was returned.

## Non-negotiable invariants

These shape every design choice below.

1. **Originals are never rewritten.** The raw bytes/text a user supplied are stored verbatim.
   Everything else (extracted text, chunks, entities, claims) is derived and reproducible.
2. **Three different numbers, never conflated:**
   - *semantic similarity* — cosine distance between embeddings,
   - *relationship strength* — a versioned weighted sum of explicit signals,
   - *query relevance* — a fused retrieval score for one specific query.
   Each is stored in its own field and rendered with its own label.
3. **Provenance for every inference.** Each relationship records how it came to exist
   (`explicit` / `deterministic` / `model_inferred` / `user_confirmed` / `user_rejected`)
   and carries the signal list that produced its score.
4. **Model output is untrusted input.** It is parsed with `json.Decoder` against a strict
   schema, enum-validated, and referenced IDs are checked against what was actually sent.
   Invalid output is recorded and discarded, never repaired by regex.
5. **Stored document content is data, never instruction.** Retrieved text is delimited and
   the LLM is told to treat it as untrusted quoted material.

## Architecture

Modular monolith + in-process background workers. One binary, several subcommands.

```
cmd/relata            serve | createadmin | seed | version
internal/domain       pure types + port interfaces. No I/O imports.
internal/config       env + file config, validated at startup
internal/parsing      text, markdown, JSON, PDF, DOCX -> ExtractedDoc
internal/chunking     structure-aware chunker
internal/embeddings   Embedder port + Ollama adapter + batching/retry
internal/analysis     LLM port + Ollama adapter + strict JSON schema validation
internal/entities     normalisation, deterministic + model-assisted extraction
internal/relationships candidate discovery, versioned scoring, review
internal/search       hybrid retrieval (lexical + vector + entity + relationship expansion)
internal/ranking      score fusion and explanation building
internal/ingest       pipeline orchestration
internal/jobs         Mongo-backed leased job queue + bounded worker pool
internal/storage/mongo  repositories implementing domain ports
internal/storage/qdrant vector store implementing domain ports
internal/auth         argon2id, sessions, CSRF, login rate limiting,
                      OAuth 2.0 + OIDC sign-in, per-user workspace provisioning,
                      hashed and scoped API tokens
internal/api          /api/v1 REST
internal/web          server-rendered HTML + HTMX
internal/audit        audit event recording
internal/observability structured logs, request IDs, Prometheus metrics
```

**Dependency rule:** `internal/domain` imports only the standard library. Storage adapters
import domain, never the reverse. `api` and `web` depend on services, not on storage.

### Why ports live in `domain`

Repository and provider interfaces are declared in `internal/domain` next to the types they
carry, so the service packages compile against domain alone. Adapters
(`storage/mongo`, `storage/qdrant`, `embeddings`, `analysis`) implement them.

## Data model (MongoDB)

`users, sessions, workspaces, items, chunks, entities, entity_aliases, claims,
relationships, jobs, processing_runs, model_versions, audit_events, api_tokens`

- String UUID `_id` everywhere. Chunk IDs double as Qdrant point IDs (Qdrant accepts UUIDs),
  which removes an entire class of ID-mapping bugs.
- **Every** user-content collection carries `workspace_id`, and every repository method takes
  a workspace and includes it in the filter. There is no repository method that can read
  user content without a workspace.
- Qdrant payloads hold only `workspace_id, item_id, chunk_id, source_type, created_at,
  embedding_model` — retrieval metadata, not a second document store.

## Pipeline

`ingest -> parse -> chunk -> embed -> extract entities -> extract claims -> discover relationships`

Each stage is a persistent job with a lease. Stages are idempotent: they key derived records
off deterministic hashes so a retry replaces rather than duplicates.

Candidate relationship discovery never does an all-pairs comparison. It gathers a bounded
candidate set from (a) vector neighbours of the new item's chunks, (b) items sharing a
normalised entity, (c) items in a temporal window, then scores that set.

## Scoring

`relationship-score-v1`: weighted sum of independent signals (semantic similarity, exact
entity overlap, alias overlap, shared concepts, temporal proximity, shared metadata, claim
support/contradiction, model classification, user confirmation/rejection), each stored with
its own value and weight so the UI can show the arithmetic. The LLM contributes **one
signal** — never the score.

## Build order

1. domain, config, ids, observability
2. storage/mongo, storage/qdrant
3. parsing, chunking
4. embeddings, analysis (ports + Ollama adapters)
5. jobs queue + workers
6. entities, claims, relationships, ingest
7. search, ranking, answer generation
8. auth, api, web
9. seed, docs, tests, Docker/Makefile

## Deliberate scope limits (MVP)

- No OCR. Scanned PDFs are reported as low-extractable-text, original preserved.
- No archive formats (zip/tar/gz) — rejected at upload.
- Single workspace in practice, but the boundary is enforced in code and storage from day one.
- Entity resolution proposes aliases; it never auto-merges on name similarity alone.
- Relationship scores are heuristic strengths, not calibrated probabilities. Stated in the UI.

---

## Outcome

Built as planned. The notes below record what changed during implementation and what running
the system revealed, because a plan that claims to have gone exactly to plan is not a useful
document.

### Bugs found by running it, not by reading it

Six defects survived code review and were caught by operating the system, testing it under
concurrency, or tightening validation elsewhere. Each now has a regression test.

1. **Worker deadlock.** `execute` waited for the heartbeat goroutine to exit, while that
   goroutine's only exit was the job context being cancelled — which happened via `defer`
   *after* `execute` returned. Every worker wedged after one job; three workers meant three
   stuck jobs and a queue that never drained. Found by watching the queue page stop moving.
   `TestWorkerCompletesJobsAndDoesNotDeadlock` queues more jobs than there are workers.

2. **Dates leaking into figures.** The number pattern could begin at the whitespace *before* a
   date and take `202` out of `2025-02-10`. The overlap check tested only the match's start
   offset, so the fragment survived and was then compared against other documents' quantities —
   manufacturing contradictions between documents that agreed. Found by reading the evidence
   behind a suspicious `contradicts`. Fixed by testing the whole span.

3. **Mirror-image relationships.** The dedupe key included endpoint order, so a link found
   while processing A and rediscovered while processing B became two records. Every pair in
   the demonstration set was duplicated. `PairKey` now normalises the order.

4. **Incomparable ranking curves.** Scaled contributions (entity match, relationship
   expansion) used `weight × strength / 15` while ranked contributions used
   `weight / (60 + rank)`. A full-strength expansion therefore outranked a direct keyword hit —
   "related to a match" beating "is a match". Both now sit on the same reciprocal-rank curve.

5. **Duplicate detection that a race could defeat.** Exact-duplicate detection read the
   content hash and then inserted, with only a non-unique index underneath. Eight concurrent
   uploads of identical content stored four items each claiming to be the only one — so
   "an identical item already exists" was simply untrue under load. Found by writing the
   concurrency test rather than by observing it. The fix puts the guarantee where it belongs:
   a partial unique index on `(workspace_id, content_hash)` covering live, non-deliberate
   items, with the insert's duplicate-key error reported as exactly the same `DuplicateError`
   the check produces, so callers see one answer regardless of which path caught it.
   Deliberate duplicates carry `duplicate_of` and sit outside the constraint.

6. **A useless model answer failed the whole request.** Adding degeneracy validation to model
   output surfaced this: `Answer` returned `nil, err` for any generation failure, discarding
   the retrieved sources and the recorded contradictions between them. Both are produced
   deterministically and neither depends on the model, so the one case where a user most needs
   to read the sources themselves was the case that showed them nothing. Invalid output now
   degrades — no prose, sources and conflicts retained, rejection reason stated — matching the
   no-material and no-readable-text paths beside it. A timeout or unreachable provider still
   errors, because that is an operational condition rather than a bad answer.

A seventh, found by fuzzing rather than running: the Markdown parser set a heading block's `Text`
to the trimmed line while its offsets spanned the untrimmed one, breaking the invariant that
`block.Text == doc.Text[block.Start:block.End]` — on which every claim and entity source
reference depends. The failing input is retained as a corpus seed.

### Deliberate changes from the plan

- **`MaxUnreviewedScore = 0.95`.** Not in the original design. Without it a candidate with
  enough accumulated signals reached 1.00 — the same score as a human confirmation — and the
  interface presented them identically. That conflates "much evidence" with "confirmed", which
  is the distinction the whole design exists to preserve.
- **Alias signals require a *confirmed* alias.** The plan listed `alias_entity` as a signal
  without saying which aliases counted. Using unconfirmed candidates would have made a guess
  into evidence, defeating the point of proposing aliases for review.
- **Claim subjects moved to a two-sided rule.** For a value the subject precedes the figure
  ("the cost is £42,000"); for a quantity it follows ("ship 240 **units**"). The one-sided
  rule produced subjects like "ship" and "ltd", which paired unrelated claims.
- **A model that says "unrelated" no longer also contributes its type suggestion.** Both
  signals were being recorded, so a model could simultaneously argue against and for a link.
- **No `scripts/openapi` generator.** The specification is hand-written and a test asserts it
  matches the routes the router actually registers, plus every enum and error code. A
  generator would have produced a specification that could only agree with itself.

### Verified, with the caveats that apply

Run against MongoDB 7, Qdrant 1.12, and Ollama with `nomic-embed-text` and `llama3.1:8b` on a
CPU-only machine.

- The full pipeline completes for all eleven demonstration documents.
- The two superficially similar depot reports acquire **no** relationship, and the note sharing
  only the entity "Example Ltd" acquires none either — sharing wording, or an entity, is not a
  relationship.
- The cost contradiction is detected deterministically, with both figures named, and is
  surfaced in generated answers **independently of what the model says**.
- Deleting an item leaves no orphaned vectors, chunks, claims or entities.
- Reprocessing twice changes no derived-record count, and a human review decision survives it.
- Two Relata instances against one MongoDB cooperated correctly through the leased queue —
  unplanned, but a genuine test of the lease design.

Known weak points are listed in the README rather than here, so that a reader meets them
before they meet a surprise.
