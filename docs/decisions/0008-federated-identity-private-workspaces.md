# 8. Federated identity, keyed on the subject, with a private workspace each

**Status:** accepted

## Context

Relata needed to sit behind an organisation's identity provider, with a public description of
the product in front of it. Two questions had to be answered before any code: what identifies
an account, and what does a new person get.

## Decision

OAuth 2.0 Authorization Code with PKCE, upgraded to OpenID Connect when the provider offers an
issuer. Accounts are keyed on `(provider, subject)`. Each person who signs in gets a **private
workspace of their own**, in which they are admin.

Local password login remains, disabled whenever an identity provider is configured, as a way
back in when that provider is misconfigured or unreachable.

## Rationale

### The subject, never the email

`sub` is the only identifier a provider undertakes not to reassign. Email addresses are
recycled constantly — a person leaves, their address is given to someone new — and an account
keyed on one hands the new holder the previous holder's documents. That is not a subtle
failure: it is a silent, total disclosure of somebody's corpus to a stranger, and it looks like
a successful login.

The email is still stored, for display and for the optional domain allowlist. It is never
looked up.

### A private workspace each

Relata already enforces a workspace boundary in every storage query, from the first commit.
Giving each person their own reuses that boundary rather than inventing a second notion of
separation on top of it — and a mechanism exercised by every query is worth far more than one
added for this feature alone.

The cost is real and worth stating rather than discovering: **relationship discovery never
spans users.** Two people who upload documents about the same contract get no link between
them, because as far as Relata is concerned they are separate corpora. The product's central
value — finding connections you did not know about — stops at the workspace edge. Anyone
choosing this model should choose it knowing that.

Making them admin of their own workspace follows from it. There is nobody else in there to
protect anything from, and a person who cannot review the relationships in their own corpus
cannot use the system at all.

### Provisioning must be idempotent

First sign-in creates a workspace and an account. Two clicks, two tabs, or a retried callback
mean two simultaneous first sign-ins, and creating two workspaces for one person would split
their corpus in a way nothing would later reconcile. A unique index on
`(oauth_provider, oauth_subject)` decides it: the loser re-reads the winner's account.

A workspace created by a losing racer is abandoned rather than deleted. Deleting it would race
with the winner, and an empty unreferenced workspace costs nothing.

### Local login stays, and defaults to off

An identity provider is a single point of failure for getting in at all. A wrong client secret,
an expired registration, or an outage would otherwise mean editing the database to recover.

It defaults to on while no provider is configured and off once one is, so a fresh installation
works out of the box and a federated one does not keep a second front door open by accident.
Setting the variable explicitly always wins. A federated account has no password hash and is
refused by the local path explicitly, rather than relying on hash verification to fail on an
empty string — the guarantee should not depend on a parser's behaviour.

## Consequences

**Good:** identity is stable across email changes; isolation reuses a boundary already
enforced everywhere; a broken provider is recoverable without database surgery.

**Bad:** no shared corpora, so cross-user discovery is impossible by construction. No account
linking either — a local account and an OAuth account with the same email are two people with
two workspaces, because merging them is a decision to make deliberately, not to infer from a
matching address.

**Rejected alternative:** one shared workspace with roles mapped from provider groups. It would
make discovery work across everyone's documents, which is where Relata is most useful — but it
means every person who signs in can read everything anyone has uploaded, and group-to-role
mapping is a poor place to discover you got the boundary wrong. If shared workspaces are added
later, claim-based role mapping belongs with them.
