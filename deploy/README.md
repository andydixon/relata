# Deployment — relata.dixon.cx

Relata runs on `server.dixon.cx` as a Docker Compose stack managed by systemd, behind the
existing nginx.

`relata.dixon.cx` is **grey-clouded** in Cloudflare — DNS only, no proxy — so requests reach
this server directly. That was a deliberate change: the zone's managed challenge blocks
non-browser clients, which made API tokens unusable, and Cloudflare's 100-second proxy timeout
would have cut off answer generation. The cost is that the origin address is public.

```
client ──► nginx :443 ──► 127.0.0.1:8477 ──► app container
                                                    ├── mongodb   (compose network only)
                                                    ├── qdrant    (compose network only)
                                                    └── ollama    (host, via bridge proxy)
```

## Files this deployment owns

| Path | What |
|---|---|
| `deploy/docker-compose.prod.yml` | The stack. Committed; contains no secrets. |
| `/etc/relata/relata.env` | Site configuration **and secrets**. Mode 600, root. Not in the repository. |
| `/etc/systemd/system/relata.service` | Brings the stack up at boot. |
| `/etc/systemd/system/relata-ollama-proxy.{socket,service}` | Bridges the host's Ollama onto the Docker network. |
| `/etc/systemd/system/ollama.service.d/keep-alive.conf` | Keeps models resident for 30 minutes. |
| `/etc/nginx/sites-available/relata.dixon.cx.conf` | TLS termination and proxying. |
| `/etc/letsencrypt/live/relata.dixon.cx/` | Certificate, renewed by the existing certbot timer. |

## Everyday operations

```bash
sudo systemctl status relata            # is it up
sudo systemctl restart relata           # restart the stack
sudo systemctl stop relata              # stop it
docker compose -f deploy/docker-compose.prod.yml logs -f app    # follow the app log
curl -s http://127.0.0.1:8477/health/ready | jq .               # dependency status
```

**Note:** `/etc/relata/relata.env` is root-readable only, so `docker compose` must be run as
root (or through systemd). Running it as your own user fails with a permission error on the
env file — that is the file permissions working, not a fault.

## Deploying a new version

```bash
cd /home/andy/src/relata
make check                                    # do not deploy something that does not pass

cd deploy
RELATA_VERSION=1.0.2 sudo -E docker compose -f docker-compose.prod.yml build app
sudo docker tag relata:1.0.2 relata:latest    # systemd deploys the `latest` tag
sudo systemctl restart relata
```

Tagging both a version and `latest` means `latest` is always a tag you can name, so a rollback
is `docker tag relata:1.0.1 relata:latest && sudo systemctl restart relata`.

## Why the pieces are as they are

**MongoDB and Qdrant publish no ports.** They are reachable only over the compose network.
The development compose file publishes them for tooling; production has no reason to, and this
host has no firewall, so anything published is a decision to publish.

**Ollama is the host's, not a container.** The host already runs one with the models pulled; a
second copy would duplicate several gigabytes of weights and, on a CPU-only host, would not be
any faster — the bottleneck is the CPU, not the process boundary. The host binds it to
`127.0.0.1`, which containers cannot reach, so `relata-ollama-proxy.socket` listens on the
Docker bridge address (`172.17.0.1:11434`) and forwards to it.

The obvious alternative — `OLLAMA_HOST=0.0.0.0` — would have been one line, and on a host with
no firewall it publishes an unauthenticated model API to the internet. The socket proxy exposes
it to containers on the bridge and nothing else. That is still slightly wider than Relata alone:
any container on this host can reach Ollama through it.

**Ollama runs as a systemd service, and did not used to.** It was previously an `ollama serve`
process started by hand inside a tmux session, owned by `andy` and orphaned to init. It worked,
but it would not have survived a reboot — and Relata is enabled at boot and depends on it, so a
restart would have brought the site up with no embedding and no analysis.

It is now `ollama.service`, enabled, with the 6.8GB model store moved from `/home/andy/.ollama`
to `/usr/share/ollama/.ollama` where the service's own user can read it. `relata.service`
declares `After=ollama.service`.

**Models stay resident for 30 minutes** (`/etc/systemd/system/ollama.service.d/keep-alive.conf`).
Measured on this host: an answer takes **47s cold and 17s warm**, and almost all of the
difference is loading 5.2GB of `llama3.1:8b`. The default of 5 minutes expired between one
question and the next, so nearly every answer paid the load. The two models total ~5.8GB
against 31GB of RAM.

**The Cloudflare real-IP snippet is deliberately NOT included.** With the hostname grey-clouded,
`$remote_addr` is already the true client. That snippet trusts `CF-Connecting-IP` from any
Cloudflare address — with no Cloudflare proxy actually in front, anyone willing to route a
request through a Cloudflare Worker could then set their own address and give themselves a
fresh login-rate-limit bucket.

**If this hostname is ever put back behind the orange cloud, re-add the include.** Without it
every request would appear to come from a Cloudflare edge node and rate limiting would bucket
the whole internet together.

**nginx adds no security headers.** Relata sets its own CSP, X-Frame-Options,
X-Content-Type-Options and Referrer-Policy on every response. `add_header` in nginx appends
rather than replaces, and two conflicting X-Frame-Options headers are worse than one.

**Two locations have long timeouts.** `GET /search` and `POST /api/v1/search/answer` run a
language model inside the request. Everything else gets 120s.

## Known constraints

### Answer generation is slow, because this host has no GPU

8 cores, 31GB RAM, CPU-only inference. Measured with `llama3.1:8b`:

| | Time |
|---|---|
| Answer, model warm | ~17s |
| Answer, model cold | ~47s |
| Full ingestion pipeline, one short note | ~120s |

Ingestion is asynchronous — jobs run in the background and nobody waits — so 120s is a
non-issue. Answer generation is synchronous and is what a person actually feels.

A second Ollama container would not change any of this: same CPU, same weights, plus several
gigabytes of duplicated model store. What would:

1. A GPU. That is the whole of the gap.
2. `RELATA_ANALYSIS_MODEL=llama3.2:3b` — several times faster, noticeably worse, and the
   safeguards still hold (an uncited answer is downgraded, a schema-echo answer is rejected).
3. `RELATA_SEARCH_ANSWER_MAX_CHUNKS=4` to send the model less material.

### Cloudflare (resolved, but worth knowing)

Both of the following applied while `relata.dixon.cx` was proxied, and are the reason it is now
grey-clouded. They return the moment the orange cloud goes back on:

- **Non-browser clients are challenged.** Requests without a browser get `403` with
  `cf-mitigated: challenge` at the edge. This is zone-wide on `dixon.cx` — `grafana.dixon.cx`
  and `intel.dixon.cx` behave the same way; `blackwall.cx` has an exception. API tokens are
  unusable through it.
- **A 100-second proxy timeout** on non-enterprise plans, which cannot be raised from the
  origin, returns 524 for a slow answer even though nginx and Relata would both have waited.

If you want the proxy back for its other benefits, both are fixable from the Cloudflare
dashboard: a **WAF → Custom rules → Skip** matching
`(http.host eq "relata.dixon.cx" and starts_with(http.request.uri.path, "/api/v1/"))` skipping
*Managed Challenge* and *Bot Fight Mode*, and either keeping answers under 100s (see above) or
accepting that they will be cut off. Re-add `snippets/cloudflare-realip.conf` to the site at the
same time.

### Everything else

- **First sign-in provisions a private workspace.** Each Blackwall identity gets its own, and
  relationship discovery never crosses between them. See
  [ADR 8](../docs/decisions/0008-federated-identity-private-workspaces.md).
- **Local password login is off.** Set `RELATA_LOCAL_LOGIN_ENABLED=true` in
  `/etc/relata/relata.env` and restart to open the break-glass form if Blackwall is ever
  unreachable. `relata createadmin` works regardless.
- **Backups.** `scripts/backup.sh` covers MongoDB and Qdrant. The compose volumes are
  `relata-prod_mongo_data`, `relata-prod_qdrant_data` and `relata-prod_uploads`.
