# Carteiro — SMTP relay in Go, database-backed and API-managed

Carteiro is a **minimal SMTP relay**: your application (Node.js with
Nodemailer, PHP, Python, etc.) connects to Carteiro on port **587**,
authenticates with an **email + password**, and Carteiro delivers the message
**directly to the recipients' MX servers**. Everything — accounts, DKIM keys
and the **message queue** — lives in a **database** (SQLite by default, MySQL
via DSN), so accounts and domains are added **over the air** through an admin
REST API with bearer tokens, with zero restarts and nothing kept only in
memory.

```
+------------+   AUTH PLAIN/LOGIN   +--------------------------------------+   SMTP delivery (25)
| Nodemailer | ----- 587 ---------► |  Carteiro (relay)                    | -------------------► Gmail/Outlook MX
| your app   |                      |  +- smtpd    (auth via bcrypt in DB) |   opportunistic TLS
+------------+                      |  +- deliverer (queue worker)         |   DKIM from DB
                                    |  +- web panel + admin API :8080     |
+------------+   HTTPS + Bearer     |  +- sqlite / mysql                   |
|  browser / | ----- /api/* --------►  +- React UI served at / (embedded)  |
|    curl    |   accounts, dkim,     |                                     |
+------------+   queue, stats, send  +--------------------------------------+
```

## Table of contents

1. [How it works](#how-it-works)
2. [Configuration](#configuration)
3. [Seeds: first run, upserts and clear logs](#seeds-first-run-upserts-and-clear-logs)
4. [Keys and certificates as base64](#keys-and-certificates-as-base64)
5. [Admin API (bearer) and monitoring](#admin-api-bearer-and-monitoring)
6. [Ports and network](#ports-and-network)
7. [Docker mode](#docker-mode)
8. [Daemon mode (Linux/macOS)](#daemon-mode)
9. [Sending with Nodemailer](#sending-with-nodemailer)
10. [The database queue](#the-database-queue)
11. [DNS: keeping email out of spam](#dns-keeping-email-out-of-spam)
12. [TLS on the submission port](#tls-on-the-submission-port)
13. [Development](#development)
14. [Limitations](#limitations)

---

## How it works

1. **Submission** — Carteiro listens on `:587`. Clients authenticate with
   `AUTH PLAIN/LOGIN`; the login is the account **email**, verified against a
   **bcrypt hash** read from the database.
2. **Anti-abuse (no open relay)** — the `MAIL FROM` (envelope) must be the
   authenticated account's own email or one of its `allowed_from` entries;
   anything else gets `553`. No authentication, no sending.
3. **Queueing** — upon `DATA`, Carteiro replies `250 Ok: queued as <id>`
   **only after** the message row is committed to the database. That is why it
   never loses messages on crash/restart.
4. **Delivery** — a worker claims due rows, resolves the **MX** of each
   recipient domain and delivers over SMTP port 25 with opportunistic STARTTLS
   and **DKIM** signing (key read from the database).
5. **Retry and dead-letter** — transient failures (4xx/network) reschedule
   the row with exponential backoff; permanent failures (5xx) or exhausted
   attempts mark it `dead` (visible and requeue-able through the API).
6. **Over the air** — accounts, allowed senders and DKIM domains are created
   and removed through the admin API while the server keeps running.

---

## Configuration

Carteiro works with **environment variables only** or with a YAML file.
Precedence:

```
system defaults  <  YAML file  <  CARTEIRO_* environment variables  <  --config flag
```

The config file is looked up (first one that exists wins):

| Location | OS |
|---|---|
| `/etc/carteiro/config.yaml` | Linux/macOS (system mode) |
| `~/.config/carteiro/config.yaml` | Linux |
| `~/Library/Application Support/carteiro/config.yaml` | macOS |
| path given by `CARTEIRO_CONFIG` or `--config` | any |

A commented example with every option lives in
[`config.example.yaml`](config.example.yaml).

### Storage (SQLite or MySQL)

```yaml
storage:
  type: sqlite            # or "mysql"
  sqlite_path: "/var/lib/carteiro/carteiro.db"   # empty = OS default data dir
  dsn: ""                 # MySQL connection string (when type: mysql)
```

- **SQLite** (default): one file, zero infrastructure, ideal for a single
  container/volume. Default path: `/var/lib/carteiro/carteiro.db` on Linux
  root/systemd, `~/.local/share/carteiro/carteiro.db` (Linux) or
  `~/Library/Application Support/carteiro/carteiro.db` (macOS).
- **MySQL**: set `CARTEIRO_DB_DSN` (or `storage.dsn`) and the server uses that
  connection — several instances can share one database.

Environment equivalents: `CARTEIRO_STORAGE_TYPE`, `CARTEIRO_SQLITE_PATH`,
`CARTEIRO_DB_DSN`.

### Web panel + API token (plain text, config only)

```yaml
api:                          # admin REST API (separate port)
  listen: "127.0.0.1:9090"    # loopback by default; env: CARTEIRO_API_LISTEN=9090
  token: "a-long-random-token"
web:                          # web dashboard (React SPA)
  listen: ":8080"             # binds all interfaces; env: CARTEIRO_WEB_LISTEN=8080
```

The token is **deliberately not stored in the database** — it lives only here
(or in `CARTEIRO_API_TOKEN`), because only the server owner sees it. The API
stays **off** until the token is configured.

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `CARTEIRO_LISTEN` | `587` | submission server port — type the number, no `:` needed |
| `CARTEIRO_HOSTNAME` | machine hostname | name used in EHLO/banner (should match the PTR) |
| `CARTEIRO_STORAGE_TYPE` | `sqlite` | `sqlite` or `mysql` |
| `CARTEIRO_SQLITE_PATH` | OS default | SQLite database file |
| `CARTEIRO_DB_DSN` | — | MySQL DSN (implies `type: mysql`) |
| `CARTEIRO_API_LISTEN` | `9090` | admin API port — type the number, no `:` needed (loopback by default; off without a token) |
| `CARTEIRO_WEB_LISTEN` | `8080` | web dashboard port — type the number, no `:` needed |
| `HTTP_ADDR` / `CARTEIRO_HTTP_ADDR` | — | aliases for `CARTEIRO_WEB_LISTEN` (the panel, not the API) |
| `CARTEIRO_API_TOKEN` | — | single bearer token (enables the admin API) |
| `CARTEIRO_ACCOUNTS` | — | seed accounts `email:password` separated by `;` |
| `CARTEIRO_DKIM_KEYS` | — | DKIM seeds: `doma.com:mail:<b64>;domb.com:selB:<b64>` (`;` separated, each key is the base64 of the whole PEM file) |
| `CARTEIRO_MAX_MESSAGE_SIZE` | `26214400` (25 MiB) | per-message size limit in bytes |
| `CARTEIRO_MAX_RECIPIENTS` | `100` | recipients per message |
| `CARTEIRO_DELIVERY_MAX_ATTEMPTS` | `10` | delivery attempts before dead-letter |
| `CARTEIRO_DELIVERY_CONCURRENCY` | `4` | concurrent deliveries |
| `CARTEIRO_DELIVERY_RETRY_BASE` | `1m` | initial retry delay (doubles each attempt) |
| `CARTEIRO_DELIVERY_RETRY_MAX` | `4h` | max delay between attempts |
| `CARTEIRO_DELIVERY_CONNECT_TIMEOUT` | `30s` | MX connect timeout |
| `CARTEIRO_DELIVERY_IO_TIMEOUT` | `2m` | per-MX exchange timeout |
| `CARTEIRO_DELIVERY_POLL_INTERVAL` | `5s` | queue scan frequency |
| `CARTEIRO_QUEUE_DEAD_MAX` | `1000` | max dead-letter rows kept (`0` = unlimited) |
| `CARTEIRO_REQUIRE_TLS` | `false` | `true` requires STARTTLS before AUTH (outside loopback) |
| `CARTEIRO_TLS_CERT` / `CARTEIRO_TLS_KEY` | — | enable TLS with base64 of the PEM cert/key (files are not supported) |
| `CARTEIRO_TLS_MODE` | `starttls` | `starttls` (587) or `implicit` (465) |
| `CARTEIRO_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `CARTEIRO_ACME` | `false` | manage a Let's Encrypt certificate for the SMTP listener in-process (off = proxy/base64 TLS handles it) |
| `CARTEIRO_ACME_EMAIL` | — | registration e-mail (required when ACME is on) |
| `CARTEIRO_ACME_HTTP_ADDR` | `:80` | listener for the http-01 challenge |
| `CARTEIRO_ACME_STAGING` | `false` | `true` = Let's Encrypt staging (test before going live) |
| `CARTEIRO_LOG_MASK_EMAILS` | `true` | e-mails are masked in the logs (`joao@example.com` → `j***@e***.com`); set `false` to log full addresses (debugging only) |
| `CARTEIRO_CONFIG` | — | path to the YAML file |

> **Security**: without TLS and with `require_tls: false` (the default), the
> password travels in plain text. Run Carteiro on **loopback or a private
> network/VPN**, or enable TLS (see below). A warning is logged at boot.

---

## Seeds: first run, upserts and clear logs

Accounts and DKIM keys declared in the YAML file or environment are **seeds**:
on **every startup** Carteiro **upserts** them into the database
(idempotent), hashing account passwords with bcrypt, and logs — clearly and
individually — what happened to each one:

```
INFO seed: upserting accounts done created=[a@example.com] updated=[b@example.com] unchanged=[]
INFO seed: a@example.com -> created in the database    kind=accounts
INFO seed: b@example.com -> updated (new password or allowed_from) kind=accounts
INFO seed: no dkim keys declared in config (add them later via the admin API)
```

- **created** — the account/domain did not exist and was inserted;
- **updated** — it existed with a different password and/or `allowed_from`
  and was replaced;
- **unchanged** — it already matches the seed, left untouched (logged at
  debug level to keep startup output readable).

Because they are seeds, **editing the YAML later does not delete or hide
database rows**: from the first boot on, accounts and DKIM domains are managed
**over the air** through the admin API. Seeds simply guarantee that a fresh
server comes up already usable.

Multiple projects share one server: give each project its own account (and
`allowed_from`) plus its own DKIM key per domain — see
[Multiple projects and domains](#multiple-projects-and-domains).

## Keys and certificates as base64

Private keys (DKIM) and TLS certificates/keys are configured **only as
base64**, never as filesystem paths. Base64 is just an encoding — it is not
encryption — but it keeps each secret on a single manageable line and avoids
mounting/reading files, which is why every config surface (YAML `key_data`,
`CARTEIRO_DKIM_KEYS`, `CARTEIRO_TLS_CERT/KEY`) accepts it.

### Generate and encode a DKIM key

```bash
# 1) generate the key pair (RSA 2048) with openssl
openssl genrsa -out dkim.yourdomain.com.key 2048

# 2) encode the PRIVATE key as base64 for the config
KEY=$(base64 -w0 dkim.yourdomain.com.key)     # Linux
# macOS (wraps lines; also accepted, or use -b 0):
# KEY=$(base64 -i dkim.yourdomain.com.key | tr -d '\n')

# 3) put it in the config seed or env (base64 of the whole PEM, one line):
#    YAML: dkim: [{domain, selector, key_data: "$KEY"}]
#    env:  CARTEIRO_DKIM_KEYS="yourdomain.com:mail:$KEY"
#         (several domains: "doma.com:mail:$KEY_A;domb.com:mail:$KEY_B")

# 4) publish the PUBLIC part in DNS (never in the config):
openssl rsa -in dkim.yourdomain.com.key -pubout -out dkim.pub
grep -v -- '-----' dkim.pub | tr -d '\n'   # -> p=... for <selector>._domainkey.<domain>
```

### Generate and encode a TLS certificate/key

```bash
# self-signed cert (client trusts it only if you add it to the CA store)
openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
  -keyout tls.key -out tls.crt \
  -subj "/CN=smtp.yourdomain.com" \
  -addext "subjectAltName=DNS:smtp.yourdomain.com"

CERT=$(base64 -w0 tls.crt)
KEY=$(base64 -w0 tls.key)
# -> YAML: tls.cert_data/key_data  |  env: CARTEIRO_TLS_CERT / CARTEIRO_TLS_KEY
```

Wrapped base64 (multiple lines) is accepted too — Carteiro strips whitespace
before decoding. Whatever you do, keep the base64 **private**: it is the key
material; only the DNS record above is meant to be public.

---

## Web dashboard and admin API (bearer)

The repository ships a small **React dashboard** (Vite + Tailwind, AWS/GCP
console style, dark/light) that is compiled and **embedded into the binary**
(`go:embed`): one process, no Node.js at runtime (the SPA and the API listen on separate ports). It talks only to
the API below using the same bearer token. Pages: **Dashboard** (counters,
queue gauges, recent activity), **Compose** (send an e-mail straight into the
queue), **Sends** (history with rendered HTML/text previews and live delivery
status) and **Accounts** (add/remove SMTP users). The SPA is served at `/`;
open `http://<host>:8080/` and log in with the API token.

The admin API (`api.listen`, loopback `127.0.0.1:9090` by default) and the web
dashboard (`web.listen`, `:8080` by default) listen on **separate ports** in
the same process. The panel serves the SPA and **proxies `/api/*` to the API
listener in-process**, so the browser keeps one origin while the API stays
independently bindable and reachable. Both are **off** until a token is
configured. Every call except `/health` and `/metrics` needs
`Authorization: Bearer <token>`.

| Endpoint | Description |
|---|---|
| `GET /api/health` | liveness (no auth) |
| `GET /api/metrics` | Prometheus metrics (no auth) |
| `GET /api/accounts` | list accounts (emails + allowed_from, never hashes) |
| `POST /api/accounts` | create/update: `{"email","password","allowed_from":[]}` → 201/200 |
| `PATCH /api/accounts/{email}` | edit an account: replace `allowed_from` and/or set a new `password` (empty/omitted keeps the current one) |
| `DELETE /api/accounts/{email}` | remove an account |
| `GET /api/dkim` | list domains + selectors (never the private key) |
| `POST /api/dkim` | create/update: `{"domain","selector","key_data"}` (PEM) → 201/200 |
| `DELETE /api/dkim/{domain}` | remove a DKIM key |
| `GET /api/queue/stats` | `{"queued":N,"due":N,"dead":N}` |
| `GET /api/queue?status=dead` | list queued/dead messages (no bodies) |
| `POST /api/queue/{id}/retry` | move a dead message back to the queue (attempts reset) |
| `GET /api/stats` | dashboard summary: counters + queue gauges + version/uptime |
| `GET /api/sends?limit=N` | recent sends (ring buffer): subject, status, attempts |
| `GET /api/sends/{id}` | one send with rendered `html`/`text` + raw source |
| `POST /api/send` | compose and queue `{"from","to":[],"subject","text","html"}` → 201 |
| `GET /api/openapi.json` | OpenAPI 3 document (no auth) — point Swagger UI at it |

> The canonical paths are under `/api`. The pre-dashboard routes `/health`,
> `/metrics`, `/openapi.json`, `/dkim` and `/queue` also keep their legacy
> root alias; `/accounts` and the dashboard endpoints are **`/api` only**.
> The React pages live on the **web port** and the API on its **own port**
> (the panel proxies `/api/*` to it); opening the API port directly never
> returns the SPA. Messages composed through `POST /api/send`
> respect the same sender rules as SMTP (the `from` must belong to an account
> or its `allowed_from`) and land in the same queue, so delivery, DKIM
> signing and retries behave identically. Recent-sends tracking is an
> in-memory ring (200 messages, body capped); it resets on restart and never
> stores credentials.

```bash
TOKEN="a-long-random-token"
BASE="http://127.0.0.1:8080"

# add a new project account over the air (password hashed server side)
curl -X POST "$BASE/api/accounts" -H "Authorization: Bearer $TOKEN" -d '{
  "email": "project-x@yourdomain.com",
  "password": "s3cr3t",
  "allowed_from": ["news@yourdomain.com"]
}'

# add its DKIM domain
curl -X POST "$BASE/api/dkim" -H "Authorization: Bearer $TOKEN" -d "{
  \"domain\": \"yourdomain.com\", \"selector\": \"mail\",
  \"key_data\": \"$(sed -z 's/\n/\\n/g' /etc/carteiro/dkim.key)\"
}"

# monitoring
curl "$BASE/api/queue/stats" -H "Authorization: Bearer $TOKEN"
curl "$BASE/api/queue?status=dead" -H "Authorization: Bearer $TOKEN"
curl -X POST "$BASE/api/queue/20260903T120000.000000000Z-abc/retry" -H "Authorization: Bearer $TOKEN"

# Prometheus (no auth; scrape only from trusted networks)
curl "$BASE/metrics"

# compose a message from the panel API (text + html; queued like an SMTP one)
curl -X POST "$BASE/api/send" -H "Authorization: Bearer $TOKEN" -d '{
  "from": "news@yourdomain.com", "to": ["client@example.com"],
  "subject": "Hello", "text": "plain", "html": "<b>rich</b>"
}'
```
The OpenAPI document (`GET /openapi.json`) describes every endpoint, schema
and the bearer security scheme; point a Swagger UI at it, e.g.
<https://petstore.swagger.io?url=http://127.0.0.1:8080/openapi.json>.

Prometheus metrics include counters (`carteiro_messages_queued_total`,
`carteiro_messages_delivered_total`, `carteiro_messages_dead_total`,
`carteiro_auth_success_total`, ...) and queue gauges
(`carteiro_queue_queued`, `carteiro_queue_due`, `carteiro_queue_dead`).

> Bind the API to loopback and reach it through a private network/VPN (or
> your orchestration layer). If you expose it publicly, put it behind TLS.

---


## Ports and network

Which ports the server uses, and when you must care about them:

| Port | Direction | Purpose | Config |
|---|---|---|---|
| **587** | inbound (clients → Carteiro) | SMTP submission: where Nodemailer connects | `CARTEIRO_LISTEN` (default `:587`) |
| **465** | inbound (optional) | implicit-TLS submission (only with `tls.mode: implicit`) | `listen: ":465"` + `tls` block |
| **8080** | inbound (web) | web dashboard (React SPA; proxies `/api/*` to the API port) | `web.listen` / `CARTEIRO_WEB_LISTEN` / `HTTP_ADDR` (default `:8080`, **off** without `api.token`) |
| **9090** | inbound (admin) | admin REST API `/api/*` + legacy `/health`, `/metrics`, `/openapi.json`, `/dkim`, `/queue` | `api.listen` / `CARTEIRO_API_LISTEN` (default `127.0.0.1:9090`, **off** without `api.token`) |
| **25 (outbound)** | outbound (Carteiro → internet) | Carteiro **connects to** the MX servers of recipients; it never listens on 25 | none — but the host/container must be allowed to open outbound port 25 (see the DNS section) |

Practical rules:

- **587** is the only port your applications need. Publish it **only** on a
  private network/VPN, or require TLS (`require_tls: true` + the `tls` block);
  never expose plain-text AUTH on the public internet.
- **8080** (web panel) binds all interfaces by default. The panel is
  protected by the bearer token (login screen); `/api/*` calls are proxied to
  the API port with the same header. Keep the port firewalled anyway, or
  reach it over a VPN / SSH tunnel.
- **9090** (admin API) binds to `127.0.0.1` by default: inside a container it
  is only reachable by the panel's in-process proxy. To call it from the host
  or external tools, set `CARTEIRO_API_LISTEN=9090` (bare number) and publish
  (firewalled).
- Changing the SMTP port is just `CARTEIRO_LISTEN=":2525"` (then point
  Nodemailer at 2525); there is no magic in 587/465 besides convention.


---

## Docker mode

Images are published automatically to **GHCR** by the
[GitHub Actions workflow](#development) for `linux/amd64` and `linux/arm64`:

```bash
docker pull ghcr.io/itxtoledo/carteiro:latest
```

The image is **configured entirely through environment variables**: it ships
with no config file, so a minimal usable server needs only two vars
(`CARTEIRO_ACCOUNTS` + `CARTEIRO_API_TOKEN`) — everything else has a
sane default. A YAML config can still be mounted at `/etc/carteiro/config.yaml`
if you prefer, and YAML values are overridden by env vars.

### Publishing the ports (Docker / Coolify: Ports Exposes and Port Mappings)

Inside the container Carteiro binds three ports (one per listener). The
listener env values bind **all interfaces** by default (`:8080`), and a bare
port number is accepted and normalized (`8080` → `:8080`):

| Port | Service | Listener env / config |
|---|---|---|
| `587` | SMTP submission | `CARTEIRO_LISTEN` (`listen`, default `:587`) |
| `8080` | Web dashboard (SPA) | `CARTEIRO_WEB_LISTEN` (`web.listen`, default `:8080`) |
| `9090` | Admin API | `CARTEIRO_API_LISTEN` (`api.listen`, default `127.0.0.1:9090`) |

Only what you map to the **host** becomes reachable from outside the
container. In the Coolify UI (or `docker run -p`, compose, etc.):

- **Ports Exposes** — tells the platform which ports the container listens on
  (used by probes/tooling). List the container ports, one per line: `587`,
  `8080`, `9090`. This field is *not* where host access is configured and it
  does not need port ranges.
- **Port Mappings** — the actual `host:container` bindings. Use one entry per
  port you want exposed, typically `1:1`:

  ```text
  587:587        # SMTP: applications/submission clients
  8080:8080      # web dashboard (token-protected login)
  9090:9090      # admin API — only if you want to call it from outside
  ```

  An entry like `8080:8080:587` is invalid (it mixes two ports into one
  mapping). Leave a port out of the mappings and it stays unreachable from
  the host, even if listed under Exposes.
- **Network Aliases** — optional service name for other containers on the
  same private network (e.g. `carteiro`); leave empty when nothing else talks
  to it by name.

Which mappings you need:

| I want to… | Mappings to add | Extra env |
|---|---|---|
| only SMTP for internal apps | `587:587` | none |
| open the web dashboard too | + `8080:8080` | none (`web.listen` already binds `:8080`) |
| call the admin API from the host/tools | + `9090:9090` | `CARTEIRO_API_LISTEN=9090` (a bare number is fine; without it the API binds loopback only and cannot be reached from the host) |

The web dashboard proxies `/api/*` to the API listener **inside the
container**, so a browser only needs port `8080` even when the API is not
published at all. Keep published ports firewalled when possible; the panel is
protected by the bearer token, but the SMTP port must not be exposed to the
public internet without TLS (`require_tls: true`).

### Quick example with environment variables

```bash
docker run -d --name carteiro \
  -p 587:587 \
  -p 8080:8080 \
  -p 9090:9090 \
  -e CARTEIRO_ACCOUNTS='sender@yourdomain.com:a-strong-password' \
  -e CARTEIRO_API_TOKEN='a-long-random-token' \
  -e CARTEIRO_API_LISTEN='9090' \    # bare number; reaches the API from the host (loopback by default)
  -v carteiro-data:/var/lib/carteiro \
  ghcr.io/itxtoledo/carteiro:latest
```

Follow the logs with `docker logs -f carteiro` — you will see the seed
upsert lines on startup. If you only need the SMTP relay (no panel/API), drop the
`-p 8080:8080`, `-p 9090:9090`, `CARTEIRO_API_TOKEN` and the listen lines.

The image ships a `HEALTHCHECK` that probes the SMTP listener every 30s
(`docker inspect --format '{{.State.Health.Status}}' carteiro`). Compose can
override it, e.g. to probe the API instead:

```yaml
    healthcheck:
      test: ["CMD", "wget", "-q", "-O-", "http://127.0.0.1:9090/health"]
      interval: 30s
      timeout: 5s
      retries: 3
```

### Passing DKIM keys when running in Docker

DKIM seeds are declared the same way as accounts, via environment variables.
Use the **base64 of the private key file** (one clean line; the API also
accepts PEM directly). Remember: the **private** key goes here, the
**public** key goes to the DNS record (`<selector>._domainkey.<domain>`).

```bash
KEY=$(base64 -w0 dkim.yourdomain.com.key)   # base64 of the PRIVATE key

docker run -d --name carteiro \
  -p 587:587 \
  -e CARTEIRO_ACCOUNTS='sender@yourdomain.com:a-strong-password' \
  -e "CARTEIRO_DKIM_KEYS=yourdomain.com:mail:$KEY" \
  -v carteiro-data:/var/lib/carteiro \
  ghcr.io/itxtoledo/carteiro:latest
```

Serving **two domains** (e.g. two projects) with the relay? One variable holds
both keys, entries separated by `;` as `domain:selector:base64`:

```bash
KEY_A=$(base64 -w0 dkim.doma.com.key); KEY_B=$(base64 -w0 dkim.domb.com.key)

docker run -d --name carteiro \
  -p 587:587 \
  -e CARTEIRO_ACCOUNTS='a@doma.com:pass-a;b@domb.com:pass-b' \
  -e "CARTEIRO_DKIM_KEYS=doma.com:mail:$KEY_A;domb.com:mail:$KEY_B" \
  -v carteiro-data:/var/lib/carteiro \
  ghcr.io/itxtoledo/carteiro:latest
```

With docker compose, put the base64 strings directly in the environment:

```yaml
    environment:
      CARTEIRO_ACCOUNTS: "a@doma.com:pass-a;b@domb.com:pass-b"
      # doma.com:mail:<base64>;domb.com:mail:<base64>
      CARTEIRO_DKIM_KEYS: "doma.com:mail:QUJDREVGR0hJSktMTU5PUFE=;domb.com:mail:UkVGR0hJSktMTU5PUFFSU1Q="
```

On startup the seed logs confirm each domain, e.g.:
`seed: doma.com -> created in the database`. To add/rotate a domain later
without restarting, use the admin API (`POST /dkim`) instead.


### Persisting the SQLite database (Docker)

The whole state — accounts (bcrypt hashes), DKIM keys and the message queue —
is **one SQLite file**: `/var/lib/carteiro/carteiro.db`. (The container runs
as root so it can bind the SMTP port 587 and write to any mounted volume.) To
persist it:

```bash
# named volume (recommended): survives container removal
docker run -d --name carteiro \
  -p 587:587 \
  -e CARTEIRO_ACCOUNTS='sender@yourdomain.com:password' \
  -e CARTEIRO_API_TOKEN='token' \
  -v carteiro-data:/var/lib/carteiro \
  ghcr.io/itxtoledo/carteiro:latest

docker volume inspect carteiro-data     # where Docker stores it on the host
docker rm -f carteiro && docker run ... # same -v volume: data comes back
```

- **Bind mount** alternative: `-v "$PWD/data":/var/lib/carteiro` (the
  container runs as root, so any host folder works without chown tricks).
- **Different location**: set `CARTEIRO_SQLITE_PATH=/data/carteiro.db` and
  mount your volume there instead.
- **Backup** (SQLite is a single file; stop the server first so the WAL is
  flushed):
  ```bash
  docker stop carteiro
  docker run --rm -v carteiro-data:/data -v "$PWD":/backup \
    alpine tar czf /backup/carteiro-backup.tgz -C /data .
  docker start carteiro
  ```
- **Restore**: stop the container, wipe the volume
  (`docker run --rm -v carteiro-data:/data alpine sh -c 'rm -rf /data/*'`),
  extract the backup into it, start again. The queue/accounts/DKIM all come
  back exactly as they were.
- **MySQL instead**: mount nothing; just set `CARTEIRO_DB_DSN` and back up the
  MySQL database normally.

### Example with docker compose

```yaml
services:
  carteiro:
    image: ghcr.io/itxtoledo/carteiro:latest
    restart: unless-stopped
    ports: ["587:587", "8080:8080", "9090:9090"]
    environment:
      CARTEIRO_ACCOUNTS: "sender@yourdomain.com:password"
      CARTEIRO_API_TOKEN: "a-long-random-token"
      CARTEIRO_API_LISTEN: "9090"    # admin API (bare number; loopback by default)
    volumes:
      - carteiro-data:/var/lib/carteiro
volumes:
  carteiro-data:
```

```bash
docker compose up -d
```

### Generate the env vars interactively

There is a helper script that asks the questions (hostname, account, DKIM
domain/selector, TLS option) and writes a ready-to-paste `.txt` with every
`CARTEIRO_*` variable. It generates the DKIM RSA-2048 key pair — and, when
asked, a self-signed TLS certificate — and prints the DNS records to
publish (`A`, DKIM `p=`, SPF, PTR). It runs on **macOS and Linux** (only
`bash` and `openssl` are required):

```bash
./scripts/gen-envs.sh                    # writes ~/Desktop/carteiro-envs.txt (macOS)
./scripts/gen-envs.sh                    # writes ~/carteiro-envs.txt (Linux)
./scripts/gen-envs.sh /path/to/envs.txt  # or pick the output path
```

> Each run generates **fresh keys**. If you already published the DKIM `p=`
> in DNS, keep that key and do not re-run the script for the same domain —
> either reuse the previous `.txt` or add the new key under a new selector.

The script prints the DNS records at the end; the full explanation of each
one lives in **[DNS.md](DNS.md)**.

---

## Daemon mode

The same binary runs as a system process.

### Linux with systemd (recommended)

```bash
# 1) build the binary (or download a release)
make build

# 2) system user + installation
sudo useradd --system --home /var/lib/carteiro --shell /usr/sbin/nologin carteiro
sudo mkdir -p /var/lib/carteiro /etc/carteiro
sudo chown carteiro:carteiro /var/lib/carteiro
sudo install -m 0755 bin/carteiro /usr/local/bin/carteiro
sudo install -m 0640 config.example.yaml /etc/carteiro/config.yaml

# 3) edit /etc/carteiro/config.yaml (accounts, dkim, api token...)
sudoedit /etc/carteiro/config.yaml

# 4) install and enable the service (ready-made unit in the repo)
sudo install -m 0644 systemd/carteiro.service /etc/systemd/system/carteiro.service
sudo systemctl daemon-reload
sudo systemctl enable --now carteiro

# 5) follow the logs (seed upserts appear on startup)
journalctl -u carteiro -f
```

The unit uses `StateDirectory=carteiro`, which creates `/var/lib/carteiro`
with the right ownership automatically (the SQLite file lives there).

### Linux/macOS without systemd (nohup / foreground)

```bash
make install   # installs the binary + default config/data folders
carteiro --config "$HOME/.config/carteiro/config.yaml"   # foreground
nohup carteiro > /var/log/carteiro.log 2>&1 &            # simple daemon
```

### macOS with launchd

Save as `~/Library/LaunchAgents/com.yourdomain.carteiro.plist` with
`ProgramArguments: [/usr/local/bin/carteiro, --config, /usr/local/etc/carteiro/config.yaml]`,
`RunAtLoad` and `KeepAlive` true, then `launchctl load` it.

---

## Sending with Nodemailer

The transport login is the **account** (email) with its password; the message
`from` must be that email (or an `allowed_from` of the account).

### Private network/VPN, no TLS on the relay

```js
const nodemailer = require("nodemailer");

const transporter = nodemailer.createTransport({
  host: "CARTEIRO-IP",   // or localhost / docker service name
  port: 587,
  secure: false,
  requireTLS: false,
  auth: { user: "sender@yourdomain.com", pass: "a-strong-password" },
});

await transporter.sendMail({
  from: "sender@yourdomain.com", // MAIL FROM = account => accepted
  to: "client@example.com",
  subject: "Hello from Carteiro",
  html: "<b>It works!</b>",
});
```

### With TLS on the relay

- **STARTTLS (587)**: `secure: false`, `requireTLS: true`.
- **Implicit TLS (465)**: `secure: true`.

If the relay sets `require_tls: true`, plaintext connections outside loopback
are refused at AUTH with `538`.

---

## The database queue

The queue is a **table** (`queue_messages`) in SQLite/MySQL; there are no
message files anymore. A message row carries the sender, the remaining
recipients, the raw content (BLOB), attempt count, next-attempt time and the
permanent-failure map.

### Tuning (YAML or environment)

The retry/queue behavior is parameterized in the `delivery` block
(`delivery.max_attempts`, `retry_base`, `retry_max`, `connect_timeout`,
`io_timeout`, `poll_interval`, `concurrency`) and the dead-letter cap in
`queue.dead_max` — every one of them also has a `CARTEIRO_DELIVERY_*` /
`CARTEIRO_QUEUE_DEAD_MAX` environment variable:

```yaml
delivery:
  max_attempts: 10     # retries before a message is marked dead
  retry_base: 1m       # 1m, 2m, 4m, 8m...
  retry_max: 4h
  concurrency: 4
queue:
  dead_max: 1000       # keep at most 1000 dead rows (0 = unlimited);
                       # the oldest are pruned automatically
```

### Guarantees and behavior

- **Durability**: the `250 queued` reply only comes after the `INSERT`
  commits. Schema migrations run automatically at startup.
- **Claim/lease**: due rows are claimed with a lease (`lease_until`). If the
  process dies mid-delivery, the row becomes due again after the lease
  expires (30 minutes) — **at-least-once**, with a tiny duplication window,
  exactly like the old file queue.
- **Retry**: transient failures follow the `1m, 2m, 4m, 8m...` backoff
  (configurable via `delivery.*`) until `max_attempts`; then the row becomes
  `dead`.
- **Dead-letter**: `status=dead` rows keep the full reason in `last_error` and
  per-recipient causes in `permanent`. Inspect and requeue them through the
  API (`GET /queue?status=dead`, `POST /queue/{id}/retry`) — the retry resets
  the attempt counter. Beyond `queue.dead_max` rows (default 1000), the
  oldest dead messages are pruned automatically.
- **Multiple instances**: SQLite fits a single instance; with MySQL several
  instances can share the queue (claims are atomic updates).

### Backup

Back up the SQLite file (`sqlite3 /var/lib/carteiro/carteiro.db ".backup
backup.db"` or copy it while the server is stopped), or dump MySQL. That is
the entire state of the server.

---

## Multiple projects and domains

A single Carteiro server can serve several of your projects, each with its own
domain: **one account per project** (own login/password and `allowed_from`
list) plus **one DKIM key per sender domain**. A project can only send with
its own addresses, so projects never impersonate each other.

```yaml
# seeds on first boot (after that: manage via the admin API)
accounts:
  - email: "a@doma.com"          # project A
    password: "password-a"
    allowed_from: ["news@doma.com"]
  - email: "b@domb.com"          # project B
    password: "password-b"

# In YAML the private key is passed only as base64 of the PEM file:
#   KEY_A=$(base64 -w0 dkim-doma.key); KEY_B=$(base64 -w0 dkim-domb.key)
dkim:
  - domain: "doma.com"
    selector: "mail"
    key_data: "PASTE-BASE64-OF-dkim-doma.key"
  - domain: "domb.com"
    selector: "mail"
    key_data: "PASTE-BASE64-OF-dkim-domb.key"
```

Per project, generate + publish its own key and add its SPF record — all
domains include the same outbound IP (section 9 below). The environment
variable `CARTEIRO_DKIM_KEYS` covers several domains too; for anything
beyond that use the YAML `dkim` list or the admin API. For hard isolation
(separate database, own limits), run one container/daemon per project with
its own `CARTEIRO_SQLITE_PATH` volume.

---

## DNS: keeping email out of spam

> **Full guide**: the section below is the quick version. For every record in
> depth, Cloudflare-specific notes and a troubleshooting table, see
> **[DNS.md](DNS.md)**.

Carteiro **only sends** (it does not receive). For Gmail, Outlook etc. to
deliver to the inbox — not spam — your domain needs **SPF + DKIM + DMARC**
aligned, plus **reverse DNS (PTR)** on the outbound IP. The steps below use
`yourdomain.com` and assume the Carteiro server has a fixed public IP
`203.0.113.10` (replace with yours).

> **Prerequisite**: the outbound IP must be allowed to make port 25
> connections. Cloud providers (AWS EC2, GCP, Azure, Oracle, DigitalOcean on
> some plans) **block outbound port 25** by default — check/enable it or pick
> a provider that allows it.

### 1. SPF (authorize the IP to send for your domain)

SPF tells receiving servers which IP addresses are allowed to send for your
domain. The recommended record for a dedicated relay IP is **explicit**:

```dns
yourdomain.com.  TXT  "v=spf1 ip4:203.0.113.10 -all"
```

**It does not have to be an IP — it can reference a domain**, as long as that
domain resolves to the sending IP. SPF compares the source IP of the
connection against what the policy lists:

| Mechanism | Example | Meaning |
|---|---|---|
| `ip4:` / `ip6:` | `v=spf1 ip4:203.0.113.10 -all` | allow that IP directly (0 DNS lookups) |
| `a:` | `v=spf1 a:smtp.yourdomain.com -all` | allow the IP(s) of the `A` record of `smtp.yourdomain.com` |
| `include:` | `v=spf1 include:yourdomain.com -all` | delegate to the SPF published at that domain (provider setups) |
| `mx:` | `v=spf1 mx -all` | allow the IPs in the domain's MX records |

Important rules:

- A bare domain is **not valid** — always use the mechanism keyword
  (`a:smtp.yourdomain.com`, never just `smtp.yourdomain.com`).
- SPF allows at most **10 DNS lookups**; `ip4:` costs 0, `a:`/`include:` cost 1.
- `ip4:` with a fixed IP is the best practice here: explicit, zero lookups,
  no dependency on an `A` record. If your relay IP ever changes, switch to
  `a:smtp.yourdomain.com` (with the `A` record updated) or rotate the record.
- If you also send over IPv6, add the address with the `ip6:` mechanism, e.g.
  `v=spf1 ip4:203.0.113.10 ip6:2001:db8::1234 -all`.
- Other legitimate services (newsletters, transactional providers) can be
  combined with `include:`; **never** use `+all`.

Verify: `dig TXT yourdomain.com +short`


### 2. DKIM (signing messages)

Generate the key and publish the public one:

```bash
openssl genrsa -out /etc/carteiro/dkim-2026.key 2048
chmod 600 /etc/carteiro/dkim-2026.key
openssl rsa -in /etc/carteiro/dkim-2026.key -pubout -out /tmp/dkim.pub
PUB=$(grep -v -- '-----' /tmp/dkim.pub | tr -d '\n')
echo "mail._domainkey.yourdomain.com.  TXT  \"v=DKIM1; k=rsa; p=$PUB\""
```

Add the key as a seed (YAML `dkim:` / `CARTEIRO_DKIM_*`) or through the admin
API (`POST /dkim`). Carteiro signs every message whose envelope-from belongs
to that domain and the log confirms it: `message signed with DKIM`.

Verify: `dig TXT mail._domainkey.yourdomain.com +short`
(tools such as [dkimvalidator.com](https://dkimvalidator.com) test the
signature end to end.)

> DKIM alignment requires the signature domain `d=` to equal (or be a
> subdomain of) the `From` header domain. Because the relay forces `MAIL
> FROM` to be an account of the configured domain, alignment is guaranteed.

### 3. DMARC (policy + reports)

```dns
_dmarc.yourdomain.com.  TXT  "v=DMARC1; p=none; rua=mailto:postmaster@yourdomain.com; pct=100"
```

Start with `p=none`, watch the reports, and once SPF+DKIM are aligned, move
to `p=quarantine`.

Verify: `dig TXT _dmarc.yourdomain.com +short`

### 4. Reverse DNS (PTR) — most ignored, most important

The server's public IP needs a **PTR pointing to Carteiro's hostname** (the
`hostname:` config value, used in EHLO). Configure it in your provider/VPS
panel (rDNS), e.g. `10.113.0.203.in-addr.arpa. PTR smtp.yourdomain.com.`,
plus an `A` record for `smtp.yourdomain.com` pointing to that IP.

Verify: `dig -x 203.0.113.10 +short`

### 5. MX record for your domain (recommended)

Not needed to **send**, but good practice to receive bounces/DMARC reports:

```dns
yourdomain.com.  MX  10 smtp.yourdomain.com.
```

### Checklist before the first real send

- [ ] Outbound port 25 allowed by the provider
- [ ] SPF published, containing the server IP
- [ ] DKIM generated, published and active (seed or API; check the boot log)
- [ ] DMARC published (start with `p=none`)
- [ ] PTR/reverse DNS matching the `hostname:` config value
- [ ] Send a test to <https://www.mail-tester.com> and fix whatever it flags
- [ ] Send to Gmail/Outlook and confirm it lands in the inbox
- [ ] Mind volume: **warm up** a new IP (few emails at first)

---

## TLS on the submission port

### Option A — fixed certificate (base64)

```bash
openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
  -keyout /etc/carteiro/tls.key -out /etc/carteiro/tls.crt \
  -subj "/CN=smtp.yourdomain.com" \
  -addext "subjectAltName=DNS:smtp.yourdomain.com"
```

In YAML the certificate and key are passed **only as base64** of the PEM
files (one line each); certificate files are not supported:

```bash
CERT=$(base64 -w0 /etc/carteiro/tls.crt)   # or tls.crt PEM
KEY=$(base64 -w0 /etc/carteiro/tls.key)
```

```yaml
tls:
  mode: "starttls"          # port 587 (default). For 465: "implicit"
  cert_data: "PASTE-BASE64-OF-tls.crt"
  key_data: "PASTE-BASE64-OF-tls.key"
require_tls: true           # refuse AUTH without TLS outside loopback
```

Or via environment only (handy in Docker):

```bash
docker run -d -p 587:587   -e CARTEIRO_ACCOUNTS='sender@yourdomain.com:password'   -e CARTEIRO_TLS_CERT="$CERT"   -e CARTEIRO_TLS_KEY="$KEY"   -e CARTEIRO_TLS_MODE=starttls   -e CARTEIRO_REQUIRE_TLS=true   -v carteiro-data:/var/lib/carteiro   ghcr.io/itxtoledo/carteiro:latest
```

### Option B — managed Let's Encrypt (ACME, lego)

Instead of pasting a certificate, Carteiro can **obtain and renew its own**
Let's Encrypt certificate for the SMTP hostname (`CARTEIRO_HOSTNAME`). The
registration and the current certificate are stored in the database and the
listener resolves the certificate **dynamically**, so renewals never require a
restart or a redeploy.

Enable it with `CARTEIRO_ACME=true` (or the CLI flag `-acme`, which overrides
the environment):

```bash
CARTEIRO_ACME=true
CARTEIRO_ACME_EMAIL=you@example.com          # ACME registration
CARTEIRO_HOSTNAME=smtp.example.com           # public name of the relay
# challenge http-01: the public port 80 must reach the relay, so map 80:80
# (Coolify Port Mappings) and use a DNS-only (grey cloud) A record
```

Only the **http-01** challenge is used — no DNS provider, no API keys of any
kind. Carteiro binds `CARTEIRO_ACME_HTTP_ADDR` (default `:80`) during the
few seconds of validation. The YAML equivalent is
`acme.enabled/email/http_addr/staging`; `CARTEIRO_ACME_STAGING=true` points at
the Let's Encrypt staging directory for tests (those certificates are not
trusted by clients). Do not configure `tls.cert_data/key_data` at the same
time: managed mode ignores them.

> **Running behind a proxy?** Leave ACME off (the default). A proxy in front
> (Coolify/Traefik, another MTA, a load balancer) keeps terminating TLS and
> Carteiro serves plaintext or with the base64 certificate from Option A. The
> toggle exists exactly so both setups live in the same binary: `false` = the
> proxy owns the certificate, `true` = Carteiro owns it.

---

## Development

### Building (production: one binary, no Node)

```bash
make web         # cd web && npm ci && npm run build  -> web/dist
make build       # builds web (if needed) then bin/carteiro with the UI embedded
make test        # go test ./...
make vet         # go vet ./...
make run         # build + run
make install     # installs binary + default config/data folders
```

`go build` (and `go test`) compile out of the box: the committed placeholder
under `web/dist` satisfies the `go:embed` directive; `make build`, Docker and
CI replace it with the real Vite output before compiling.

### Developing the dashboard (hot reload)

The UI iterates with Vite against the running Go server — no embed rebuild:

```bash
# terminal 1: the relay (web panel :8080, admin API on loopback :9090, SMTP :587)
go run ./cmd/carteiro

# terminal 2: Vite on :5173, proxying /api to 127.0.0.1:8080
make web-dev
```

Open http://localhost:5173 (frontend sources live in `web/`; the compiled
React app in `web/dist` is served by the binary at `http://localhost:8080`
and proxies `/api` to the API listener on `127.0.0.1:9090`).

### Toolbox

```bash
make build       # web + go build -> bin/carteiro
make test        # go test ./...
make vet         # go vet ./...
make run         # build + run
make install     # installs binary + default config/data folders
```

Layout:

```
cmd/carteiro/         main: flags, signals, wiring, seed upserts
internal/config/      YAML + env + per-OS defaults
internal/storage/     database layer: sqlite + mysql, queue, accounts, dkim
internal/smtpd/       SMTP submission server (AUTH, DATA, STARTTLS)
internal/relay/       MX delivery (retry/backoff, dead-letter)
internal/dkim/        DKIM signing (RSA/Ed25519)
internal/api/         admin REST API + dashboard endpoints (bearer)
internal/sends/       recent-sends ring + message parse/build (panel feed)
internal/webui/       embedded SPA handler (web/dist via go:embed)
internal/metrics/     Prometheus counters
web/                  React + Vite + Tailwind dashboard (src -> dist)
systemd/              example unit
.github/workflows/    multi-arch image build (amd64+arm64 -> GHCR)
```

The single workflow [`.github/workflows/release.yml`](.github/workflows/release.yml)
does everything for every `v*` tag:

1. **image** — builds `linux/amd64` + `linux/arm64` with buildx+QEMU and
   pushes to GHCR (`ghcr.io/OWNER/carteiro:vX.Y.Z`, also tagged `latest`);
2. **binaries** — cross-compiles static linux/macOS binaries and SHA256
   checksums;
3. **release** — creates the GitHub Release with auto-generated notes and the
   binaries attached.

### Releasing a new version

1. Tag and push — the release workflow runs on the tag:
   ```bash
   git tag v1.2.3
   git push origin v1.2.3
   ```
2. The **image** job publishes
   `ghcr.io/OWNER/carteiro:v1.2.3` (plus `latest`); the **release** job opens
   the GitHub Release with the binaries attached.
3. Or trigger it manually (**Actions → release → Run workflow**) with an
   already-existing tag in the `tag` input.

Required permission in `Settings → Actions → General → Workflow permissions →
Read and write`: `packages: write` (GHCR) and `contents: write` (Release) —
both are declared at the top of the workflow.

Tests cover the database layer (accounts hashing/upserts, DKIM, queue
lifecycle with leases and dead-letter), end-to-end SMTP sessions against a
real client, delivery against an in-memory fake MX (success, 5xx, 4xx, DKIM
from the database) and the full admin API surface.

---

## Limitations

Deliberately simple project:

- **No inbound email**: outbound relay only; bounces do not come back.
- **No UI**: config file/env for startup seeds and API tokens, admin REST API
  for everything else, Prometheus metrics for monitoring.
- **SQLite is single-writer**: one instance per database file; use MySQL when
  you need several instances sharing the queue.
- **No per-account rate limiting** (add it on the client or at a gateway).
- **At-least-once delivery**: a tiny duplication window if a crash happens
  between the MX accepting the message and the progress commit.
- **Opportunistic TLS** to destination MX servers (like most MTAs): STARTTLS
  when available, plain-text fallback otherwise.

## License

MIT — use, study, modify.