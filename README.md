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
                                    |  +- admin API :9090 (bearer)         |
+------------+   HTTPS + Bearer     |  +- sqlite / mysql                   |
| your ops / | ----- /accounts -----►                                     |
| portal     |      /dkim /queue    |                                      |
+------------+                      +--------------------------------------+
```

## Table of contents

1. [How it works](#how-it-works)
2. [Configuration](#configuration)
3. [Seeds: first run, upserts and clear logs](#seeds-first-run-upserts-and-clear-logs)
4. [Admin API (bearer) and monitoring](#admin-api-bearer-and-monitoring)
5. [Ports and network](#ports-and-network)
6. [Docker mode](#docker-mode)
7. [Daemon mode (Linux/macOS)](#daemon-mode)
8. [Sending with Nodemailer](#sending-with-nodemailer)
9. [The database queue](#the-database-queue)
10. [DNS: keeping email out of spam](#dns-keeping-email-out-of-spam)
11. [TLS on the submission port](#tls-on-the-submission-port)
12. [Development](#development)
13. [Limitations](#limitations)

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

### Admin API tokens (plain text, config only)

```yaml
api:
  listen: "127.0.0.1:9090"
  token: "a-long-random-token"
```

The token is **deliberately not stored in the database** — it lives only here
(or in `CARTEIRO_API_TOKEN`), because only the server owner sees it. The API
stays **off** until the token is configured.

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `CARTEIRO_LISTEN` | `:587` | submission server address |
| `CARTEIRO_HOSTNAME` | machine hostname | name used in EHLO/banner (should match the PTR) |
| `CARTEIRO_STORAGE_TYPE` | `sqlite` | `sqlite` or `mysql` |
| `CARTEIRO_SQLITE_PATH` | OS default | SQLite database file |
| `CARTEIRO_DB_DSN` | — | MySQL DSN (implies `type: mysql`) |
| `CARTEIRO_API_LISTEN` | `127.0.0.1:9090` | admin API address (off without a token) |
| `CARTEIRO_API_TOKEN` | — | single bearer token (enables the admin API) |
| `CARTEIRO_ACCOUNTS` | — | seed accounts `email:password` separated by `;` |
| `CARTEIRO_DKIM_DOMAIN` / `SELECTOR` | —/`mail` | seed DKIM for one domain |
| `CARTEIRO_DKIM_KEY` / `KEY_FILE` | — | seed DKIM key: inline PEM (or `\n`, or base64) / file path |
| `CARTEIRO_DKIM_KEYS` | — | several DKIM seeds at once: `doma.com:mail:keyA;domb.com:selB:keyB` (`;` separated) |
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
| `CARTEIRO_TLS_CERT_FILE` / `CARTEIRO_TLS_KEY_FILE` | — | enable TLS (cert + key, both required) |
| `CARTEIRO_TLS_MODE` | `starttls` | `starttls` (587) or `implicit` (465) |
| `CARTEIRO_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
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

---

## Admin API (bearer) and monitoring

The API runs on its own port (default `127.0.0.1:9090`), **off** until a token
is configured. Every call except `/health` and `/metrics` needs
`Authorization: Bearer <token>`.

| Endpoint | Description |
|---|---|
| `GET /health` | liveness (no auth) |
| `GET /metrics` | Prometheus metrics (no auth) |
| `GET /accounts` | list accounts (emails + allowed_from, never hashes) |
| `POST /accounts` | create/update: `{"email","password","allowed_from":[]}` → 201/200 |
| `DELETE /accounts/{email}` | remove an account |
| `GET /dkim` | list domains + selectors (never the private key) |
| `POST /dkim` | create/update: `{"domain","selector","key_data"}` (PEM) → 201/200 |
| `DELETE /dkim/{domain}` | remove a DKIM key |
| `GET /queue/stats` | `{"queued":N,"due":N,"dead":N}` |
| `GET /queue?status=dead` | list queued/dead messages (no bodies) |
| `POST /queue/{id}/retry` | move a dead message back to the queue (attempts reset) |
| `GET /openapi.json` | OpenAPI 3 document (no auth) — point Swagger UI at it |

```bash
TOKEN="a-long-random-token"
BASE="http://127.0.0.1:9090"

# add a new project account over the air (password hashed server side)
curl -X POST "$BASE/accounts" -H "Authorization: Bearer $TOKEN" -d '{
  "email": "project-x@yourdomain.com",
  "password": "s3cr3t",
  "allowed_from": ["news@yourdomain.com"]
}'

# add its DKIM domain
curl -X POST "$BASE/dkim" -H "Authorization: Bearer $TOKEN" -d "{
  \"domain\": \"yourdomain.com\", \"selector\": \"mail\",
  \"key_data\": \"$(sed -z 's/\n/\\n/g' /etc/carteiro/dkim.key)\"
}"

# monitoring
curl "$BASE/queue/stats" -H "Authorization: Bearer $TOKEN"
curl "$BASE/queue?status=dead" -H "Authorization: Bearer $TOKEN"
curl -X POST "$BASE/queue/20260903T120000.000000000Z-abc/retry" -H "Authorization: Bearer $TOKEN"

# Prometheus (no auth; scrape only from trusted networks)
curl "$BASE/metrics"
```

The OpenAPI document (`GET /openapi.json`) describes every endpoint, schema
and the bearer security scheme; point a Swagger UI at it, e.g.
<https://petstore.swagger.io?url=http://127.0.0.1:9090/openapi.json>.

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
| **9090** | inbound (admin) | admin REST API, `/health`, `/metrics`, `/openapi.json` | `api.listen` (default `127.0.0.1:9090`, **off** without `api.token`) |
| **25 (outbound)** | outbound (Carteiro → internet) | Carteiro **connects to** the MX servers of recipients; it never listens on 25 | none — but the host/container must be allowed to open outbound port 25 (see the DNS section) |

Practical rules:

- **587** is the only port your applications need. Publish it **only** on a
  private network/VPN, or require TLS (`require_tls: true` + the `tls` block);
  never expose plain-text AUTH on the public internet.
- **9090** binds to `127.0.0.1` by default. Inside Docker that means it is
  reachable only within the container: to reach it from the host, either run
  with host networking or set `CARTEIRO_API_LISTEN=":9090"` **and** keep the
  port firewalled (or reach it over a VPN / SSH tunnel).
- Changing the SMTP port is just `CARTEIRO_LISTEN=":2525"` (then point
  Nodemailer at 2525); there is no magic in 587/465 besides convention.


---

## Docker mode

Images are published automatically to **GHCR** by the
[GitHub Actions workflow](#development) for `linux/amd64` and `linux/arm64`:

```bash
docker pull ghcr.io/YOUR-USERNAME/carteiro:latest
```

The image is **configured entirely through environment variables**: it ships
with no config file, so a minimal usable server needs only two vars
(`CARTEIRO_ACCOUNTS` + `CARTEIRO_API_TOKEN`) — everything else has a
sane default. A YAML config can still be mounted at `/etc/carteiro/config.yaml`
if you prefer, and YAML values are overridden by env vars.

### Quick example with environment variables

```bash
docker run -d --name carteiro \
  -p 587:587 \
  -p 9090:9090 \
  -e CARTEIRO_ACCOUNTS='sender@yourdomain.com:a-strong-password' \
  -e CARTEIRO_API_TOKEN='a-long-random-token' \
  -e CARTEIRO_API_LISTEN=':9090' \   # reach the admin API from the host (loopback by default)
  -v carteiro-data:/var/lib/carteiro \
  ghcr.io/YOUR-USERNAME/carteiro:latest
```

Follow the logs with `docker logs -f carteiro` — you will see the seed
upsert lines on startup. If you only need the SMTP relay (no API), drop the
`-p 9090:9090`, `CARTEIRO_API_TOKEN` and `CARTEIRO_API_LISTEN` lines.

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
  ghcr.io/YOUR-USERNAME/carteiro:latest

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
    image: ghcr.io/YOUR-USERNAME/carteiro:latest
    restart: unless-stopped
    ports: ["587:587", "9090:9090"]
    environment:
      CARTEIRO_ACCOUNTS: "sender@yourdomain.com:password"
      CARTEIRO_API_TOKEN: "a-long-random-token"
      CARTEIRO_API_LISTEN: ":9090"   # loopback inside the container otherwise
    volumes:
      - carteiro-data:/var/lib/carteiro
volumes:
  carteiro-data:
```

```bash
docker compose up -d
```

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
domains include the same outbound IP (section 9 below). The environment DKIM
variables configure a **single** domain; for several domains use the YAML
`dkim` list or the admin API. For hard isolation (separate database, own
limits), run one container/daemon per project with its own
`CARTEIRO_SQLITE_PATH` volume.

---

## DNS: keeping email out of spam

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

```dns
yourdomain.com.  TXT  "v=spf1 ip4:203.0.113.10 -all"
```

If you also send over IPv6, add `ip6:`. If other legitimate services send for
the domain, include them (`include:`), but **never** use `+all`.

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

```bash
openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
  -keyout /etc/carteiro/tls.key -out /etc/carteiro/tls.crt \
  -subj "/CN=smtp.yourdomain.com" \
  -addext "subjectAltName=DNS:smtp.yourdomain.com"
```

```yaml
tls:
  cert_file: "/etc/carteiro/tls.crt"
  key_file:  "/etc/carteiro/tls.key"
  mode: "starttls"          # port 587 (default). For 465: "implicit"
require_tls: true           # refuse AUTH without TLS outside loopback
```

---

## Development

```bash
make build       # builds bin/carteiro
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
internal/api/         admin REST API (bearer) + queue monitoring
internal/metrics/     Prometheus counters
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
