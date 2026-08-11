# Architectural decision records

One file per decision that would be expensive to reverse or surprising to encounter without
explanation. Each records the context, what was decided, why, and what it costs — including
the alternative that was rejected, since that is usually the part a later reader wants.

| # | Decision | Turns on |
|---|---|---|
| [1](0001-modular-monolith.md) | Modular monolith with in-process workers | One binary and a real dependency rule beat a distributed system for a corpus this size |
| [2](0002-scoring-not-model-percentage.md) | Relationship strength is computed from signals, not asked of a model | A model's percentage cannot be explained, reproduced or debugged |
| [3](0003-conservative-entity-resolution.md) | Entities merge only on an exact normalised key; similar names become candidate aliases | "Example Ltd" and "Example Holdings Ltd" are different companies more often than they are typos |
| [4](0004-leased-job-queue-in-mongodb.md) | A leased job queue in MongoDB rather than a broker | A lease expires by itself; a lock held by a dead worker does not |
| [5](0005-untrusted-model-output.md) | Model output is parsed as untrusted input and never repaired | A regex that fixes malformed JSON is inventing content the model did not produce |
| [6](0006-duplicate-detection-enforced-by-index.md) | Duplicate detection is enforced by a partial unique index, not by a check | A uniqueness rule that a race can defeat is not a rule |
| [7](0007-unusable-model-output-degrades.md) | Unusable model output degrades the answer; it does not fail the request | Retrieval and contradiction detection do not involve the model, so a bad response should not discard them |
| [8](0008-federated-identity-private-workspaces.md) | Federated identity keyed on the provider subject, with a private workspace each | Email addresses get reassigned; keying on one hands a recycled address the previous holder's documents |
| [9](0009-api-tokens-hashed-and-scoped.md) | API tokens are hashed, scoped, and bounded by their owner's live role | A stolen database should yield no usable credential, and demoting someone should narrow every token they hold |

Superseding a decision means adding a record that says so, not editing the old one.
