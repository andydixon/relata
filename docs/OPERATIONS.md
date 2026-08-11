# Operations

Running Relata: what to watch, what the logs mean, and what to do when something is wrong.

---

## Health

| Endpoint | Purpose | Fails when |
|---|---|---|
| `GET /health/live` | Liveness | The process is gone |
| `GET /health/ready` | Readiness | MongoDB or Qdrant is unreachable |

`/health/live` deliberately checks nothing else: a liveness probe that fails during a brief
database blip causes an orchestrator to restart a process that was working perfectly well.

`/health/ready` is unauthenticated, so it names each dependency and whether it is reachable,
but never why. **The reason is in the server log**, at `WARN` with `dependency` and the
request ID:

```bash
grep "readiness check failed" relata.log | tail
```

Returning the driver's error to an unauthenticated caller would publish the database host,
port and — when the connection string carries them — its credentials.

`/health/ready` reports the AI providers but **does not require them**. Ingestion, keyword
search and browsing all work without a model, so taking the service out of rotation because
Ollama restarted would be wrong.

```json
{
  "status": "ready",
  "checks": {
    "mongodb":            {"status": "ok", "required": true},
    "qdrant":             {"status": "ok", "required": true},
    "embedding_provider": {"status": "ok", "required": false},
    "analysis_provider":  {"status": "unavailable", "required": false,
                           "detail": "model \"llama3.1:8b\" is not installed — run: ollama pull llama3.1:8b"}
  }
}
```

---

## Metrics

Prometheus exposition at `GET /metrics`. These describe **the service**, not the stored
knowledge: no item ID, entity name or query string ever becomes a label value.

| Metric | Type | Labels | Watch for |
|---|---|---|---|
| `relata_http_requests_total` | counter | route, method, status | A rise in `5xx` |
| `relata_http_request_duration_seconds` | histogram | route, method | Search latency drift |
| `relata_jobs_processed_total` | counter | type, outcome | `outcome="permanent"` climbing |
| `relata_job_duration_seconds` | histogram | type | Embedding or analysis slowing |
| `relata_jobs_queued` | gauge | status | A backlog that never drains |
| `relata_ai_requests_total` | counter | provider, operation, outcome | Provider errors |
| `relata_ai_request_duration_seconds` | histogram | provider, operation | Model host under strain |
| `relata_storage_operations_total` | counter | store, operation, outcome | Database errors |
| `relata_model_invalid_output_total` | counter | task, reason | A model producing unusable output |
| `relata_auth_attempts_total` | counter | outcome | `bad_password` or `rate_limited` spikes |
| `relata_ingest_items_total` | counter | source_type, outcome | Rejections |
| `relata_search_duration_seconds` | histogram | — | Retrieval latency |

### Alerts worth having

```yaml
- alert: RelataJobsFailing
  expr: rate(relata_jobs_processed_total{outcome=~"permanent|invalid_model_output"}[15m]) > 0.1
  annotations:
    summary: Jobs are failing permanently — check /jobs for the errors

- alert: RelataQueueBacklog
  expr: relata_jobs_queued{status="pending"} > 100
  for: 30m
  annotations:
    summary: The queue is not draining — is the AI provider reachable?

- alert: RelataModelOutputRejected
  expr: rate(relata_model_invalid_output_total[15m]) > 0.05
  annotations:
    summary: The model is producing output that fails validation — consider a larger model

- alert: RelataLoginAttacks
  expr: rate(relata_auth_attempts_total{outcome="rate_limited"}[5m]) > 1
  annotations:
    summary: Sustained failed sign-ins
```

---

## Logs

Structured, via `log/slog`. Set `RELATA_LOG_JSON=true` in production.

Every line carries `request_id` or `job_id`, so a user's report ("I got reference
`3f2504e0-…`") leads straight to the failure.

```json
{"time":"...","level":"INFO","msg":"job completed","job_id":"...","job_type":"extract_entities",
 "item_id":"...","worker":"host-a1b2-w0","duration_ms":4210}
```

**Never logged:** passwords, session tokens, API credentials, complete uploaded documents,
embedding vectors. Content appears as lengths and hashes; user agents as a truncated hash.

Levels: `debug` (per-chunk detail, noisy), `info` (requests, job outcomes — the default),
`warn` (degraded: model unavailable, lease lost, parse warnings), `error` (failures needing
attention).

---

## Common problems

### Items stay "Waiting" and the queue does not move

The workers are not running or cannot reach a dependency.

```bash
grep "background workers started" relata.log   # should appear at startup
curl -s localhost:8080/health/ready | jq .checks
curl -s localhost:11434/api/tags | jq '.models[].name'
```

If `RELATA_WORKERS_ENABLED=false`, nothing will ever process. If the embedding model is
missing, `ollama pull` it — jobs retry and will drain once it is back.

### "model ... is not installed"

```bash
ollama pull nomic-embed-text
ollama pull llama3.1:8b
```

On a CPU-only machine, use `llama3.2:3b` for analysis and set `RELATA_ANALYSIS_MODEL`
accordingly. Expect entity and relationship quality to be noticeably lower with a small model.

### Jobs fail with "invalid model output"

The model is returning JSON that fails schema validation. These are **not retried**, by design.

```bash
curl -s -b cj.txt 'localhost:8080/api/v1/jobs?status=failed' | jq '.jobs[].last_error'
```

Usually a model too small to follow a schema. Use a larger one, or set
`RELATA_ANALYSIS_ENABLED=false` to run the deterministic pipeline only — ingestion, search and
relationships still work.

### Search returns nothing for content that is clearly stored

1. Is the item `ready`? Check the queue page.
2. Is meaning-based search available? Search responses carry a note when it is not.
3. Do the vector and chunk counts match on the settings page? If not, re-embed.
4. Did the embedding model change? Vectors written by a different model are excluded from
   search by design, because comparing across models is meaningless.

### "qdrant collection stores N-dimensional vectors but the model produces M"

The embedding model changed. Either restore the previous model, or:

```bash
# set RELATA_EMBEDDING_DIMENSIONS to M, then
curl -X DELETE http://127.0.0.1:6333/collections/relata_chunks
# restart Relata, then Settings → Re-embed workspace
```

### Answer generation times out

A large model on CPU can exceed the default 120-second analysis timeout when asked to read
several passages. The API returns **504** with code `dependency_timeout` and names the fix.

Measured on a CPU-only machine with `llama3.1:8b`:

| Sources in the prompt | Time |
|---|---|
| 3 | ~33 s |
| 6 | exceeds 120 s |

The shipped defaults already account for this — six passages, a 300-second analysis timeout
and a 360-second write timeout. If you still hit it:

```bash
RELATA_ANSWER_MAX_CHUNKS=3        # fewer passages per answer (cheapest fix)
RELATA_ANALYSIS_TIMEOUT=600s      # allow the model longer
RELATA_HTTP_WRITE_TIMEOUT=660s    # must exceed the analysis timeout
RELATA_ANALYSIS_MODEL=llama3.2:3b # a smaller model
```

`RELATA_HTTP_WRITE_TIMEOUT` must exceed `RELATA_ANALYSIS_TIMEOUT`, or the connection closes
while the model is still working and the user sees nothing at all. Configuration validation
refuses to start on that combination rather than letting it fail silently at request time.

### Everything is slow

Almost always the model host. Check `relata_ai_request_duration_seconds`. A 8B model on CPU
takes seconds per call, and entity plus claim extraction makes several calls per chunk.

Options: a smaller analysis model; lower `RELATA_REL_MAX_MODEL_CALLS`; raise
`RELATA_AI_CONCURRENCY` **only** if the model host has capacity; or disable analysis entirely.

### A worker died mid-job

Nothing to do. The lease expires, `ReleaseExpired` returns the job to the queue, and another
worker picks it up. You will see:

```
WARN returned jobs from expired leases to the queue count=2
```

Jobs out of attempts are marked failed and can be retried from the queue page.

---

## Routine maintenance

**Daily** — check `/health/ready`; check failed jobs on the queue page.

**Weekly** — review relationships awaiting review (the dashboard shows the count); confirming
and rejecting improves search, because confirmed links carry more weight in expansion and
rejected ones stop pulling results in.

**Monthly** — `make security` (includes `govulncheck`); check the settings page for a
vector/chunk mismatch; verify a backup restores.

**On any model change** — reprocess affected items, or re-embed the workspace. Both are
idempotent.

---

## Capacity

Rough figures on a modern laptop, CPU-only, with `llama3.2:3b`:

| Operation | Time |
|---|---|
| Ingest and parse a small note | < 100 ms |
| Chunk and embed one chunk | 0.3–1 s |
| Model entity extraction per chunk | 2–10 s |
| Model claim extraction per chunk | 2–10 s |
| Relationship discovery per item | 5–30 s, depending on model calls |
| Search (no answer generation) | 20–200 ms |
| Answer generation | 5–30 s |

A GPU changes the model figures by an order of magnitude. Deterministic-only mode
(`RELATA_ANALYSIS_ENABLED=false`) processes an item in well under a second.

Storage: roughly 3 KB per chunk in Qdrant at 768 dimensions, plus the original and extracted
text in MongoDB.

---

## Deployment notes

**Keep the loopback binding** unless you have a specific reason not to. If you must expose it:

1. Put it behind a reverse proxy terminating TLS.
2. Set `RELATA_BASE_URL` to the https URL — this enables `Secure` cookies and sets the trusted
   CSRF origin.
3. Set `RELATA_HTTP_ADDR=0.0.0.0:8080` **and** `RELATA_ALLOW_EXTERNAL_BIND=true`; Relata
   refuses to start otherwise, so exposure is always deliberate.
4. Set `RELATA_TRUST_PROXY_HEADERS=true` **only** if the proxy strips client-supplied
   forwarding headers. Otherwise anyone can defeat login rate limiting with a forged header.

**Least-privilege MongoDB:**

```javascript
use relata
db.createUser({
  user: "relata_app",
  pwd: passwordPrompt(),
  roles: [{ role: "readWrite", db: "relata" }]
})
```

Relata creates its own indexes at startup, which `readWrite` permits. It never needs
`dbAdmin` or `clusterAdmin`.

**Container:** the image runs as UID 65532 on a read-only root filesystem with all
capabilities dropped. The upload volume is the only writable path and must be writable by
that UID.

---

## Upgrading

1. Back up (see [BACKUP_AND_RESTORE.md](BACKUP_AND_RESTORE.md)).
2. Read the release notes for changes to the parser, scoring or embedding defaults.
3. Deploy. Index creation is idempotent and runs at startup.
4. If the parser version changed, reprocess items to benefit — old items keep working with
   their existing extraction.
5. If the scoring version changed, existing relationships keep the version that produced them.
   Reprocess to recompute; human review decisions survive.
