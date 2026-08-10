# Authentication and access

Relata has three ways in, for three different callers, and they do not overlap.

| Caller | Mechanism | Gets |
|---|---|---|
| A person, in a browser | OAuth 2.0 Authorization Code + PKCE against Blackwall | A server-side session and a cookie |
| A person, when the identity provider is broken | Local username and password, **disabled by default** | The same session and cookie |
| A program, against `/api/v1` | A bearer token the person created in the interface | A scoped, non-interactive principal |

Everything a signed-out visitor can reach is a static description of the product. No stored
item, entity, relationship, count or workspace name is readable without authenticating.

---

## 1. The public site

`/`, `/how-it-works` and `/api` are served to anyone. They are hand-written pages describing
what Relata does and what it deliberately does not do; they read nothing from the database.

`/` is the only dual-purpose route: signed in, it is the dashboard; signed out, it is the
landing page. Every other application route requires a session and redirects to `/login`.

---

## 2. Signing in through Blackwall

### The endpoint to register

```
https://<your-relata-host>/auth/oauth/callback
```

The path is fixed. Register the same path under whatever hosts you serve, including
`http://127.0.0.1:8080/auth/oauth/callback` for local development.

### The flow

1. `GET /auth/oauth/start` mints a 32-byte `state`, a PKCE verifier and (for OIDC) a `nonce`,
   stores them in a short-lived, `HttpOnly`, `SameSite=Lax` cookie scoped to `/auth/oauth`,
   and redirects to the provider's authorization URL with `code_challenge_method=S256`.
2. The person authenticates with Blackwall.
3. `GET /auth/oauth/callback` compares `state` against the cookie **in constant time**, clears
   the cookie so the value is single-use, exchanges the code and PKCE verifier for tokens over
   a timeout-bounded HTTP client, and establishes the identity.
4. Identity comes from the ID token when the provider is OIDC — signature verified against
   cached JWKS, with `iss`, `aud`, `exp`, `iat` and `nonce` all checked — and otherwise from
   the userinfo endpoint.
5. The user record is found or created, a **new** session ID is issued, and the browser lands
   on the dashboard.

### Identity is the subject, never the email

Users are keyed on `(oauth_provider, oauth_subject)` with a unique index. The `sub` claim is
the only immutable identifier a provider offers; email addresses get reassigned, and keying on
one means whoever receives a recycled address inherits the previous holder's documents.

The email is stored for display and for the optional domain allowlist. It is never the key.

### What a new person gets

A **private workspace of their own**, created on first sign-in, in which they are `admin`.
Nobody sees anyone else's documents, entities or relationships — the workspace boundary was
enforced in storage from the first commit, and this reuses it rather than adding a new one.

The consequence is worth stating plainly: **relationship discovery never spans users.** Two
people who upload documents about the same contract get no link between them, because as far
as Relata is concerned they are separate corpora. That is the cost of the isolation.

Provisioning is idempotent. Two concurrent first sign-ins race on the unique subject index;
the loser re-reads the winner's record rather than creating a second workspace.

### Local login

Disabled unless `RELATA_LOCAL_LOGIN_ENABLED=true`. When disabled, the password form is not
rendered and `POST /login` returns 404 — the handler is not registered at all, rather than
registered and refusing, so there is no code path to reach.

Enable it to recover from a broken provider configuration. `relata createadmin` still works
regardless, so the account can be prepared in advance.

A user provisioned through OAuth has no password hash and can never sign in locally, whatever
is submitted.

---

## 3. API bearer tokens

### Creating one

Settings → API tokens. Choose a name, the scopes, and an expiry. **The token is shown once**
and never again; only a SHA-256 hash is stored, so a database read cannot be replayed as a
credential and neither can a backup.

```
rlt_v1_8Kx2mQ… (43 characters of base64url from 32 random bytes)
```

The first 12 characters are stored in clear as a display prefix, so the interface can list
`rlt_v1_8Kx2…` and you can tell which token is which without holding the secret.

### Using one

```bash
curl -H "Authorization: Bearer $RELATA_TOKEN" \
     https://relata.example/api/v1/search?q=delivery+schedule
```

### Scopes

| Scope | Permits |
|---|---|
| `ingest` | Create items, upload files, reprocess, delete |
| `query` | Search, answer generation, and reading stored items, entities and relationships |
| `review` | Confirming, rejecting and retyping relationships; confirming entity aliases |

A token carries the scopes it was given and **never more than the role of the user who created
it**. A viewer's token cannot ingest even if `ingest` was requested, because the check is the
intersection of the two. Demoting a person immediately narrows every token they hold.

Scopes are checked per route. A request outside its scopes gets `403` with
`insufficient_scope` and the scope it needed, not a generic denial.

### What a token cannot do

- **Reach the web interface.** Bearer tokens authenticate `/api/v1` only. A token in a browser
  request to `/items` is ignored.
- **Manage tokens.** Token creation, listing and revocation require a session. A leaked token
  cannot mint more tokens or extend its own life.
- **Outlive its expiry or revocation.** Both are checked on every request against the stored
  record; there is no cached grant.
- **Change the account.** Nothing under `/auth` accepts a bearer token.

### CSRF

Bearer requests do not carry CSRF tokens and are not asked for them: CSRF exists to stop a
third-party page using *ambient* credentials, and an `Authorization` header is not ambient — a
browser never attaches one on its own.

The exemption is bound to how the request authenticated, not to a header being present. A
request holding a session cookie still needs its CSRF token even if it also sends
`Authorization`, so an attacker cannot disable the check by adding a header.

### Rate limiting and audit

Each token has its own rate limit bucket. Creation, use-after-expiry, revocation and every
authentication failure are audit events. The token value never appears in a log, an audit
record, an error message or a metric label.

`last_used_at` is recorded at most once a minute per token, so a busy token does not turn every
read into a write.

---

## 4. Configuration

See `.env.example` for the full list with comments. The minimum for Blackwall:

```bash
RELATA_OAUTH_ENABLED=true
RELATA_OAUTH_PROVIDER_NAME=Blackwall
RELATA_OAUTH_CLIENT_ID=...
RELATA_OAUTH_CLIENT_SECRET=...

# Either discovery:
RELATA_OAUTH_ISSUER=https://www.blackwall.cx
# ...or the endpoints directly:
RELATA_OAUTH_AUTHORIZE_URL=https://www.blackwall.cx/oauth/authorize
RELATA_OAUTH_TOKEN_URL=https://www.blackwall.cx/oauth/token
RELATA_OAUTH_USERINFO_URL=https://www.blackwall.cx/oauth/userinfo

RELATA_OAUTH_SCOPES=openid profile email
RELATA_OAUTH_ALLOWED_EMAIL_DOMAINS=          # optional; empty means any
```

Startup validates this as a set: OAuth enabled with neither an issuer nor a full endpoint
triple is refused, as is a non-HTTPS endpoint outside development, as is disabling both OAuth
and local login — which would leave no way in at all.

---

## 5. Known limitations

- **No refresh tokens.** Relata's session is its own; the provider's access token is used once
  to establish identity and then discarded rather than stored. A revoked Blackwall account
  keeps its Relata session until that session expires (idle 24h, absolute 7 days by default).
  Revoke the Relata session too, from Settings, if that matters.
- **No group or role mapping yet.** Every OAuth user is admin of their own workspace. There is
  nothing to map a provider group onto, because there is no shared workspace to have roles in.
  If shared workspaces are added later, claim-based role mapping belongs with them.
- **No account linking.** A local account and an OAuth account with the same email are two
  separate users with two separate workspaces. Merging them is deliberate manual work, not
  something to infer from a matching address.
- **Token scopes are coarse.** Three scopes, checked per route. There is no per-item or
  per-collection restriction.
- **The provider is trusted for identity.** If Blackwall asserts a subject, Relata believes it.
  That is what delegating authentication means.
- **A newly created token is rendered only in the creation response.** It is never placed in a
  redirect URL, browser history, referrer header or access-log query string. Refreshing or
  navigating away cannot recover it because only its hash is stored.
- **Key rotation that reuses a key ID costs one failed request.** Relata caches the provider's
  signing keys for an hour. If a key is replaced while keeping its `kid`, the first token
  signed by the new key fails signature verification, triggers a single throttled refetch, and
  succeeds on the retry within the same request. A provider that rotates properly — new key,
  new `kid` — never hits this path.
