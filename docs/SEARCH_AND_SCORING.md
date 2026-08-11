# Search and scoring

Relata computes **three different numbers** and keeps them apart everywhere — in storage, in
the API, and on screen. Conflating them would make every explanation meaningless.

| Number | Question it answers | Range | Where it lives |
|---|---|---|---|
| **Semantic similarity** | How alike are these two pieces of text? | ~0–1 cosine | `VectorMatch.Score`, `Result.semantic_similarity` |
| **Relationship strength** | How well evidenced is a link between two items? | 0–1 | `Relationship.score` + `score_version` |
| **Query relevance** | How well does this item answer *this* query? | 0–1, relative | `Result.query_relevance` + `rank_version` |

None of them is a probability. All three are stated as such wherever they are displayed.

---

## 1. Semantic similarity

Cosine similarity between embedding vectors, computed by Qdrant. It is what it is: a measure
of how alike two passages read.

The critical property: **similarity is not evidence of a factual relationship.** Two incident
reports about the same warehouse, written by the same team in the same house style, will be
highly similar and describe entirely different events. The demonstration dataset contains
exactly that pair.

Relata therefore:

- reports similarity as its own field, never folded into another score;
- rebases it before it becomes a relationship signal (see below);
- uses `similar_to` as the relationship type when similarity is all there is.

---

## 2. Relationship strength

**Version: `relationship-score-v1`** (constant `relationships.ScoreVersion`)

A relationship's strength is the sum of weighted contributions from named signals, capped
at 1.00:

```
score = min(1, Σ (signal.strength × signal.weight × diminishing_factor))
```

Every signal is stored on the relationship, so the arithmetic is always inspectable:

```json
{
  "score": 0.72,
  "score_version": "relationship-score-v1",
  "signals": [
    {
      "type": "semantic_similarity",
      "value": "0.89 cosine",
      "strength": 0.76,
      "weight": 0.30,
      "detail": "How alike the wording and subject matter are. Similar wording is not evidence that two items describe the same thing."
    },
    {
      "type": "shared_entity",
      "value": "Example Ltd",
      "strength": 1.00,
      "weight": 0.25,
      "detail": "Both items refer to this organisation."
    },
    {
      "type": "claim_contradiction",
      "value": "cost",
      "strength": 1.00,
      "weight": 0.22,
      "detail": "The sources give different figures for cost: 42000 and 51500. Relata does not decide which is correct."
    }
  ]
}
```

### Signal weights

| Signal | Weight | What it means |
|---|---:|---|
| `content_hash_match` | 0.45 | Byte-identical content |
| `semantic_similarity` | 0.30 | Rebased cosine similarity |
| `shared_entity` | 0.25 | An exact entity match, discounted by corpus frequency |
| `claim_support` | 0.22 | Two claims agree about the same subject |
| `claim_contradiction` | 0.22 | Two claims disagree about the same subject |
| `model_classification` | 0.15 | A language model's opinion — one signal, never the score |
| `shared_concept` | 0.12 | Overlapping concept vocabulary |
| `alias_entity` | 0.10 | A match through an alias a person **confirmed**. Unconfirmed candidates contribute nothing. |
| `temporal_proximity` | 0.10 | Created close together in time |
| `shared_metadata` | 0.08 | Matching user-supplied metadata |
| `user_confirmation` | **1.00** | A person confirmed it |
| `user_rejection` | **−1.00** | A person rejected it |

### Why these weights

The weights encode three deliberate judgements.

**Similarity alone cannot look like evidence.** Cosine similarity between unrelated documents
from one corpus routinely sits at 0.5–0.7, so raw cosine carries almost no information at the
low end. The similarity signal rebases against a floor of 0.55:

```
strength = max(0, (cosine − 0.55) / (1 − 0.55))
```

A perfect 0.99 similarity therefore yields `0.98 × 0.30 ≈ 0.29` — below the default storage
threshold of 0.25 only marginally, and far below anything that reads as strong. A pair of
documents that are merely alike will be recorded as `similar_to` at a weak score, which is
the honest description.

**A model cannot manufacture a relationship.** The model signal's weight is 0.15, so even a
maximally confident classification contributes 0.15 — below the 0.25 storage threshold. A
model opinion can *shape* a relationship that other evidence already supports; it cannot
create one. There is a test asserting this.

**Human decisions dominate.** Confirmation adds 1.00, saturating the score. Rejection
subtracts 1.00, driving it to zero. Neither deletes anything: the original proposal, its
provenance and its signals are all retained.

### Diminishing returns

Repeated signals of one type contribute with a factor of 1.0, 0.5, 0.25, then nothing:

```
shared_entity × 6  →  0.25 + 0.125 + 0.0625  =  0.44
```

Breadth of evidence beats repetition. A document mentioning one company twelve times should
not outrank one that relates on several independent grounds.

### Entity frequency discount

An entity appearing in most documents says little about any particular pair:

```
strength ×= 1 / (1 + ln(corpus_frequency))
```

Concepts and subjects start at 0.5 rather than 1.0, because they overlap by nature.

### Relationship type inference

Deterministic evidence outranks a model opinion:

1. `content_hash_match` → `derived_from`
2. `claim_contradiction` → `contradicts`
3. `claim_support` → `supports`
4. the model's type, **except** `same_entity_as`, `supersedes` and `derived_from`, which are
   never established on a model's word alone
5. shared entity plus differing dates → `occurred_before` / `occurred_after`
6. shared entity → `mentions`
7. otherwise → `similar_to`

Chronology yields only a chronological statement. Relata never infers `possibly_explains`,
`updates` or `supersedes` from ordering alone — that would be inferring causation from
sequence, which is precisely the mistake the design forbids.

### Thresholds

| Setting | Default | Effect |
|---|---|---|
| `RELATA_REL_MIN_SCORE` | 0.18 | Below this, a candidate is discarded before any model call |
| `RELATA_REL_STORE_SCORE` | 0.25 | At or above this, stored as **pending review** |
| `RELATA_NEAR_DUPLICATE_SIMILARITY` | 0.94 | Flagged as a likely near-duplicate — never merged |

Nothing is ever auto-confirmed. Every discovered relationship arrives as a proposal.

---

## 3. Query relevance

**Version: `query-rank-v1`** (constant `ranking.RankVersion`)

### Retrieval paths

A query runs through several independent paths, each producing its own ordering:

| Path | Weight | Source |
|---|---:|---|
| Lexical (items) | 1.00 | MongoDB `$text` index over title and extracted text |
| Lexical (chunks) | 1.00 | MongoDB `$text` index over chunk text |
| Vector | 1.00 | Qdrant cosine search, workspace-filtered |
| Entity | 0.60 | Query n-grams matched against normalised entity keys |
| Title | 0.50 | Substring match on the item title |
| Relationship expansion | 0.35 | Items linked to a strong match |
| Confirmed relationship | 0.25 | Bonus when a person confirmed the link |
| Recency | 0.15 | Exponential decay, 180-day half-life |

### Fusion by rank, not by score

MongoDB text scores and cosine similarities are not on comparable scales — adding them
directly would let whichever happens to have the larger range dominate. Relata fuses by
**reciprocal rank**:

```
contribution = weight × 1 / (60 + rank)
```

Rank position is comparable by construction. Paths without a meaningful internal ordering
(entity match, title match, expansion) contribute a scaled value pitched at roughly a
mid-ranked hit, so they augment rather than swamp the ranked paths.

The final score is normalised against the top result, so the best result in any response is
1.00. **This makes relevance comparable within one result set, not across queries.**

### Relationship expansion

After direct matching, Relata pulls in items connected to the strongest matches:

- only from the top `RELATA_SEARCH_MAX_EXPANDED_ITEMS` seeds;
- only relationships at or above `RELATA_SEARCH_EXPANSION_MIN_SCORE` (default 0.45);
- **never** through a relationship a person has rejected;
- optionally only through confirmed relationships (`confirmed_only=true`).

An expanded result is labelled in the interface and in the API:

> Not a direct match. Reached because this item **updates** a matching item — strength 0.68
> (suggested by analysis, not yet reviewed).

### Why each result matched

Every result carries a `why_matched` list and the raw `contributions`:

```json
{
  "query_relevance": 0.83,
  "semantic_similarity": 0.89,
  "why_matched": [
    "A section of this item is close in meaning to your query (similarity 0.89).",
    "The item's text contains words from your query.",
    "This item refers to Example Ltd (organisation), which your query names.",
    "Created 12 days ago."
  ]
}
```

---

## Answer generation

"Answer from stored knowledge" retrieves, then generates strictly from what was retrieved.

**Guarantees that hold regardless of what the model returns:**

1. The model sees only retrieved passages, inside delimited untrusted blocks.
2. Passages are labelled `S1`, `S2`… — **item IDs are never shown to the model**, so a
   fabricated citation cannot resolve to a real record. Citations naming an unsupplied
   reference are dropped and counted as invalid output.
3. An answer claiming sufficiency while citing nothing is downgraded to insufficient.
4. Contradictions Relata has recorded between the retrieved items are surfaced
   **independently of the model's own account**, so a model that quietly picked a side
   cannot hide the disagreement.
5. Inferred relationships that influenced retrieval are listed as proposals, not facts.
6. If a passage tried to instruct the model, that is reported rather than hidden.
7. **A model that returns nothing usable does not cost you the retrieval.** Output rejected as
   invalid — including an answer that echoes the schema back (`{"answer": "sufficient"}`) —
   produces no prose, but the sources and any recorded contradictions between them are still
   returned, and the rejection reason is stated. Retrieval and contradiction detection are
   deterministic; neither depends on the model behaving. A transport failure (timeout,
   unreachable provider) is different and is reported as an error, because that is an
   operational condition rather than a bad answer.

Every answer has a **"Sources used"** section, and an **"Uncertainty and conflicts"** section
whenever conflicts, insufficiency, inferred-relationship reliance or an injection attempt
applies.

---

## Changing the scoring model

Weights live in `relationships.Weights`; retrieval weights in `ranking.Weights`.

If you change either:

1. **Bump the version constant.** `relationship-score-v1` → `-v2`. Every stored relationship
   records the version that produced its score, so mixed generations remain distinguishable.
2. Existing relationships are **not** rescored automatically. Reprocess the affected items to
   recompute them; the dedupe key means this updates rather than duplicates.
3. Update the weights table above and the settings page, which reads the live map.
4. Re-run `internal/relationships/scoring_test.go`. Several tests encode the design
   invariants — similarity alone staying weak, a model opinion never reaching the storage
   threshold, human decisions dominating — and will fail if a change breaks them.
