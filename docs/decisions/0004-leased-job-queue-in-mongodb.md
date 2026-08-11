# 4. A leased job queue in MongoDB, not a message broker

**Status:** accepted

## Context

Processing takes tens of seconds per item and must survive restarts and crashes. Options: a
message broker (RabbitMQ, NATS, Kafka), a dedicated job library backed by Redis, or a queue
in the database Relata already has.

## Decision

A leased queue in MongoDB. Claiming a job atomically sets an owner and an expiry; the worker
renews the lease while it runs; an expired lease returns the job to the pending pool.

## Rationale

Relata already requires MongoDB. A broker would add a fourth service to deploy, monitor, back
up and secure, for a queue that peaks at a few hundred jobs.

More importantly, the queue needs properties brokers make awkward:

- **Jobs are inspectable.** The queue page shows every job, its attempts, its last error and a
  retry button. That is a database query, not a management API.
- **Idempotency is enforced by the store.** A partial unique index on `idempotency_key` for
  *pending* jobs means a duplicate enqueue is a no-op while completed jobs remain as history.
- **Failure history is part of the record.** Each attempt appends to the job, so "why did this
  item never process?" is answerable from the job itself.

Leases rather than locks: a lock needs someone to release it, and a crashed worker releases
nothing. A lease expires on its own, so crash recovery needs no external coordination. Losing
a lease also cancels the worker's context, so a worker that was slow rather than dead abandons
its attempt instead of writing a result another worker has superseded.

## Consequences

**Good:** no extra infrastructure. Jobs are queryable, inspectable and retryable through the
ordinary interface. Crash recovery is automatic and testable. Multiple instances against one
MongoDB would already cooperate correctly.

**Bad:** polling rather than push, so there is up to one poll interval of latency. MongoDB
carries the write load of claims and heartbeats. No fan-out or routing — but Relata's pipeline
is a linear chain, so neither is needed.

**Bug worth recording:** the first implementation deadlocked. `execute` waited for the
heartbeat goroutine to exit, while that goroutine only exited when the job context was
cancelled — which happened via `defer` after `execute` returned. Every worker wedged after one
job. It was caught by running the application, not by the unit tests, and
`TestWorkerCompletesJobsAndDoesNotDeadlock` now queues more jobs than there are workers so a
regression fails immediately.
