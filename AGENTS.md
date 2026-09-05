# AGENTS.md — guidance for AI agents working on Carteiro

Carteiro is a **database-backed SMTP relay**: clients (Nodemailer, etc.)
authenticate on port 587 and Carteiro queues the message in a database and
delivers it to the recipients' MX servers. Accounts, DKIM keys and the queue
all live in a database, managed over the air through an admin REST API.

## How the human wants the project

- **Everything in English**: code identifiers, comments, docs, logs, error
  messages, README, config examples. (The *conversation* with the user is in
  Portuguese; artifacts are English.)
- **Simple and pragmatic over hardened**: the user accepted running the
  container as root and keeping the API token in plain text in config/env —
  reasoning: the worst an attacker can do is send emails, and anyone with
  server access can change everything anyway.
- **No file-based secrets for configuration**: DKIM keys and TLS
  certificates are configured **only as base64** (YAML or env). Filesystem
  paths were removed everywhere (TLS files, then DKIM env/file paths) in
  favour of base64: one variable per secret, no mounted files to read.
- **Over the air (OTA) management**: accounts and DKIM domains are added via
  the admin API while the server runs; seeds are only for first boot.
- **Answers in Portuguese, artifacts English**, rich Markdown when verbose.

## Architecture

```
cmd/carteiro/         main: flags, wiring, seed upserts, signals
internal/config/      YAML + CARTEIRO_* env + per-OS defaults (sqlite path)
internal/storage/     database layer: sqlite (default) + mysql drivers
                      accounts (bcrypt), dkim_keys (PEM text), queue
internal/smtpd/       SMTP submission server (:587, AUTH, DATA, STARTTLS)
internal/relay/       MX delivery worker (retry/backoff, dead-letter)
internal/dkim/        DKIM signing (RSA/Ed25519 via go-msgauth)
internal/api/         web server: admin REST API + dashboard routes (bearer),
                      openapi.json, SPA mount
internal/acme/        managed Let's Encrypt TLS (lego): obtain/renew, dynamic cert
internal/sends/       recent-sends ring buffer + RFC 5322 build/parse helpers
internal/webui/       embedded SPA handler over web/dist (go:embed)
internal/metrics/     atomic Prometheus counters
web/                  React + Vite + Tailwind dashboard (src -> dist, embedded)
systemd/              example unit (StateDirectory=/var/lib/carteiro)
scripts/gen-envs.sh   interactive env generator (DKIM/TLS/accounts) for Coolify
DNS.md                full DNS guide (SPF/DKIM/DMARC/PTR, Cloudflare notes)
.github/workflows/    single release.yml (image + binaries + release)
```

Package rules: `config` is pure config (never imports storage/smtpd);
`smtpd` authenticates against `storage.Store`; `relay` only talks to
`storage.Store` + MX; `sends` depends on nothing internal (stdlib only);
`api` talks to `storage.Store` + `sends` + `webui`; `smtpd`/`relay` push
lifecycle events into the optional `sends.Recorder`. No global mutable state
besides the API's own metrics; the config snapshot is read once at startup
(no SIGHUP reload; OTA changes go through the DB/API).

## Conventions

- **English** for every identifier, comment, log, error and doc string.
- Standard library first; current external deps: `gopkg.in/yaml.v3`,
  `modernc.org/sqlite`, `go-sql-driver/mysql`, `golang.org/x/crypto` (bcrypt),
  `emersion/go-msgauth` (DKIM), and their transitive deps.
- Doc comments on exported identifiers; keep code comments to "why", not
  "what". Errors are lower-case, no trailing punctuation.
- **Do not downgrade the Go toolchain**: `go.mod` requires **go 1.26**
  (current deps demand it); the Docker build stage must stay on
  `golang:1.26-alpine`. `x/crypto` must stay ≥ 0.56 or builds break on 1.25.
- Commit messages in English, why-focused (see git history: initial commit +
  "Make TLS base64-only and expand DNS/key documentation").

## Configuration model

Precedence: **defaults < YAML file < CARTEIRO_* env < --config flag**. Config
file lookup: `/etc/carteiro/config.yaml` (Linux), `~/.config/carteiro`,
`~/Library/Application Support/carteiro` (macOS), or `CARTEIRO_CONFIG`/flag.

Key rules (validated in `config.normalizeAndValidate`):

- Storage: `storage.type: sqlite|mysql`; sqlite file defaults to the OS data
  dir (`/var/lib/carteiro/carteiro.db` as root/systemd); mysql needs a DSN.
- **Accounts are optional seeds**, created/updated on every boot (see Seeds).
- **DKIM (YAML)**: `key_data` is base64 of the PEM, decoded at load; env has
  a single variable, `CARTEIRO_DKIM_KEYS` =
  `"doma.com:mail:<b64>;domb.com:sel:<b64>"` (base64 of each whole PEM; no
  singular DKIM env vars, no file paths). Env overrides YAML for the same
  domain.
- **TLS is base64-only**: `tls.cert_data`/`key_data` in YAML,
  `CARTEIRO_TLS_CERT`/`CARTEIRO_TLS_KEY` in env. No `cert_file`/`key_file`
  anywhere; leftover file env vars are ignored. Pair is validated with
  `tls.X509KeyPair` at load.
- **API token is a single plain-text token** (`api.token` /
  `CARTEIRO_API_TOKEN`), never stored in the DB. Admin API + web panel are
  off without it. API listen defaults to loopback `127.0.0.1:9090`; the web
  panel (enabled whenever the API is) defaults to `:8080`. Listen values
  accept a bare port (`8080`), normalized to `:8080` by
  `config.normalizeListen`. `CARTEIRO_WEB_LISTEN`/`HTTP_ADDR` configure the
  PANEL; `CARTEIRO_API_LISTEN` configures the API.
- **Managed TLS (ACME)** (`CARTEIRO_ACME=true` or `-acme` flag, off by
  default): `internal/acme` uses lego to obtain/renew a Let's Encrypt
  certificate for `CARTEIRO_HOSTNAME` using ONLY the http-01 challenge (no
  DNS provider or API keys), served on `CARTEIRO_ACME_HTTP_ADDR` (`:80`);
  the account and certificate persist in the
  DB (`acme_account`, `managed_cert`, single-row) and the SMTP listener
  serves them dynamically through `smtpd.UseManagedTLS` (no restart on
  renewal). Enabled ignores `tls.cert_data/key_data`. Keep it off when a
  proxy in front terminates TLS.
- Listen addresses (`listen`, `api.listen`, `CARTEIRO_LISTEN`,
  `CARTEIRO_API_LISTEN`/`HTTP_ADDR`) accept a bare port (`8080`), which
  `config.normalizeListen` rewrites to `:8080`; validate with host:port.
- Delivery/queue tunables exist in YAML and env: `CARTEIRO_DELIVERY_*`
  (connect/io timeouts, retry base/max, max attempts, poll interval,
  concurrency) and `CARTEIRO_QUEUE_DEAD_MAX` (`queue.dead_max`, default 1000,
  0 = unlimited, oldest dead rows pruned).

## Storage / database

- One schema, two drivers (`sqlite` sets WAL + `busy_timeout`, single
  connection; `mysql` uses its DSN). Migrations: versioned DDL list applied
  in `schema_migrations`, additive only.
- Tables: `accounts(email PK, password_hash bcrypt, allowed_from JSON)`,
  `dkim_keys(domain PK, selector, key_data PEM text)`,
  `queue_messages(id PK, sender, to_json, data BLOB, attempts, next_attempt
  ms, status queued|dead, lease_until, worker_id, created_at, last_error,
  permanent_json)`.
- Queue is **at-least-once**: `NextDue` claims rows with a 30-minute lease
  (`lease_until`); after a crash the row becomes due again. Keep the
  claim→persist→release→succeed/dead-letter flow intact.
- `deadMax` prune runs after DeadLetter; do not prune rows that are not
  `status='dead'`.
- Passwords: `HashPassword`/`VerifyPassword` (bcrypt) live in
  `internal/storage/accounts.go`; never store or return plain passwords.

## Seeds (startup upserts)

Every boot upserts YAML/env accounts and DKIM keys with **clear per-entry
logs**: `created`, `updated (new password or allowed_from)`, `unchanged`
(logged at debug). DKIM upsert compares domain+selector+key text so restarts
report `unchanged`. Seeds never delete rows — runtime changes belong to the
API. Keep the logSeed format and the idempotence when editing.

## Logging (logmask)

`CARTEIRO_LOG_MASK_EMAILS` (`log_mask_emails`, **default `true`**) masks
e-mail addresses in log messages and attributes via `internal/logmask`
(first char + `***` per label, public suffix kept). The mask is applied
centrally in `main` with `logmask.NewLogger`, so every package logging
through the shared logger is covered (seeds, smtpd, relay, api). Set it to
`false` only for debugging with full addresses. Rule: strings without `@` are
never altered. When adding a new log line with addresses, no per-site change
is needed; keep log values as plain strings/slices so the handler can reach
them.

## SMTP behavior (smtpd)

- Banner `220 <hostname> ESMTP Carteiro`; EHLO advertises PIPELINING,
  8BITMIME, SIZE, STARTTLS (when TLS configured), AUTH PLAIN LOGIN (when
  allowed).
- AUTH: login is the account email; verify bcrypt hash from the DB. With
  `require_tls`, AUTH outside loopback requires STARTTLS (538 otherwise).
- MAIL FROM must equal the account email or an `allowed_from` entry (553
  otherwise); envelope addresses are lowercased. SIZE param > max → 552.
- DATA is streamed with dot-unstuffing, capped at `max_message_size`
  (over-limit consumes to the terminator and replies 552). After the store
  INSERT commits: `250 2.0.0 Ok: queued as <id>`; a Received header carries
  the queue id (`storage.NewID`).
- Keep the last EHLO reply line without a `-` suffix (`250 `), or Go's
  `net/smtp` clients hang.

## Delivery (relay)

- `Run` polls the store + wakes on `Notify`; per-domain delivery batches,
  opportunistic STARTTLS with certificate verification and plain-text
  fallback, per-recipient permanent (5xx) drops recorded in `permanent`,
  transient (4xx/network) rescheduled with exponential backoff until
  `max_attempts`, then `dead` with reason. Metrics counters on attempts,
  delivered, dead.
- DKIM: sign when the envelope-from domain has a key in the DB; signing
  failure logs and sends unsigned (never blocks delivery).

## Web server: API, dashboard endpoints, SPA

- TWO HTTP listeners share the binary: the admin API (`api.listen`, loopback
  `127.0.0.1:9090` default) and the web panel (`web.listen`, `:8080` default;
  env `CARTEIRO_WEB_LISTEN`/`HTTP_ADDR`). `api.NewPanel` serves the SPA on the
  web port and proxies `/api/*` to the API listener in-process
  (`api.APITargetURL` derives the target); the API listener is API-only and
  never returns the SPA. Routes are registered from a single table in
  `api.registerRoutes` (call it with prefix "" + legacy=true for the root
  aliases and with "/api" + legacy=false for the canonical set). The root alias exists ONLY for
  pre-dashboard paths that are not React pages (`/health`, `/metrics`,
  `/openapi.json`, `/dkim`, `/queue`); `/accounts` and the dashboard
  endpoints are `/api`-only because their root paths (`/accounts`, `/sends`,
  `/sends/{id}`, `/stats`, `/send`) are SPA client routes — never give an
  API route a root path that shadows a page or direct reloads/refreshes of
  the panel start answering API JSON. Public: `/api/health`,
  `/api/metrics`, `/api/openapi.json`. Everything else (including
  `/api/stats`, `/api/sends`, `/api/send`) requires the bearer token.
- Dashboard-specific endpoints: `GET /api/stats` (counters + queue +
  version + uptime), `GET /api/sends?limit=` and `GET /api/sends/{id}`
  (recent sends with parsed `html`/`text`/`raw` for rendering),
  `POST /api/send` (compose into the queue: `{"from","to":[],"subject",
  "text","html"}`; `from` must be allowed by an account — same rule as SMTP
  MAIL FROM) and `PATCH /api/accounts/{email}` (edit allowed senders and/or
  reset the password; empty/omitted password keeps the current hash). The
  dashboard's Accounts screen uses the PATCH endpoint; `GET/POST /api/accounts`
  only list/create. Composed messages go through `storage.EnqueueWithID`
  with a queue id in `Message-ID`.
- The recent-sends feed is an **in-memory ring** (`internal/sends`, wired in
  `main` as `sends.New(200, 512<<10)`): `smtpd` records on enqueue (Add
  BEFORE the DB insert, `Drop` on failure), `relay` updates attempts/
  delivered/dead, the retry endpoint re-queues. The database queue remains
  the source of truth; the ring resets on restart and stores no credentials.
- SPA: `web/fs.go` embeds `web/dist`; `internal/webui` serves files, falls
  back to `index.html` for extension-less GETs and never swallows `/api/*`
  (unknown API routes 404). `//go:embed` needs `web/dist` to exist — the
  committed placeholder (`.gitkeep` + `index.html`) keeps `go build`/
  `go test` green without Node; `make build`, Docker and CI always build the
  real UI first.
- Responses never leak `password_hash`, token or DKIM key text.
- The OpenAPI JSON lives in `internal/api/openapi.json` (go:embed) and is
  used by Swagger UI; update it whenever endpoints/schemas change.

## Docker, containers and releases

- `Dockerfile`: `node:22-alpine` UI stage (`npm ci && npm run build`), then
  static binary (CGO_ENABLED=0) on `golang:1.26-alpine` copying
  `web/fs.go` + the UI `dist` before compiling, `alpine:3.20` runtime with
  ca-certificates/tzdata/busybox-extras (for the `nc` healthcheck), runs
  **as root** (binds 587 < 1024 and writes volumes), `HEALTHCHECK` = TCP
  probe on 587, `EXPOSE 587 8080 9090`. No inline comments after EXPOSE (Apple's
  builder rejects them).
- Image: `ghcr.io/itxtoledo/carteiro`. One workflow
  (`.github/workflows/release.yml`) per `v*` tag: multi-arch image
  (amd64+arm64, also tagged `latest`) + static binaries for linux/darwin
  (amd64/arm64) + GitHub Release with checksums. Permissions needed:
  `packages: write`, `contents: write`.
- Apple container runtime (`container` CLI): builder DNS may need
  `--dns 8.8.8.8`; volumes are root-owned with `lost+found` — running as root
  sidesteps it; HEALTHCHECK metadata is not surfaced by the Apple runtime
  (probe via `container exec` instead).

## Testing

- `make fmt vet test` / `gofmt -w cmd internal`, `go vet ./...`,
  `go test ./...`.
- Storage tests open SQLite in `t.TempDir()`; relay tests use the in-memory
  fake MX and the `lookupMX`/`smtpPort` package-level seams (restore them in
  test cleanup). SMTP tests drive a real `net/smtp` client; note Go's
  `PlainAuth` refuses plaintext unless TLS or the server name matches —
  tests dial `127.0.0.1` explicitly.
- Test names and failure messages are English. Run the whole suite after any
  change to config/storage/smtpd/relay/api/sends; the packages are coupled
  through the config model. `sends` tests cover the build/parse round trip
  (RFC 2047 words, quoted-printable, latin-1) and the recorder ring.
- `gofmt -w cmd internal web` (web/fs.go is Go). Never reformat `web/src`.
- Changing a constructor signature touches `main` and the package tests
  (`smtpd.New`, `relay.New`, `api.New` all take the recorder; `api.New` also
  takes version + size limits).

## Gotchas learned

- Backslash-heavy text (PEM, `\n`, YAML in heredocs) is easy to corrupt when
  patching files programmatically — prefer whole-file writes or exact `edit`
  with fresh reads; verify with `gofmt`/build right away.
- `config.go` was once corrupted by index-splice edits that removed whole
  functions — when editing it, prefer `lsp_replace_symbol`/`edit` on fresh
  reads over blind index surgery.
- `go 1.26` in `go.mod` is load-bearing (modernc + x/crypto); never revert it
  without also pinning older deps.
- Docker/Apple builder: inline `#` comments after `EXPOSE`/`RUN` values and
  trailing-comment lines can fail the parse.
- Base64 everywhere: values with spaces/newlines are tolerated (whitespace is
  stripped before decoding), so `base64 -w0` output and wrapped output both
  work.
- `web/dist/` is gitignored except the committed placeholder
  (`.gitkeep`, `index.html`). Never delete the folder: `go:embed` fails at
  compile time without it, and the placeholder is what keeps Node-free
  `go build`/`go test` working.

## Remote / workflow

- Remote: `origin` → `github.com:itxtoledo/carteiro.git` (branch `main`).
- User explicitly asks to "send to remote" when they want commit+push;
  commit messages are English and why-focused with the Crush attribution
  footer, then `git push origin main`.
