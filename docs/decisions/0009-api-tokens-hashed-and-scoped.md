# 9. API tokens are hashed, scoped, and bounded by their owner's role

**Status:** accepted

## Context

Programs need to ingest and query without a browser session. Sessions are the wrong shape for
that: they expire on idleness, rotate, and are bound to a cookie and a CSRF token.

## Decision

Bearer tokens a person creates for themselves. Only a SHA-256 hash is stored. Each token
carries explicit scopes (`ingest`, `query`, `review`), and its effective permission is the
intersection of those scopes and its owner's **current** role, re-read on every request.

Tokens authenticate `/api/v1` only, and cannot manage tokens.

## Rationale

### Hash, not encrypt

An encrypted token is recoverable by whoever holds the key, which is the same process that
holds the database connection. Hashing means a database read, a backup, a dump or a stolen
snapshot yields nothing that authenticates. The cost — the secret cannot ever be shown again —
is the point rather than a limitation, and the interface says so at the moment of creation.

Authentication is a lookup on a unique index over the whole hash. There is no comparison in
application code for an attacker to time, and no way to probe for a prefix. The 12-character
display prefix is kept in clear so a person can tell their tokens apart; it is far too short to
be a usable fraction of a 32-byte secret.

### Intersection, not assignment

Scopes could have been a fixed grant checked on its own. Making permission the *intersection*
with the owner's live role means one thing that a fixed grant cannot give: **demoting or
disabling a person immediately narrows or kills every token they hold**, without anyone having
to find those tokens. The user record is re-read per request precisely to make that true; a
role cached on the token would mean the change took effect at expiry, which is exactly when it
does not matter.

### Tokens cannot manage tokens

The single most valuable thing a leaked credential can do is make itself permanent. Requiring
an interactive session for creation, listing and revocation means a leaked token has a fixed
expiry it cannot extend, cannot mint replacements, and cannot revoke the tokens that would
replace it.

### The CSRF exemption is about *how*, not *whether*

Bearer requests carry no CSRF token, and should not have to: CSRF defends against a
third-party page spending *ambient* credentials, and an `Authorization` header is not ambient —
a browser never attaches one by itself.

The dangerous way to implement that is to skip CSRF when an `Authorization` header is present,
because then any cookie-authenticated request can disable the check by adding one. So the
exemption keys off how the request actually authenticated, and authentication resolves the
session cookie **first**. A request holding a cookie is never token-authenticated, so the
header cannot switch anything off. This was verified against a running instance, not only
reasoned about.

### Rejected: PATs that never expire

Convenient, and the reason so many leaked credentials stay live for years. Every token has an
expiry, capped by configuration, and stops working with no further action.

### Rejected: tokens in query parameters

A token in a URL ends up in access logs, browser history, referrer headers and screenshots.
Only the `Authorization` header is read.

## Consequences

**Good:** a stolen database yields no usable credential; a role change takes effect instantly
across every token; a leaked token is bounded in scope, in lifetime and in what it can escalate
to.

**Bad:** a lost token cannot be recovered, only revoked and replaced. Scopes are coarse — three
of them, checked per route, with no per-item restriction. Creating a token through the
interface puts its value in the browser's history for one page load, which the page warns
about; the API route returns it in a response body and avoids that entirely.
