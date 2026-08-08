# Relata

A self-organising semantic document database.

Relata accepts unstructured and semi-structured information, analyses it, discovers
evidence-backed relationships between stored items, and lets you search associatively —
returning an explanation of *why* each result came back.

The central idea: **store arbitrary information, discover evidence-backed relationships
between items, and retrieve related knowledge with an explanation of why each result was
returned.**

---

## What Relata does

- **Ingests** plain text, Markdown, JSON, PDF and DOCX — pasted, uploaded, or posted to a
  JSON API. The original is always preserved verbatim.
- **Extracts** text while keeping headings, paragraphs, page numbers and document sections.
- **Chunks** documents along structural boundaries rather than at arbitrary character counts.
- **Embeds** chunks with a local model and stores the vectors in Qdrant.
- **Extracts entities and claims** in two stages: deterministic rules first, then
  model-assisted extraction for broader concepts. Every record says which produced it.
- **Discovers candidate relationships** between items from a bounded candidate set, and
  scores them from named, inspectable signals.
- **Searches** with hybrid retrieval — exact matching, MongoDB text search, vector
  similarity, entity matching and relationship expansion — and explains every result.
- **Answers questions** from stored material only, with citations, an explicit
  "Sources used" section, and an "Uncertainty and conflicts" section when sources disagree.
- **Keeps humans in charge**: every inferred relationship is a proposal until someone
  confirms it, and rejecting one never deletes the evidence behind it.

## What Relata does not do

These are deliberate limits, not oversights.

- **It does not decide what is true.** A claim records that a source *said* something.
  When two sources disagree, Relata shows both and says they conflict; it never picks a winner.
- **It does not treat similarity as a relationship.** Two documents can be near-identical in
  wording and describe entirely different events. Semantic similarity alone tops out well
  below the threshold at which a link looks well-evidenced.
- **It does not produce calibrated probabilities.** A relationship's strength is a weighted
  sum of explicit signals under a versioned scoring model. It is a heuristic strength, and
  the interface says so wherever it appears.
- **It does not merge entities on name similarity.** "Example Ltd" and "Example Holdings Ltd"
  stay separate. Similar names become *candidate aliases* for a person to confirm — and even a
  confirmed alias does not merge the records; it becomes one weighted signal among many.
- **It does not ask a language model for a score.** A model may classify a relationship type
  or describe a connection; that contributes exactly one signal among many.
- **It does not OCR.** A scanned PDF with no text layer is stored and preserved, and Relata
  tells you its contents are not searchable.
- **It does not accept archives** (zip, tar, gz, rar, 7z) or executables.
- **It does not fetch URLs found inside documents.** Nothing in a stored document can cause
  Relata to make an outbound request.
- **It is not multi-tenant yet.** Every record carries a workspace ID and every query filters
  on it, so the boundary is enforced from day one — but there is one workspace in practice.

---

## Architecture overview

A modular monolith with in-process background workers. One binary, three data stores.

```
                    ┌──────────────────────────────────────────┐
   browser ────────▶│  HTTP: server-rendered HTML + /api/v1    │
   API client ─────▶│  auth · CSRF · CSP · rate limits         │
                    └───────────────┬──────────────────────────┘
                                    │
                    ┌───────────────▼──────────────────────────┐
                    │  Services: ingest · search · relationships│
                    │            entities · analysis            │
                    └───────────────┬──────────────────────────┘
                                    │
              ┌─────────────────────┼─────────────────────┐
              ▼                     ▼                     ▼
    ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐
    │ MongoDB          │  │ Qdrant           │  │ Ollama           │
    │ authoritative:   │  │ vectors + minimal│  │ embeddings +     │
    │ items, chunks,   │  │ retrieval payload│  │ analysis         │
    │ entities, claims,│  │                  │  │ (replaceable)    │
    │ relationships,   │  │                  │  │                  │
    │ jobs, audit      │  │                  │  │                  │
    └──────────────────┘  └──────────────────┘  └──────────────────┘
              ▲
              │
    ┌─────────┴──────────────────────────────────────────────────┐
    │ Background workers: leased job queue in MongoDB            │
    │ parse → chunk → embed → entities → claims → relationships  │
    └────────────────────────────────────────────────────────────┘
```

`internal/domain` holds the types and port interfaces and imports only the standard library.
Storage and AI adapters implement those ports; the service layer never sees a MongoDB, Qdrant
or HTTP type. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

---

## Prerequisites

- **Go 1.26.5 or newer** — earlier 1.26 patches carry an `html/template` XSS fix Relata needs
- **Docker** and the Compose plugin, for MongoDB, Qdrant and Ollama
- **~8 GB free disk** for the Ollama models
- Linux or macOS. Windows via WSL2.

Ollama can run either in Compose or natively on the host — Relata only needs an HTTP endpoint.

---

## Exact local startup

```bash
# 1. Fetch dependencies and create .env from the example
make setup

# 2. Start MongoDB, Qdrant and Ollama
make compose-up

# 3. Pull the models (a few GB, once)
make models

# 4. Create the administrator account (prompts for a password, no echo)
make createadmin

# 5. Run Relata
make dev
```

Then open **http://127.0.0.1:8080** and sign in.

To confirm everything is healthy before signing in:

```bash
curl -s http://127.0.0.1:8080/health/ready | jq
```

`mongodb` and `qdrant` must report `ok`. The AI providers are reported but not required —
ingestion, keyword search and browsing all work without them.

### Initial administrator creation

```bash
relata createadmin --username admin
```

You are prompted for a password twice, without echo. The minimum length is 12 characters;
length is what matters, so a passphrase is fine. The password is hashed with Argon2id
(64 MiB, 3 passes) and the plain text is never stored, logged or echoed.

For unattended setup, set `RELATA_ADMIN_PASSWORD` in the environment instead. The password
is deliberately **not** accepted as a command-line flag, because arguments are visible in
the process list and shell history.

### Signing in through an identity provider

Relata can sit behind OAuth 2.0 / OpenID Connect. The redirect URI to register is derived from
`RELATA_BASE_URL` and a fixed path:

```
<RELATA_BASE_URL>/auth/oauth/callback
```

The path is not configurable, so what you register and what Relata serves cannot drift apart.

```bash
RELATA_OAUTH_ENABLED=true
RELATA_OAUTH_PROVIDER_NAME=Blackwall
RELATA_OAUTH_ISSUER=https://www.blackwall.cx      # or the three endpoints directly
RELATA_OAUTH_CLIENT_ID=...
RELATA_OAUTH_CLIENT_SECRET=...
```

Each person who signs in gets a **private workspace of their own** and is admin of it. Nobody
sees anyone else's documents — which also means relationship discovery never spans users. That
trade-off is the point of [ADR 8](docs/decisions/0008-federated-identity-private-workspaces.md).

Local password login switches itself off once OAuth is configured, and back on if it is not.
Set `RELATA_LOCAL_LOGIN_ENABLED=true` to keep it as a way back in when the provider is broken;
`relata createadmin` works regardless. A federated account has no password and can never sign
in locally.

Startup refuses a configuration with no way in at all, and refuses non-HTTPS provider endpoints
outside development.

Full detail: [docs/AUTHENTICATION.md](docs/AUTHENTICATION.md).

### API tokens

Programs authenticate with bearer tokens created in **Settings → API tokens**, carrying any of
`ingest`, `query` and `review`:

```bash
curl -H "Authorization: Bearer $RELATA_TOKEN" \
  'http://127.0.0.1:8080/api/v1/search?q=delivery+schedule'
```

A token is shown once — only its hash is stored — never carries more than its owner's current
role, cannot manage tokens, and cannot reach the web interface.

### Ollama model setup

Relata uses two models, both replaceable:

| Purpose | Default | Why |
|---|---|---|
| Embeddings | `nomic-embed-text` (768 dims) | Small, fast, good retrieval quality |
| Analysis | `llama3.1:8b` | Entity/claim extraction, relationship classification, answers |

```bash
ollama pull nomic-embed-text
ollama pull llama3.1:8b
```

On a CPU-only machine, a smaller analysis model is much more comfortable:

```bash
ollama pull llama3.2:3b
# then in .env:
RELATA_ANALYSIS_MODEL=llama3.2:3b
```

To run **without** a language model entirely, set `RELATA_ANALYSIS_ENABLED=false`. Ingestion,
chunking, embedding, deterministic entity and claim extraction, relationship scoring and
search all continue to work; model-assisted extraction and generated answers are disabled.

Changing the embedding model changes the vector geometry. Relata detects a dimension mismatch
and refuses to write mixed-width vectors. After such a change, set
`RELATA_EMBEDDING_DIMENSIONS` to match and re-embed the workspace from **Settings →
Re-embed workspace**.

---

## Demonstration dataset

```bash
make seed          # load and return
make seed-wait     # load and wait for processing to finish
```

Eleven documents chosen so that the difference between *similarity* and *evidence* is visible:

| Document | What it demonstrates |
|---|---|
| Atlas delivery schedule (original) | Related to, and superseded by, the revision |
| Atlas revised delivery schedule | Updates the original; shares entities and subject |
| Atlas invoice reconciliation | **Contradicts** the original about the project cost |
| Returns policy v3 / v2 | An update that **supersedes** older information |
| Northfield fire drill / safety inspection | Superficially similar, **different events** |
| Example Ltd sponsorship announcement | Shares an entity but **not** a subject |
| Atlas revised delivery — forwarded copy | A **likely duplicate**, flagged not merged |
| Office plant care rota | **Unrelated** to everything |
| Atlas shipment record (JSON) | Structured data sharing entities and figures |

Demonstration searches:

- *What information relates to the revised delivery date?*
- *Which sources disagree about the project cost?*
- *Show information associated with Example Ltd.*
- *What replaced the original policy?*
- *Find items similar in meaning but not actually about the same event.*

---

## Example API requests

The API uses the same session cookie as the browser, and enforces CSRF on state-changing
requests. Sign in first and keep the cookie jar.

```bash
BASE=http://127.0.0.1:8080

# Sign in (the login form's own CSRF token comes from the page)
TOKEN=$(curl -s -c cj.txt $BASE/login \
  | grep -o 'name="csrf_token" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"//')

curl -s -b cj.txt -c cj.txt -X POST $BASE/login \
  --data-urlencode "username=admin" \
  --data-urlencode "password=YOUR-PASSWORD" \
  --data-urlencode "csrf_token=$TOKEN"

# The session's CSRF token, for subsequent writes
CSRF=$(curl -s -b cj.txt $BASE/ \
  | grep -o 'name="csrf_token" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"//')
```

**Store a note**

```bash
curl -s -b cj.txt -X POST $BASE/api/v1/items \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{
        "title": "Delivery update",
        "content": "The revised delivery date is 14 March 2025. Example Ltd will ship 240 units.",
        "source_type": "note",
        "metadata": {"project": "atlas"},
        "created_at": "2025-02-28T09:00:00Z"
      }'
```

Returns `202 Accepted` with the item and the ID of the job that will process it.
Identical content returns `409` with `existing_item_id`; add `"allow_duplicate": true` to
store it anyway.

**Upload a document**

```bash
curl -s -b cj.txt -X POST $BASE/api/v1/items/upload \
  -H "X-CSRF-Token: $CSRF" \
  -F 'file=@contract.pdf' \
  -F 'title=Supply contract' \
  -F 'metadata[project]=atlas'
```

**Search**

```bash
curl -s -b cj.txt \
  "$BASE/api/v1/search?q=revised+delivery+date&limit=5" | jq '.search.results[] | {
     title: .item.title,
     query_relevance,
     semantic_similarity,
     why: .why_matched
   }'
```

**Ask a question, answered only from stored material**

```bash
curl -s -b cj.txt -X POST $BASE/api/v1/search/answer \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"question": "Which sources disagree about the project cost?"}' | jq
```

**Confirm that two names refer to the same thing**

```bash
curl -s -b cj.txt -X POST "$BASE/api/v1/aliases/$ALIAS_ID/confirm" -H "X-CSRF-Token: $CSRF"
```

Candidate aliases appear on an entity's page. Confirming one records a human judgement that
becomes evidence during relationship discovery; the two entities remain separate records.

**Inspect and review a relationship**

```bash
curl -s -b cj.txt "$BASE/api/v1/items/$ITEM_ID/relationships" | jq '.outgoing[0].explanation'

curl -s -b cj.txt -X POST "$BASE/api/v1/relationships/$REL_ID/confirm" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"note": "Checked against the signed contract."}'
```

**Preview a deletion before committing**

```bash
curl -s -b cj.txt -X DELETE -H "X-CSRF-Token: $CSRF" \
  "$BASE/api/v1/items/$ITEM_ID?dry_run=true" | jq .plan
```

The full specification is in [docs/openapi.yaml](docs/openapi.yaml).

---

## Running the tests

```bash
make test               # unit tests with the race detector
make test-integration   # against disposable MongoDB and Qdrant containers
make test-security      # security regression tests
make fuzz               # short run over all 15 fuzz targets: parsers, model output,
                        #   API decoding, filenames, entity and claim extraction, scoring
make lint               # gofmt check, go vet, staticcheck
make security           # govulncheck + security tests + secret check
make check              # everything CI runs
```

Some of those tests exercise concurrency rather than behaviour, because two of the defects
found in this codebase were only reachable under it: duplicate detection is a read followed by
a write, and asserting a relationship used to page through a node's links. Both are now
enforced by unique indexes and covered by `tests/race_test.go`.

Integration tests need Docker. They start their own containers, use a database named after
the run, and clean up afterwards. To point them at instances you already have:

```bash
RELATA_TEST_MONGO_URI=mongodb://127.0.0.1:27017 \
RELATA_TEST_QDRANT_URL=http://127.0.0.1:6333 \
go test -tags=integration ./tests/...
```

---

## Resetting the local environment

```bash
make backup     # capture MongoDB and the uploaded originals first
make reset      # asks for confirmation, then removes all volumes and uploads
```

Or piecemeal:

```bash
docker compose down -v          # drop MongoDB and Qdrant data
rm -rf data/uploads/*           # drop stored original files
make createadmin                # create an account again
```

To clear only the vectors and rebuild them, use **Settings → Re-embed workspace**.

---

## Configuration

Every setting is an environment variable, documented in [`.env.example`](.env.example), which
`make setup` copies to `.env`. Real environment variables take precedence over the file.

The settings you are most likely to change:

| Variable | Default | Notes |
|---|---|---|
| `RELATA_HTTP_ADDR` | `127.0.0.1:8080` | Non-loopback requires `RELATA_ALLOW_EXTERNAL_BIND=true` |
| `RELATA_EMBEDDING_MODEL` | `nomic-embed-text` | Changing it requires re-embedding |
| `RELATA_ANALYSIS_MODEL` | `llama3.1:8b` | Use `llama3.2:3b` on CPU-only machines |
| `RELATA_ANALYSIS_ENABLED` | `true` | `false` runs the deterministic pipeline only |
| `RELATA_WORKER_COUNT` | `3` | Background workers |
| `RELATA_AI_CONCURRENCY` | `2` | Concurrent model calls, independent of workers |
| `RELATA_REL_STORE_SCORE` | `0.25` | Strength below which a candidate is discarded |
| `RELATA_MAX_UPLOAD_BYTES` | `26214400` | 25 MB |

Configuration is validated at startup: Relata refuses to start with a clear list of every
problem rather than failing later inside a request.

**`.env` must never be committed.** It is in `.gitignore`, and `make security` fails if it
is tracked.

---

## Known limitations

Stated plainly, because a system that discovers relationships should be honest about its own.

**Relationship scoring**
- Scores are **heuristic strengths, not calibrated probabilities**. The weights in
  `relationship-score-v1` were chosen by judgement, not fitted to labelled data. A 0.7 does
  not mean "70% likely to be a real relationship".
- Changing weights changes every future score. Existing relationships keep the version that
  produced them; they are not recomputed automatically.
- Human confirmations and rejections influence ranking and expansion. They do **not** retrain
  any model, and Relata never sends your decisions anywhere.

**Extraction quality**
- Deterministic entity extraction is intentionally narrow: organisations with a legal suffix,
  people with an honorific, email addresses and dates. It will miss a person named without a
  title. Broader coverage comes from the model stage and is labelled as such.
- Deterministic claim extraction only picks up sentences carrying a figure or a date, because
  those are the statements that can actually agree or conflict. Prose claims need the model.
- Contradiction detection compares figures and dates about the same coarse subject. It will
  not catch a contradiction expressed entirely in prose.
- Model output quality varies considerably with model size. A 3B model produces noticeably
  weaker entity and relationship classification than an 8B one, and its generated answers are
  often unusable — though the safeguards still hold: an uncited answer is downgraded to
  "not sufficient", and recorded contradictions are surfaced regardless of what the model says.
  A model that answers with the schema itself (`llama3.2:3b` returned `{"answer": "sufficient"}`)
  is rejected as `degenerate_content` rather than displayed; the request still returns the
  retrieved sources and any recorded contradictions, since neither depends on the model. That
  check catches obvious emptiness only — it cannot distinguish a weak answer from a good one,
  so a small model still produces answers not worth reading, it just no longer produces
  answers that are not answers.
- **Answer generation on CPU is slow.** With `llama3.1:8b`, three sources take about 33
  seconds. The defaults (six passages, a 300-second model timeout) are set to complete on a
  CPU-only machine; exceeding them returns a 504 that names the remedy. A GPU changes this by
  an order of magnitude. See [docs/OPERATIONS.md](docs/OPERATIONS.md#answer-generation-times-out).

**Search**
- Query relevance is a fused rank score normalised against the best result in the same
  response. It is comparable *within* one result set, not across queries.
- MongoDB text search is English-stemmed by default. Other languages match less well.
- Relationship expansion only follows links from the strongest matches, bounded by
  `RELATA_SEARCH_MAX_EXPANDED_ITEMS`.

**Scale**
- Designed for a personal or small-team corpus — thousands to low tens of thousands of items.
  Alias proposal compares new entities against up to 200 existing ones of the same type per
  run, which is bounded but not free at large entity counts.
- Relationship discovery indexes at most 500 **confirmed** aliases per workspace. Beyond that,
  further confirmed aliases stop contributing their signal; discovery weakens rather than
  breaking, and a warning is logged rather than the loss happening silently.
- One binary, deliberately — no sharding, no coordination layer, no service mesh. Running
  more than one instance against the same MongoDB does work: the job queue leases rather than
  locks, so instances share the workload and an instance that dies has its jobs requeued when
  the lease expires. That was verified with two instances, but it is a property of the queue
  design rather than a supported deployment topology, and nothing else is coordinated.

**Formats**
- No OCR. Scanned PDFs are preserved and flagged as having little extractable text.
- PDF extraction handles ordinary text layouts; complex multi-column layouts and tables may
  come out in an awkward order.
- DOCX extraction reads paragraph text and heading styles. Tables, footnotes, comments and
  tracked changes are not extracted.

**Authentication**
- **No refresh tokens.** The provider's access token establishes identity once and is then
  discarded rather than stored. A revoked upstream account keeps its Relata session until that
  session expires (idle 12h, absolute 7 days by default); revoke the Relata session too if that
  matters.
- **No group or role mapping.** Every federated user is admin of their own workspace. There is
  nothing to map a provider group onto, because there is no shared workspace to have roles in.
- **No account linking.** A local account and a federated account with the same email address
  are two separate people with two separate workspaces. Merging them is deliberate manual work,
  not something to infer from a matching address.
- **Token scopes are coarse** — three of them, checked per route, with no per-item restriction.
- Creating a token through the interface puts its value in the browser's history for one page
  load; the page says so. `POST /api/v1/tokens` returns it in a response body instead.

**Security**
- Prompt-injection defences (delimited untrusted blocks, an unforgeable fence, strict output
  schemas, no tools available to the model) make injection much harder and bound its effect.
  They are not a proof. Treat generated answers as summaries of sources, not as authority.
- Relata trusts its operator. There is no protection against a malicious administrator.
- Sessions are stored server-side and revocable, but there is no device management, no
  two-factor authentication, and no password reset flow — recreate the account instead.

The threat model, including what is explicitly out of scope, is in
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md).

---

## Documentation

| Document | Contents |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Package layout, dependency rules, data model |
| [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) | Trust boundaries, defences, known limitations |
| [docs/SEARCH_AND_SCORING.md](docs/SEARCH_AND_SCORING.md) | The three scores and how each is computed |
| [docs/INGESTION_PIPELINE.md](docs/INGESTION_PIPELINE.md) | Stage by stage, with failure behaviour |
| [docs/AI_PROVIDER_INTERFACE.md](docs/AI_PROVIDER_INTERFACE.md) | Adding a provider without touching the pipeline |
| [docs/BACKUP_AND_RESTORE.md](docs/BACKUP_AND_RESTORE.md) | What to back up and how to restore it |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Running it: metrics, logs, common problems |
| [docs/IMPLEMENTATION_PLAN.md](docs/IMPLEMENTATION_PLAN.md) | The plan this was built to |
| [docs/AUTHENTICATION.md](docs/AUTHENTICATION.md) | Sign-in, workspaces and API tokens |
| [docs/decisions/](docs/decisions/) | Architectural decision records |

---

## Licence

Provided as-is for evaluation.
