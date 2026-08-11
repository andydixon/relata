# 1. A modular monolith with in-process workers

**Status:** accepted

## Context

Relata has several distinct concerns — ingestion, parsing, embedding, analysis, search — and
background work that can take tens of seconds per item. That shape invites splitting into
services: an ingestion service, an analysis worker, a search API.

## Decision

One binary, one process. Modules are Go packages with enforced dependency boundaries, and
background workers run in-process against a job queue stored in MongoDB.

## Rationale

The workload does not justify distribution. Relata targets a personal or small-team corpus;
the bottleneck is the language model, and that is not helped by splitting the application
around it.

Distribution would add: service discovery, inter-service authentication, distributed tracing
to answer questions a single stack trace answers now, partial-failure handling on every call
that is currently a function call, and a deployment story requiring an orchestrator.

The boundaries that matter are enforced without processes. `internal/domain` imports only the
standard library; storage and AI adapters implement its ports. That gives the testability and
replaceability people usually go to microservices for — the scoring model is unit-tested with
nothing running — while keeping a stack trace that spans the whole request.

The job queue is the one piece that looks like distributed infrastructure, because it must
survive a crash. It is a leased queue in MongoDB rather than a message broker: Relata already
requires MongoDB, and adding Kafka or RabbitMQ for a queue that peaks at a few hundred jobs
would be infrastructure without a purpose.

## Consequences

**Good:** one process to deploy, monitor and debug. Refactoring across modules is a compiler
problem rather than a migration. Local development needs no orchestrator.

**Bad:** no independent scaling — a heavy analysis load competes with request handling for the
same process, though the worker pool and AI semaphore bound that. No independent deployment.
Horizontal scaling would need work, though the leased queue means multiple instances against
one MongoDB would already cooperate correctly.

**If this needs revisiting:** the boundary to extract first is the worker pool, since it
already communicates only through the queue.
