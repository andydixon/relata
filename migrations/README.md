# Migrations

Relata has **no migration files to run.** Schema management happens in two ways, both
automatic and both idempotent.

## Indexes

Every index is declared in `Store.EnsureIndexes` in
[`internal/storage/mongo/store.go`](../internal/storage/mongo/store.go) and created on every
startup, and by `relata createadmin` and `relata seed`. Creating an index that already exists
is a no-op, so this is safe to run repeatedly and needs no version tracking.

Adding an index means adding it to that function. There is deliberately no separate migration
file: keeping the declaration next to the queries that depend on it is what stops the two
drifting apart.

Four of those indexes are load-bearing rather than merely useful — they are what make the
pipeline idempotent under concurrent workers:

| Index | Guarantees |
|---|---|
| `entities (workspace_id, normalised_key)` unique | One entity per normalised name and type. Concurrent workers extracting the same company converge on one record instead of racing to create two. |
| `relationships (workspace_id, dedupe_key)` unique | One relationship per item pair. Reprocessing updates rather than duplicating, and a human review decision is never overwritten. |
| `jobs (idempotency_key)` unique, partial on `status = "pending"` | At most one *pending* job per key, while completed jobs remain as history. Re-enqueuing after a crash is a no-op. |
| `items (workspace_id, content_hash)` unique, partial on `duplicate_of = "" and deleted = false` | At most one live, non-deliberate item per content hash. Duplicate detection is a read followed by a write, so concurrent uploads of identical content can both pass the read; this index is what actually enforces the rule. Deliberate duplicates (`duplicate_of` set) and tombstoned items fall outside the filter and are unaffected. |

### The one document migration

The content-hash index is partial on `duplicate_of = ""`, and an **absent** field does not
match that filter. Items written before that field was stored unconditionally therefore need a
one-off backfill, which `EnsureIndexes` performs itself before creating the index:

```js
db.items.updateMany({duplicate_of: {$exists: false}}, {$set: {duplicate_of: ""}})
```

It is idempotent and a no-op once applied, and it logs the number of items it touched.

If startup fails with *"the collection already contains more than one live item with the same
content hash"*, the data predates the constraint and already violates it. Find the offenders
and decide which to keep:

```js
db.items.aggregate([
  {$match: {deleted: false, duplicate_of: ""}},
  {$group: {_id: {w: "$workspace_id", h: "$content_hash"}, ids: {$push: "$_id"}, n: {$sum: 1}}},
  {$match: {n: {$gt: 1}}}
])
```

Then point each extra at the one being kept — `db.items.updateOne({_id: extra},
{$set: {duplicate_of: keeper}})` — which records the relationship honestly rather than
deleting evidence, and restart. Nothing is removed automatically: which copy is canonical is
not a decision Relata should make on your behalf.

## Documents

MongoDB is schemaless, and Relata's structs decode missing fields to their zero values. A new
field therefore needs no migration: existing documents read back with the zero value, and the
code treats that as "not set".

Where a new field changes behaviour, the version constants make the difference visible rather
than silent:

- `parsing.Version` — items record the parser that produced their extracted text
- `relationships.ScoreVersion` — relationships record the scoring model behind their score
- `ranking.RankVersion` — search results record the fusion model that ordered them
- `model_versions` — which embedding and analysis models produced stored data

Reprocessing an item recomputes its derived records under the current versions, and retains
the previous processing runs for audit. That is the migration mechanism for derived data:

```bash
# One item
curl -s -b cj.txt -X POST "$BASE/api/v1/items/$ID/reprocess" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"reason":"parser upgraded"}'

# Every vector in the workspace, after an embedding-model change
#   Settings → Re-embed workspace
```

## Qdrant

The collection is created at startup, sized for the configured embedding model, with a payload
index on every filterable field. A dimension mismatch is refused with an error naming the fix,
rather than silently writing vectors that cannot be compared with the existing ones.

See [BACKUP_AND_RESTORE.md](../docs/BACKUP_AND_RESTORE.md) for rebuilding the collection.
