# Security Audit Report

## 1. Executive Summary

Relata has a generally strong security baseline: workspace scoping is pervasive, authentication uses Argon2id and constant-time comparisons, untrusted bodies and document parsing are bounded, templates escape output, CSRF and CSP controls are present, and the production application container is non-root, capability-free, and read-only. This audit confirmed one high-risk credential disclosure path and four medium/low issues; all five were fixed with regression tests. No critical issue, remote-code-execution path, authorization bypass, hard-coded production credential, or reachable vulnerable Go dependency was found.

Residual risk is concentrated in authenticated resource abuse, floating container tags and vendored frontend provenance, and unauthenticated plaintext datastore traffic inside the private Compose network. Those items require operational or architectural decisions beyond a safe local patch.

## 2. Scope Reviewed

Reviewed the Go application under `cmd/` and `internal/`, HTML templates and JavaScript under `internal/web/`, unit and integration tests, `go.mod`/`go.sum`, Dockerfile and Compose definitions, deployment documentation, migrations, backup scripts, and environment examples. Entry points included browser sessions, OAuth/OIDC, API bearer tokens, uploads, search/answer endpoints, background jobs, MongoDB, Qdrant, and Ollama. The real `.env` contents and `/etc/relata/relata.env` were deliberately not read.

## 3. Methodology

The review combined manual data-flow and authorization tracing, adversarial attack-surface mapping, configuration and dependency review, cryptography and secret-lifecycle analysis, panic/error-path inspection, memory and DoS analysis, frontend event review, and negative-test review. Verification used `make check`, `make test-security`, focused regressions, `go mod verify`, `govulncheck`, shell/JavaScript syntax checks, and Docker Compose validation. Malformed inputs were assessed with bounded tests; no destructive load testing or live exploit against production was performed.

## 4. Risk Rating Method

Severity combines exploitability, required privilege, affected confidentiality/integrity/availability, and blast radius. Confidence reflects evidence quality and runtime confirmation. `Fixed` means the remediation and a proportionate regression test passed during this audit; `Open` means a decision or further work remains.

## 5. Attack Surface Map

- **Internet/browser boundary:** nginx is expected to terminate TLS and proxy to the loopback-only Go HTTP service. Browser routes accept credentials, OAuth/OIDC callbacks, forms, and uploads.
- **API boundary:** `/api/v1/*` accepts session or scoped bearer-token authentication, JSON, filters, pagination, search, and answer requests.
- **Processing boundary:** uploaded PDF, text, HTML, email, and archive-like content flows through parsers, chunking, embeddings, analysis, and leased background jobs.
- **Data boundary:** MongoDB stores identities, sessions, token hashes, documents, jobs, and audit events; Qdrant stores workspace-tagged vectors; upload volumes retain originals.
- **External integrations:** OAuth/OIDC issuers, Qdrant, MongoDB, and Ollama/model responses are separate trust boundaries.
- **Starting positions:** anonymous network client, authenticated member, scoped API-token holder, malicious document author, compromised model/service, compromised database, or local host/container operator.

## 6. Findings Summary Table

| ID | Severity | Confidence | Title | Component | Status |
|---|---|---|---|---|---|
| SEC-001 | High | High | One-time API token disclosed through redirect URL | Web token creation | Fixed |
| SEC-002 | Medium | High | Negative search offset could panic request handling | API/search pagination | Fixed |
| SEC-003 | Medium | High | Stored Argon2 parameters permitted extreme resource requests | Password verification | Fixed |
| SEC-004 | Medium | High | Local secrets and backups inherited permissive file modes | Setup/backup operations | Fixed |
| SEC-005 | Low | High | Panic values could disclose sensitive input in logs or job records | HTTP, jobs, PDF parsing | Fixed |
| SEC-006 | Medium | Medium | Authenticated expensive-work controls are incomplete | Upload/search/job pipeline | Open |
| SEC-007 | Low | High | Deployments are not fully reproducible or provenance-pinned | Containers/frontend vendor asset | Open |
| SEC-008 | Low | Medium | Internal datastores rely primarily on network isolation | MongoDB/Qdrant transport and auth | Open |

## 7. Detailed Findings

### Finding ID: SEC-001

#### Title

One-time API token disclosed through redirect URL

#### Severity

High

#### Confidence

High

#### Affected Files / Components

`internal/web/tokens.go:43-46,97-136`, `internal/web/templates/tokens.html`, route `POST /settings/tokens`.

#### Description

Token creation previously redirected to a URL containing the complete bearer secret in query parameters. URLs commonly enter browser history, reverse-proxy/access logs, monitoring, screenshots, and referrer data. Possession of the secret grants its configured API scopes until revocation or expiry.

#### Evidence

The fixed handler renders the secret only in the `201 Created` POST response and does not emit a `Location` header. `internal/web/tokens_test.go:63-101` verifies both that creation never redirects with a secret and that attacker-supplied `?secret=` input is ignored.

#### Attack Scenario

An administrator creates an automation token. A party with access to browser history or URL logs extracts `rlt_v1_...` and uses it against API routes within the token's scopes.

#### Impact

Unauthorized reading or mutation of workspace data, bounded by token scopes and lifetime.

#### Recommendation

Implemented: keep one-time credentials out of URLs and render them once in the direct authenticated response. Continue redacting authorization headers and token-shaped values in all telemetry.

#### Safer Example

`h.renderTokensPage(w, r, principal, issued.Secret, issued.Token.Name, http.StatusCreated)` with no redirect or query serialization.

#### Suggested Tests

Retain the redirect/query regressions and add a browser test confirming history and referrer data never contain the token.

#### References

CWE-598; OWASP ASVS V9 (communications) and V8 (data protection).

#### Assumptions / Limitations

Existing logs or history may retain tokens created before this fix. Operators should rotate any such token if URL logging was enabled.

### Finding ID: SEC-002

#### Title

Negative search offset could panic request handling

#### Severity

Medium

#### Confidence

High

#### Affected Files / Components

`internal/api/handlers.go:991-1003`, `internal/search/search.go:145-163`, authenticated search endpoint.

#### Description

A negative `offset` survived query parsing and could become a negative slice start during result paging. The recovery middleware converted the panic to HTTP 500, but repeated authenticated requests could consume CPU, generate error traffic, and degrade availability.

#### Evidence

The API now accepts only positive offsets and the service independently clamps negatives to zero. `internal/api/http_test.go:462-470` covers the hostile query.

#### Attack Scenario

A valid user or leaked query-scoped token repeatedly calls search with `offset=-7`, forcing panic recovery on otherwise successful searches.

#### Impact

Request-level denial of service, noisy logs, and possible service degradation under repetition; no memory disclosure was observed.

#### Recommendation

Implemented: normalize at both transport and service boundaries. Apply the same invariant to every future pagination entry point.

#### Safer Example

`offset := q.Offset; if offset < 0 { offset = 0 }` before constructing the response or slicing.

#### Suggested Tests

Retain API and service boundary tests for negative, zero, maximum, and overflow-like pagination values.

#### References

CWE-129; CWE-400; OWASP API Security API4:2023.

#### Assumptions / Limitations

Exploitation requires an authenticated principal because search is not anonymous.

### Finding ID: SEC-003

#### Title

Stored Argon2 parameters permitted extreme resource requests

#### Severity

Medium

#### Confidence

High

#### Affected Files / Components

`internal/auth/password.go:29-44,123-189`, password login against stored PHC hashes.

#### Description

The verifier trusted memory, iteration, parallelism, salt, and output sizes encoded in a stored hash. A database write compromise or corrupt record could request gigabytes of memory or excessive CPU at the next login, extending a database incident into application-host denial of service.

#### Evidence

The decoder now limits the encoded string to 512 bytes, memory to 256 MiB, iterations to 10, parallelism to 16, salt to 16-64 bytes, and key to 16-64 bytes. Malformed-hash tests at `internal/auth/auth_test.go:78-93` include excessive memory, time, and size.

#### Attack Scenario

An attacker with a database write primitive changes a user's PHC parameters, then submits that username to trigger expensive verification.

#### Impact

Memory exhaustion, CPU saturation, or process termination during login.

#### Recommendation

Implemented: treat stored parameters as untrusted instructions and reject values outside an upgrade-compatible policy. Alert on invalid stored hashes.

#### Safer Example

Validate all parsed PHC fields before calling `argon2.IDKey`; never allocate based on unchecked stored values.

#### Suggested Tests

Keep table and fuzz tests; add a metric/assertion that invalid hashes terminate without invoking Argon2.

#### References

CWE-400; OWASP ASVS V2.4.

#### Assumptions / Limitations

The primary prerequisite is database modification or corruption. The selected 256 MiB ceiling remains intentionally expensive and should be considered in host concurrency limits.

### Finding ID: SEC-004

#### Title

Local secrets and backups inherited permissive file modes

#### Severity

Medium

#### Confidence

High

#### Affected Files / Components

`Makefile:40-49`, `scripts/backup.sh:16-30`, `.gitignore`, `.dockerignore`, local `.env`.

#### Description

Setup and backup creation previously inherited the caller's umask. On a common `022` configuration, `.env` and complete knowledge-store archives could be group/world readable. Backup output may include documents and personal data.

#### Evidence

Setup now applies mode `0600`; backup execution sets `umask 077`; backup directories/files were dynamically verified as `0700`/`0600` under caller umask `022`; generated backups are excluded from Git and Docker build contexts. The existing `.env` was changed from `0644` to `0600` without reading its contents.

#### Attack Scenario

Another local account or process reads a newly generated environment file or backup archive from a shared host.

#### Impact

Credential disclosure and bulk compromise of stored documents and metadata.

#### Recommendation

Implemented for file modes. Additionally encrypt backups before off-host transfer, restrict backup service identities, define retention, and test restoration.

#### Safer Example

Run `umask 077` before `mkdir`, `mongodump`, and archive creation; explicitly `chmod 600 .env` after copying the example.

#### Suggested Tests

Add a shell smoke test under umask `022` that asserts exact output modes and a restore drill that checks encrypted archive integrity.

#### References

CWE-732; CWE-312; OWASP ASVS V8.

#### Assumptions / Limitations

POSIX modes do not protect against root, the same account, host compromise, snapshots, or unencrypted off-host copies.

### Finding ID: SEC-005

#### Title

Panic values could disclose sensitive input in logs or job records

#### Severity

Low

#### Confidence

High

#### Affected Files / Components

`internal/api/middleware.go:155-169`, `internal/jobs/worker.go:258-268`, `internal/parsing/pdf.go:30-39`.

#### Description

Recovered panic values were logged or converted to persistent job errors. Panic strings from application or third-party parsing code can include credentials, document fragments, or attacker-controlled content.

#### Evidence

Recovery now records only stable context and generic errors. `internal/api/http_test.go:441-459` and `internal/jobs/worker_test.go:269-279` assert sentinel secrets do not reach responses, logs, or persistent errors.

#### Attack Scenario

A malformed document triggers a library panic whose value contains document text; the old worker path stored it with the failed job and emitted it to logs.

#### Impact

Secondary disclosure to log readers, support tooling, and job viewers.

#### Recommendation

Implemented: log correlation identifiers and stable error classes, never raw panic objects or sensitive payloads. Preserve stack traces only in a separately protected diagnostic channel if operationally necessary.

#### Safer Example

Return `errors.New("job handler panicked")` and log request path/method plus an existing request ID.

#### Suggested Tests

Retain sentinel-secret tests and add structured-log redaction checks for authorization headers and model/parser errors.

#### References

CWE-532; OWASP ASVS V7.

#### Assumptions / Limitations

This does not prove every ordinary non-panic error is free of sensitive third-party text; logging policy should remain conservative.

### Finding ID: SEC-006

#### Title

Authenticated expensive-work controls are incomplete

#### Severity

Medium

#### Confidence

Medium

#### Affected Files / Components

Upload, search/answer, embedding/analysis, and background-job paths under `internal/api/`, `internal/ingest/`, `internal/search/`, and `internal/jobs/`.

#### Description

Individual request sizes, parser work, retries, and worker counts are bounded, and API tokens have rate controls. However, there is no clearly enforced global or per-workspace budget spanning session-driven uploads, queued jobs, embeddings, and model-backed queries. A valid member can generate sustained expensive work within ordinary request limits.

#### Evidence

The code has useful local ceilings and bounded workers, but no unified admission-control counter or workspace quota was identified across the end-to-end pipeline. The threat model already identifies authenticated queue exhaustion as a limitation.

#### Attack Scenario

A compromised member session submits many maximum-allowed documents and model-backed queries, keeping workers and Ollama busy and delaying other tenants.

#### Impact

CPU, memory, disk, model capacity, and queue exhaustion; degraded availability and increased operating cost.

#### Recommendation

Add per-user/workspace concurrent-job limits, queue-depth ceilings, token-bucket admission controls, storage quotas, and operator alerts. Return `429`/`503` with retry guidance before accepting work that cannot be serviced safely.

#### Safer Example

Atomically reserve a workspace work unit before persisting a job; reject when `active + queued >= configured limit`, and release the reservation on every terminal path.

#### Suggested Tests

Add concurrent admission tests, queue saturation tests, cancellation/lease recovery tests, and bounded load tests using disposable stores.

#### References

CWE-400; OWASP API Security API4:2023.

#### Assumptions / Limitations

Practical severity depends on tenant trust, model capacity, reverse-proxy limits, and production rate controls not present in this repository.

### Finding ID: SEC-007

#### Title

Deployments are not fully reproducible or provenance-pinned

#### Severity

Low

#### Confidence

High

#### Affected Files / Components

`docker-compose.yml`, `deploy/docker-compose.prod.yml:21-65`, `internal/web/static/vendor/htmx.min.js:1`.

#### Description

MongoDB uses the mutable major tag `mongo:7`, development Ollama uses `latest`, and the application defaults to `relata:latest`; no image digests are recorded. Vendored HTMX identifies as 2.0.4 while the current upstream stable release observed during the audit was 2.0.9, and no checksum/provenance manifest documents the copied artifact.

#### Evidence

Compose files contain the mutable tags above. `govulncheck` found no reachable Go vulnerability; it reported only GO-2026-5932 in the unimported `x/crypto/openpgp` module package. `go mod verify` passed. The [official HTMX releases](https://github.com/bigskysoftware/htmx/releases) identify 2.0.9 as current; no upstream advisory was found, so the version gap is not classified as a known exploitable vulnerability.

#### Attack Scenario

A rebuild at a later date resolves a different image under the same tag, or an unreviewed vendor replacement enters the tree without a verifiable source hash.

#### Impact

Reduced reproducibility, harder incident response, and increased supply-chain uncertainty.

#### Recommendation

Pin production images by digest, automate controlled update PRs, generate an SBOM, record HTMX source/version/checksum, and update the asset after compatibility testing.

#### Safer Example

Use `image: mongo:7@sha256:<reviewed-digest>` and store a vendor manifest containing upstream URL, release tag, SHA-256, and license.

#### Suggested Tests

Add CI checks rejecting production `latest`/unpinned images and verifying vendored asset checksums and licenses.

#### References

CWE-1104; OWASP Top 10 A06:2021; SLSA provenance guidance.

#### Assumptions / Limitations

Registry attestations and external deployment pinning were not available for inspection. “Current” upstream version was checked on 2026-08-10.

### Finding ID: SEC-008

#### Title

Internal datastores rely primarily on network isolation

#### Severity

Low

#### Confidence

Medium

#### Affected Files / Components

`deploy/docker-compose.prod.yml:21-57,78-87`, MongoDB and Qdrant connections.

#### Description

Production does not publish datastore ports, which is a strong boundary, but the application connects without datastore credentials and over plaintext HTTP/MongoDB transport within the Compose network. A compromised peer container or host-level network foothold could access or observe the stores directly.

#### Evidence

The production URI is `mongodb://mongodb:27017` and Qdrant URL is `http://qdrant:6333`; neither service defines authentication. Ports remain private to the Compose network and the services use `no-new-privileges`.

#### Attack Scenario

An attacker compromises the application or another container attached to the same network and directly queries MongoDB/Qdrant, bypassing application authorization and audit logic.

#### Impact

Bulk disclosure or modification of workspace data and vectors after an existing network/container compromise.

#### Recommendation

Enable service authentication and least-privilege credentials; split networks where practical; evaluate TLS/mTLS for cross-host or higher-assurance deployments; rotate credentials through an external secret manager.

#### Safer Example

Create a dedicated MongoDB application user scoped to the Relata database and configure a Qdrant API key via the protected production env file.

#### Suggested Tests

Add deployment smoke tests proving anonymous datastore access fails and the app still passes readiness and integration checks with credentials enabled.

#### References

CWE-306; CWE-319; OWASP ASVS V9.

#### Assumptions / Limitations

The current single-host private network materially lowers likelihood. Exact support and operational costs for datastore TLS/auth must be validated against the deployed versions.

## 8. Blackhat-Style Abuse Paths

1. **Historical token leakage to API access:** obtain a pre-fix secret from history/logs, call scoped API routes, then use read results to target higher-value data. Breakpoints are token rotation, short TTLs, scope minimization, hashed-at-rest token storage, and the fixed no-URL creation response.
2. **Member session to capacity exhaustion:** compromise a member account, enqueue allowed uploads, and issue model-backed queries until jobs/model capacity saturate. Existing size/worker bounds limit each operation; workspace/global admission control would break the chain earlier.
3. **Container foothold to datastore bypass:** compromise a network peer, connect directly to unauthenticated stores, and bypass workspace checks. Private networks reduce initial reachability; datastore credentials and network segmentation reduce post-compromise blast radius.

## 9. Denial-of-Service and Resource Exhaustion Review

Request bodies, password length, document bytes/pages, result limits, retry counts, worker concurrency, model responses, and login limiter retention are bounded. Negative search paging and attacker-influenced PHC allocation were fixed. Go code contains no obvious user-controlled unbounded goroutine creation. Remaining risk is aggregate authenticated workload across uploads, embeddings, analysis, and jobs (SEC-006). Safe test execution also exposed that concurrent integration suites can exhaust Qdrant file resources; the complete security suite passes sequentially in 508 seconds.

## 10. Cryptography and Key Management Review

Passwords use Argon2id at 64 MiB and three iterations with random 16-byte salts; comparisons are constant-time. Sessions and high-entropy API tokens are stored as hashes, and randomness comes from `crypto/rand`. OIDC verifies supported RSA signatures, issuer, audience, expiry, issued-at time, nonce, and minimum RSA key size; redirect following is disabled for discovery/key requests. No custom cipher or predictable secret generator was found. TLS termination and certificate policy live outside this repository and require manual verification. Post-quantum migration is not urgent for password hashing or random bearer tokens; operators should track interoperable hybrid TLS/OIDC support and crypto-agility rather than deploy bespoke algorithms.

## 11. Secrets Lifecycle and In-Memory Credential Review

No real secret was found in tracked examples or source. `.env` is ignored and now mode `0600`; production secrets are documented outside the repository. Tokens are displayed once and stored hashed, while OAuth/OIDC and datastore credentials enter via configuration. Go strings cannot be reliably zeroized, so passwords/tokens may persist until garbage collection; no `pprof` or debug dump endpoint was found. Panic redaction was strengthened, but host swap, core dumps, observability exports, and proxy/header redaction need operational verification. Prefer short-lived credentials and a secret manager where feasible.

## 12. Memory and Resource Safety Review

The production build disables CGO and no `unsafe`/native FFI surface was identified, substantially reducing classic memory-corruption risk. The pure-Go PDF dependency handles hostile structured input; byte/page caps and generic panic recovery contain failures but do not replace upstream maintenance and fuzzing. Collections, queues, retry loops, and parser/model outputs have local bounds. Aggregate workload remains the principal resource risk.

## 13. Dependency and Supply-Chain Review

`go mod verify` passed. `govulncheck -show verbose ./...` found no reachable symbol or package vulnerability; GO-2026-5932 affects an unimported `x/crypto/openpgp` package only. Available non-security updates include several direct/indirect modules and should be applied through reviewed, tested update PRs. Production image digests, SBOM/attestation generation, and vendored HTMX provenance are absent (SEC-007). The Docker build is multi-stage, CGO-disabled, and runs from a minimal non-root image.

## 14. Authentication and Authorisation Review

Local login, OAuth/OIDC, sessions, scoped API tokens, role checks, workspace identifiers, and audit events were traced. Login has rate limiting and anti-enumeration behavior; session cookies and CSRF handling are appropriately constrained in code. Repository queries consistently accept a principal/workspace filter in reviewed paths. No horizontal/vertical authorization bypass was confirmed. Production proxy correctness, OAuth provider settings, session revocation operations, and pre-fix token rotation require operator validation.

## 15. Input Validation and Output Encoding Review

JSON bodies and uploads are size-limited, identifiers and enums are validated, pagination/results are normalized, SQL is not used, Mongo filters are constructed rather than directly decoded from clients, and HTML templates use contextual escaping. PDF/page work is capped and model outputs are schema-checked with deterministic fallback. The negative search offset defect was fixed. Continue fuzzing parsers and filter normalization, especially after dependency updates.

## 16. Frontend HTML/JavaScript Robustness Review

Templates use escaped rendering and CSP/CSRF integration; no application `innerHTML` sink or credential-bearing debug log was identified. The clipboard handler previously allowed rejected promises and weak fallback behavior. It now handles missing sources, unavailable Clipboard API, and rejection while avoiding logging the token or full error object. A real-browser test is still needed for Clipboard API, CSP, HTMX navigation, focus, and accessible status announcements.

## 17. Infrastructure and Deployment Review

The app binds to loopback at the host, has a read-only filesystem, drops all capabilities, sets `no-new-privileges`, uses bounded tmpfs and rotated logs, and stores data in named volumes. MongoDB/Qdrant are not published in production. `docker compose ... config --no-env-resolution --quiet` passed. Residual issues are image pinning, datastore defense in depth, lack of visible container CPU/memory/PID limits, backup encryption/retention, and external nginx/systemd/TLS controls that were not inspectable.

## 18. Logging, Monitoring, and Error Handling Review

Client errors are stable and avoid internal details; request IDs and audit records support correlation. Panic values are now excluded from HTTP logs, stored job errors, and PDF errors. JSON log rotation is configured. No evidence of alert thresholds for login abuse, queue saturation, invalid stored hashes, datastore auth failures, or token misuse was found; add metrics and alerts without high-cardinality or sensitive labels.

## 19. Test Coverage and Security Regression Gaps

New regressions cover URL-secret leakage, reflected secret queries, negative offset normalization, hostile PHC parameters, panic-secret redaction, and clipboard failure syntax. `make check` passed vet, staticcheck, and the race-enabled full unit suite. `make test-security` passed its unit and end-to-end security suites. Gaps remain in browser interaction testing, authenticated load/quotas, datastore-auth deployment testing, backup restore/encryption drills, image/provenance policy, and fuzzing beyond existing seeds. Integration suites share services and should not be run concurrently; stale test collections caused one environmental Qdrant failure before cleanup and a sequential pass.

## 20. Prioritised Remediation Plan

- **Immediate:** rotate any token that may have appeared in pre-fix URL logs/history; deploy the five fixed controls; protect and encrypt new backups.
- **Short term:** implement workspace/user admission limits and queue/storage quotas; pin production images; update and checksum HTMX; alert on invalid hashes and saturation.
- **Medium term:** enable MongoDB/Qdrant authentication, assess internal TLS/network segmentation, automate SBOM/vulnerability/provenance checks, and add browser/security load tests.
- **Long term:** perform regular restore drills, external proxy/TLS reviews, tenant-isolation tests, and crypto-agility reviews as interoperable post-quantum standards mature.

## 21. False Positives / Needs Manual Verification

- TLS, HSTS, proxy header trust, request limits, and access-log redaction at nginx/systemd were outside the repository.
- `/etc/relata/relata.env` was permission-protected and intentionally not read; its mode/content and secret rotation require operator verification.
- Backup encryption, retention, off-host ACLs, and restore viability were not observable.
- Live OAuth/OIDC provider configuration and redirect registration were not exercised.
- Registry digests/attestations, production runtime resource limits, host firewalling, swap, core-dump, and log-export policies require deployment inspection.
- No advisory was found for vendored HTMX 2.0.4, but provenance and compatibility with 2.0.9 still need validation.

## 22. Final Notes

All safe, confirmed code and local-file-permission fixes were implemented and tested. The remaining findings are defense-in-depth or aggregate-abuse concerns, not evidence of an anonymous direct compromise. Reassess after deployment configuration is available and whenever auth, parser/model, tenancy, or infrastructure boundaries change.
