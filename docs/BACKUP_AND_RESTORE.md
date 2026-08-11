# Backup and restore

## What must be backed up

| Store | Contains | Recoverable without a backup? |
|---|---|---|
| **MongoDB** | Items, extracted text, chunks, entities, claims, relationships, review history, users, audit | **No.** This is the irreplaceable part. |
| **Upload directory** | Original PDF and DOCX bytes | **No.** Only the originals a user supplied. |
| **Qdrant** | Embedding vectors | **Yes** — rebuildable from MongoDB by re-embedding. |
| `.env` | Configuration | Yes, but keep it: it records which models produced your data. |

MongoDB and the upload directory must be backed up **together and consistently**: an item row
referring to a file that is not in the backup is a broken record.

Qdrant is derived data. Backing it up saves re-embedding time; losing it costs only the time
to rebuild.

---

## Backing up

### Quick local snapshot

```bash
./scripts/backup.sh                 # writes to backups/<timestamp>/
./scripts/backup.sh /mnt/backups    # or somewhere else
```

The script captures MongoDB and the upload directory together, requests a Qdrant snapshot if
one is available, copies the non-secret configuration for reference, and writes a `RESTORE.md`
alongside the archives.

It honours `RELATA_UPLOAD_DIR`, `RELATA_MONGO_DATABASE`, `RELATA_QDRANT_URL` and
`RELATA_QDRANT_COLLECTION`, so a non-default deployment needs no edits.

### Consistency

Relata does not use MongoDB transactions across collections, so a dump taken while the workers
are running may catch an item mid-pipeline — for example chunks written but embeddings not yet
stored. That is **recoverable but untidy**: on restore, reprocess any item whose status is not
`ready` or `partial`.

For a clean backup, stop the application first. The workers shut down gracefully: in-flight
jobs are drained, and any that do not finish keep their leases and return to the queue on
restart.

For a hot backup, prefer a filesystem or volume snapshot that captures MongoDB's data
directory and the upload directory at the same instant.

---

## Restoring

```bash
# 1. Stop Relata.
docker compose stop app

# 2. MongoDB
docker compose exec -T mongodb mongorestore \
  --archive --gzip --drop < backups/TIMESTAMP/relata-mongo.archive.gz

# 3. Original files
rm -rf data/uploads
tar -xzf backups/TIMESTAMP/uploads.tar.gz -C data

# 4. Start Relata. Indexes are created automatically at startup.
docker compose start app     # or: make dev
```

### After restoring

Check the counts on the settings page. If **vectors stored** does not match **chunks in
MongoDB**, Qdrant is out of step. Rebuild:

**Settings → Re-embed workspace**, or:

```bash
curl -s -b cj.txt -X POST http://127.0.0.1:8080/settings/reembed \
  -H "X-CSRF-Token: $CSRF"
```

Re-embedding processes in bounded batches, re-enqueuing itself as it goes, so a large
workspace does not hold one lease for hours. Progress is visible on the queue page.

### Restoring without Qdrant data

Entirely supported — vectors are derived.

```bash
# Recreate the collection and rebuild every vector.
curl -X DELETE http://127.0.0.1:6333/collections/relata_chunks
# Relata recreates it at startup, then:
#   Settings → Re-embed workspace
```

Between deletion and completion, meaning-based search returns nothing and search responses say
so in their notes. Keyword, entity and relationship search continue to work.

---

## Verifying a restore

```bash
# Everything reports healthy
curl -s http://127.0.0.1:8080/health/ready | jq '.status, .checks'

# Counts match the source system
docker compose exec -T mongodb mongosh relata --quiet --eval '
  ["items","chunks","entities","claims","relationships","users"].forEach(
    c => print(c.padEnd(16) + db[c].countDocuments({})))'

# Vector count matches the chunk count
curl -s -X POST http://127.0.0.1:6333/collections/relata_chunks/points/count \
  -H 'Content-Type: application/json' -d '{"exact":true}' | jq .result.count

# Any item left mid-pipeline
docker compose exec -T mongodb mongosh relata --quiet --eval '
  db.items.find({status: {$nin: ["ready","partial","tombstoned"]}}, {title:1, status:1}).forEach(printjson)'
```

Reprocess anything the last query returns, from the item page or:

```bash
curl -s -b cj.txt -X POST "http://127.0.0.1:8080/api/v1/items/$ID/reprocess" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"reason":"restored from backup"}'
```

Reprocessing is idempotent: chunks and claims are replaced, entities merge on their normalised
key, and relationships upsert on a dedupe key **without overwriting human review decisions**.

---

## What a backup preserves

Worth knowing, because it is the point of the audit design:

- **Original content**, exactly as supplied.
- **Review history** — every confirmation, rejection and type change, with actor and timestamp,
  append-only.
- **Original model proposals**, retained even where a person overruled them.
- **Processing runs**, recording which parser, embedding model, analysis model and scoring
  version produced each generation of derived data.
- **Audit events** for logins, ingestion, deletion, reprocessing and relationship decisions.

Restoring therefore recovers not just the data but the record of how it came to be as it is.

---

## Retention and disposal

Deleting an item removes its chunks, vectors, claims, orphaned entities, relationships and the
stored original. Audit events and processing runs are **deliberately retained**: they record
that something existed and was removed, without retaining its content.

Backups are not covered by that deletion. If a user asks for their data to be removed, older
backups will still contain it until they age out — plan a retention window accordingly.

---

## Disaster recovery drill

Worth doing once, before you need it:

```bash
# 1. Back up.
./scripts/backup.sh

# 2. Destroy everything.
make reset

# 3. Restore, then create an account if the users collection was not restored.
#    (mongorestore --drop restores users too, so a restore normally includes them.)

# 4. Verify with the checks above, then run a demonstration search.
```
