# Functionality and Next Steps Report

## 1. Executive Summary

Relata is a well-structured Go modular monolith with explicit service/repository boundaries, deterministic fallbacks, useful operational documentation, and broad race-enabled tests. The highest-value next work is to make expensive-work admission explicit, shorten and isolate integration tests, improve browser-level coverage, and make backup/deployment artifacts reproducible. The audit fixes also improved token UX safety, malformed-input behavior, error privacy, file permissions, and clipboard feedback.

## 2. Current Functional Risks

- The security integration suite takes about 8½ minutes and shares MongoDB/Qdrant. Concurrent suite runs exhausted Qdrant file resources and left test collections, making local/CI feedback fragile.
- Rendering a one-time token directly after POST prevents URL disclosure, but refreshing that page can prompt a form resubmission and create a second token. A server-side one-time flash/nonce flow would preserve Post/Redirect/Get without putting credentials in the URL.
- Upload, embedding, analysis, and answer operations have local limits but no unified per-workspace capacity budget; one busy tenant can increase latency for others.
- The backup script creates private files but does not provide encryption, checksums, retention, or an automated restore verification.
- Production operation depends on nginx, systemd, `/etc/relata/relata.env`, and host Ollama behavior that cannot be validated by repository tests alone.

## 3. Code Quality Improvements

Centralize pagination normalization so browser lists, API lists, and search use the same typed parser and invariant tests. Introduce stable public/internal error classes at parser, model, and job boundaries so logs and persisted failures remain useful without carrying raw dependency messages. Split integration harness setup into suite-scoped infrastructure plus per-test logical isolation to reduce repeated collection/index creation. Keep security-sensitive helpers—token display, PHC decoding, principal/workspace filters—small and directly testable.

## 4. Frontend Interaction and Error-Handling Improvements

The clipboard handler now handles absent sources, unavailable API, and rejected writes without logging the bearer value. Add an accessible `aria-live` status element instead of temporarily changing only button text, preserve focus across HTMX swaps, and test keyboard-only token creation/revocation. Add a browser-level assertion that CSRF headers survive HTMX requests and that no token reaches URL, history, referrer, console, or analytics. Update the vendored HTMX asset through a documented checksum-based process.

## 5. Testing Roadmap

1. Add focused service tests for every pagination boundary and hostile PHC value; retain the new panic/token regressions.
2. Add Playwright or equivalent browser tests for login, upload, token lifecycle, clipboard failure/success, HTMX navigation, CSP, and accessibility.
3. Make integration resources suite-scoped or serialize targets with an explicit lock; always remove stale `relata_test_*` stores on controlled startup/cleanup.
4. Add concurrent queue/admission, lease-expiry, cancellation, model timeout, and bounded-load tests.
5. Add deployment smoke tests with authenticated datastores and a backup-create/restore/checksum drill.
6. Run fuzz targets for PHC parsing, PDFs, mail/HTML parsing, filter normalization, and model schema responses in scheduled CI.

## 6. Recommended New Functionality

- **Workspace capacity controls:** configurable queued/active job, upload-byte, stored-byte, and model-request budgets with clear `429`/`503` feedback.
- **One-time credential handoff:** server-side encrypted or in-memory single-use flash storage keyed by a short-lived nonce, allowing safe redirect-after-POST without replaying token creation.
- **Backup lifecycle tooling:** encrypted streaming output, manifest/checksums, retention pruning, Qdrant/Mongo/upload consistency metadata, and a non-destructive restore verifier.
- **Session and token management:** display active sessions, last token use, suspicious-use events, and one-click revocation/rotation while never storing raw tokens.
- **Operational readiness view:** queue depth, oldest job, failed/retried jobs, model/datastore latency, storage use, and dependency health by workspace-safe aggregates.

## 7. Next-Level Opportunities

Generate an SBOM and signed provenance for images, pin production digests, and automate dependency/vendor update pull requests. Add OpenTelemetry-compatible traces with strict attribute allowlists. Consider separate worker deployment and independently scalable queues once workload evidence justifies the operational cost. Improve accessibility through automated axe checks and manual keyboard/screen-reader testing. Maintain a compatibility matrix for MongoDB, Qdrant, Ollama models, and embedding dimensions to make upgrades and restores predictable.

## 8. Prioritised Action Plan

- **Immediate:** ship the audited fixes, rotate historically URL-exposed tokens, document sequential integration execution, and encrypt off-host backups.
- **Short term:** remove test-harness resource churn, add browser tests, implement workspace admission/queue limits, pin images, and record HTMX provenance.
- **Medium term:** add datastore authentication, automated restore drills, SBOM/provenance checks, operational dashboards/alerts, and server-side one-time token flash delivery.
- **Long term:** evaluate split worker scaling, richer session/security management, cross-service tracing, and multi-tenant performance objectives based on observed production demand.

## 9. Caveats and Validation Needed

Priorities assume Relata may host sensitive, multi-user knowledge and that authenticated abuse matters. Validate expected tenant count, upload volume, model capacity, recovery-time/recovery-point objectives, and operator tolerance before choosing quota defaults or new infrastructure. Browser behavior, OAuth providers, nginx/systemd configuration, datastore-auth compatibility, backup encryption keys, and production load all need runtime validation. The repository was almost entirely untracked at audit start, so commits of fixed files record those whole files rather than a conventional baseline diff.
