# mailserver

A self-hosted SMTP + IMAP mail server with a built-in webmail UI, written in
Go, deployable via Docker Compose behind Traefik. Phase 1: inbound SMTP,
authenticated submission with outbound relay/retry, DKIM signing, and a full
IMAP server backed by standard Maildir storage. Phase 2: a server-rendered
webmail client on top of that same storage. Phase 3: inbound SPF/DKIM/DMARC
verification, reverse-proxy-aware brute-force protection, and Prometheus
metrics/health checks.

Built on `emersion/go-smtp`, `emersion/go-imap/v2`, `emersion/go-msgauth`
(DKIM/DMARC), `blitiri.com.ar/go/spf`, `emersion/go-sasl`, `go-acme/lego`
(ACME DNS-01), `pires/go-proxyproto`, `prometheus/client_golang`, and htmx
for the webmail UI — no custom crypto, TLS, or protocol parsing.

## Architecture

- **Inbound SMTP** (port 25): accepts mail for configured local domains,
  delivers to Maildir. No relaying, no auth.
- **Submission SMTP** (port 587, STARTTLS + AUTH PLAIN): accepts mail from
  authenticated local users; delivers local recipients directly, queues
  everything else for outbound relay.
- **Outbound relay**: a disk-backed queue (`storage.queue_path`) with
  exponential-ish backoff retries; permanent failures bounce back into the
  sender's own INBOX.
- **IMAP** (port 993, implicit TLS): LOGIN, LIST, SELECT, FETCH, STORE,
  SEARCH, APPEND, EXPUNGE, COPY/MOVE, IDLE.
- **Webmail** (plain HTTP, meant to sit behind Traefik): login, folder
  sidebar, paginated message list, reading pane, compose with
  reply/reply-all/forward, basic search. It talks to the exact same
  in-process mailbox objects the IMAP server uses (see
  `internal/imapbackend/webapi.go`) rather than opening its own IMAP/SMTP
  connection, so it and any concurrently connected IMAP client always agree
  on mailbox state.
- **Storage**: standard Maildir (`tmp`/`new`/`cur`, Maildir++ dot-folders)
  on disk; a small SQL index (SQLite or Postgres) tracks IMAP UIDs and the
  user/domain directory (bcrypt password hashes).
- **TLS**: a mounted cert/key pair, or automatic ACME DNS-01 (Cloudflare)
  when ports 80/443 belong to a reverse proxy in front of this host.
- **Inbound verification**: SPF, DKIM, and DMARC are checked on every
  inbound message (not submission — that's already authenticated) and the
  result is surfaced as an `Authentication-Results` header; nothing is
  rejected based on it, this phase only annotates.
- **Brute-force protection**: failed-attempt rate limiting on SMTP AUTH,
  IMAP LOGIN, and webmail login, each with its own limiter. Real-client-IP
  resolution is reverse-proxy-aware: an address or header is only trusted
  from `security.trusted_proxies` — see "Hardening" below for why that
  matters.
- **Observability**: structured JSON logs (`log/slog`) throughout, plus a
  `/healthz` and Prometheus `/metrics` endpoint on their own listener
  (`listen.metrics`).

## Requirements

- A VPS with a static IP, ports 25/587/993 reachable from the internet.
- A domain you control, able to add DNS records (MX, TXT, A).
- Docker + Docker Compose.
- Traefik (or another reverse proxy) already running on the same VPS,
  attached to a Docker network this compose file can join, if you want the
  webmail UI reachable over HTTPS — see "Webmail" below.
- Your VPS provider's reverse DNS (PTR) for the VPS IP set to your mail
  hostname (e.g. `mail.example.com`) — most receiving MTAs treat mail from a
  host with no/mismatched PTR as spam. This is set with your hosting
  provider, not in your own DNS zone.

## First-run setup

1. Copy the example config and edit it:

   ```sh
   cp configs/config.example.yaml config.yaml
   ```

   At minimum set `hostname`, `domains`, and `admin.email`/`admin.password`
   (the admin account is only bootstrapped once, on first startup with an
   empty database — change the password afterwards via IMAP or by recreating
   the user). Alternatively, leave those four blank in `config.yaml` and set
   `MAILSERVER_HOSTNAME` / `MAILSERVER_DOMAINS` / `MAILSERVER_ADMIN_EMAIL` /
   `MAILSERVER_ADMIN_PASSWORD` as environment variables instead (see
   `.env.example`) — useful if you'd rather manage secrets through your
   platform's environment-variables UI than a committed file. Env vars
   always win over `config.yaml` for the settings both can set.

2. Build and start:

   ```sh
   docker compose build
   docker compose up -d
   docker compose logs -f mailserver
   ```

   On first run the server generates a DKIM key pair and prints the DNS TXT
   record to publish (see below) — grab it from the logs, or re-print it
   anytime with:

   ```sh
   docker compose exec mailserver mailserver dkim show --config /etc/mailserver/config.yaml
   ```

3. Add the DNS records below, then send yourself a test message from an
   external account to confirm inbound delivery, and from a mail client
   configured against the submission port to confirm outbound relay.

## DNS records needed

Replace `example.com` and `mail.example.com` with your domain and the
`hostname` from your config.

| Type | Host | Value | Notes |
|---|---|---|---|
| A | `mail.example.com` | your VPS IP | matches `hostname` in config.yaml |
| MX | `example.com` | `10 mail.example.com.` | tells senders where to deliver |
| TXT | `example.com` | `v=spf1 mx ~ all` (no space) | SPF: only `mail.example.com` (the MX) may send as `example.com` |
| TXT | `<selector>._domainkey.example.com` | printed by `mailserver dkim show` | DKIM public key; `<selector>` is `dkim.selector` in config (default `default`) |
| TXT | `_dmarc.example.com` | `v=DMARC1; p=quarantine; rua=mailto:postmaster@example.com` | DMARC policy; start with `p=quarantine`, move to `p=reject` once confident |

Notes:

- If you host multiple domains on one server, repeat the MX/SPF/DMARC rows
  per domain; DKIM signing uses the same key/selector for every domain (one
  DNS TXT record per domain, same value).
- These records mainly affect how *other* servers treat mail *you* send.
  This server also verifies them on *inbound* mail (see "Hardening" below)
  but only annotates the result — it doesn't reject based on it.
- DNS propagation can take minutes to hours. Don't flip DMARC to
  `p=reject` until you've confirmed signed mail passes at the receiving end
  (e.g. by sending to a Gmail account and checking "Show original").

## Creating mailboxes

```sh
docker compose exec mailserver mailserver user create \
  --config /etc/mailserver/config.yaml \
  --email alice@example.com \
  --password 'a strong password' \
  [--admin]
```

This creates the user (bcrypt-hashed password) and its standard folders
(INBOX, Sent, Drafts, Trash, Junk) if they don't already exist. The domain
must already be listed in `config.yaml`'s `domains`. Point any IMAP/SMTP
client (Thunderbird, etc.) at your `hostname` — IMAP on 993 (SSL/TLS), SMTP
on 587 (STARTTLS) — using the full email address as the username.

## TLS

Pick one in `config.yaml`:

- **Mounted cert/key** (`tls.cert_file` / `tls.key_file`): simplest if you
  already have a certificate (e.g. issued by Traefik for another purpose,
  or a wildcard cert you manage separately). Takes priority if both are set.
- **ACME DNS-01** (`tls.acme`): used when ports 80/443 belong to Traefik, so
  HTTP-01/TLS-ALPN-01 aren't available to this process. Currently supports
  Cloudflare; set `tls.acme.provider: cloudflare` and put a scoped API token
  (Zone:Read + DNS:Edit on the relevant zone) in
  `tls.acme.options.CLOUDFLARE_DNS_API_TOKEN`. Set `tls.acme.staging: true`
  while testing to avoid Let's Encrypt's production rate limits, then flip
  it off. Certificates and the ACME account are persisted under
  `tls.acme.cache_dir` (part of the `/data` volume) and renewed
  automatically starting 30 days before expiry.
- **Neither configured**: falls back to a self-signed certificate generated
  at startup, logged clearly as development-only. Mail clients and other
  MTAs won't trust it — fine for local testing, not for production.

## Webmail

The webmail UI listens on plain HTTP (`listen.webmail`, default `:8080`)
and is meant to be reached only through Traefik, which terminates TLS —
never publish this port directly to the internet the way 25/587/993 are.

`docker-compose.yml` attaches the `mailserver` service to an **external**
Docker network (compose-file key `traefik`, actual network name controlled
by the `TRAEFIK_NETWORK` env var, default `traefik` — create it once with
`docker network create traefik`, and make sure your separate Traefik stack
is attached to the same network) and carries example Traefik labels that
read `MAILSERVER_HOSTNAME` for the routing rule:

```yaml
labels:
  - traefik.enable=true
  - traefik.docker.network=${TRAEFIK_NETWORK:-traefik}
  - traefik.http.routers.webmail.rule=Host(`${MAILSERVER_HOSTNAME:-mail.example.com}`)
  - traefik.http.routers.webmail.entrypoints=websecure
  - traefik.http.routers.webmail.tls.certresolver=letsencrypt
  - traefik.http.services.webmail.loadbalancer.server.port=8080
```

Set `MAILSERVER_HOSTNAME` (see `.env.example`) and adjust the entrypoint
name / cert resolver to match your own Traefik setup, then visit
`https://<hostname>/` and sign in with any mailbox created via
`mailserver user create`. (Deploying via Dokploy instead? See the next
section — Dokploy's Traefik uses a different network name and you'll want
one extra setting for correct rate limiting.)

Sessions are in-memory (a restart signs everyone out) and the session
cookie relies on `SameSite=Strict` rather than a separate CSRF token —
reasonable given the UI only reaches the mail server in-process and sits
behind Traefik's TLS termination. Login attempts are rate-limited per
client IP (resolved via `X-Forwarded-For`, trusted only from
`security.trusted_proxies` — see "Hardening" below).

## Deploying with Dokploy

This repo works as a Dokploy "Compose" application — you point Dokploy at
the repo, it builds `Dockerfile` and runs `docker-compose.yml` as-is,
behind the Traefik instance Dokploy already runs for you.

1. **Create the app.** In Dokploy: New Project → Application → Compose,
   pointed at this repo/branch. Dokploy will find `docker-compose.yml`
   automatically.

2. **Set environment variables** (Dokploy's Environment tab, or copy
   `.env.example` to `.env` and let compose read it — either way the
   container sees the same variables):

   ```
   MAILSERVER_HOSTNAME=mail.yourdomain.com
   MAILSERVER_DOMAINS=yourdomain.com
   MAILSERVER_ADMIN_EMAIL=you@yourdomain.com
   MAILSERVER_ADMIN_PASSWORD=<a strong password>
   TRAEFIK_NETWORK=dokploy-network
   ```

   These override the matching settings in `config.yaml` (see "First-run
   setup" — you still need a `config.yaml` for everything else: storage
   paths, DKIM, queue backoff; copy `configs/config.example.yaml` into the
   repo, or use Dokploy's file-mount/"Volumes" feature to provide it at
   `/etc/mailserver/config.yaml` without committing it).

3. **Set `MAILSERVER_TRUSTED_PROXIES` to Dokploy's Traefik subnet.** This
   is the one setting that actually matters for correctness, not just
   convenience: Dokploy's Traefik sits in front of the webmail login, so
   without this, every webmail user would appear to come from Traefik's
   own address, and the per-IP rate limiter (see "Hardening") would lock
   real users out of each other's attempts instead of tracking them
   individually. Find the subnet with:

   ```sh
   docker network inspect dokploy-network --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}'
   ```

   and set `MAILSERVER_TRUSTED_PROXIES=<that subnet>` (a CIDR like
   `10.0.1.0/24`). Leave SMTP/IMAP alone — Dokploy's Traefik never sits in
   front of those raw TCP ports, so they don't need this.

4. **Confirm ports 25/587/993 publish on the host.** Dokploy Compose apps
   generally respect the `ports:` section of the compose file directly, but
   versions differ in whether raw TCP port publishing needs to also be
   confirmed/enabled in the app's own Domains/Ports tab — check there if
   inbound mail isn't reaching the container after deploying.

5. **Webmail HTTPS**: the Traefik labels already in `docker-compose.yml`
   (parameterized by `MAILSERVER_HOSTNAME` and `TRAEFIK_NETWORK`, both set
   in step 2) are enough on their own. If you'd rather manage the domain
   through Dokploy's own Domains UI instead, point it at the `mailserver`
   service's port 8080 and remove the `labels:` block from
   `docker-compose.yml` first — having both configure Traefik for the same
   router can conflict.

6. **Database**: embedded SQLite (default) is fine for small deployments
   and lives on the `mail-data` volume Dokploy manages. For Postgres,
   provision one via Dokploy's own Databases feature and set
   `MAILSERVER_DATABASE_DRIVER=postgres` and `MAILSERVER_DATABASE_DSN` to
   the internal connection string Dokploy gives you (uncomment the
   matching lines in `docker-compose.yml`'s `environment:` block, or just
   set both as Dokploy env vars directly — no compose file edit needed).

7. **Deploy**, then create your first mailbox from Dokploy's terminal/exec
   feature for the container:

   ```sh
   mailserver user create --config /etc/mailserver/config.yaml \
     --email alice@yourdomain.com --password 'a strong password'
   ```

   and add the DNS records from the section above (`mailserver dkim show`
   for the DKIM one).

## Database: SQLite vs Postgres

Default is embedded SQLite (`database.driver: sqlite`, a single file under
`/data`). For Postgres, set:

```yaml
database:
  driver: postgres
  dsn: "postgres://mailserver:password@postgres:5432/mailserver?sslmode=disable"
```

and uncomment the `postgres` service and `depends_on` block in
`docker-compose.yml`. Schema migrations run automatically on startup either
way.

## Outbound relay via a smart host (Brevo, etc.)

A fresh VPS IP has no sending reputation, and major providers (Gmail,
Outlook, Yahoo) will often silently spam-box or reject mail from it
regardless of correct SPF/DKIM/DMARC. Routing outbound mail through an
established relay's IPs instead of delivering directly to each recipient's
MX sidesteps that. The mechanism is plain authenticated SMTP — identical
for Brevo, Postmark, SES, Mailgun, or any other provider; nothing here is
Brevo-specific.

Configure it from the webmail **Admin** page (any admin account →
`/admin` → "Outbound relay"):

- **Host**: e.g. `smtp-relay.brevo.com`
- **Port**: `587` (STARTTLS; this is the only mode supported — implicit-TLS
  ports like 465 aren't)
- **Username** / **Password**: your provider's SMTP credentials (for
  Brevo: your account email + an SMTP key, not your login password)

Toggling "Enable outbound relay" takes effect on the *next* delivery
attempt — no restart needed. While enabled, **every** non-local outbound
message routes through it exclusively (no direct-MX fallback); disabling
it reverts to direct-to-MX delivery for everything. The connection's
certificate is fully verified (unlike the opportunistic, unverified TLS
used for direct-to-MX delivery) since real credentials go over it — a
misconfigured host/port will fail loudly rather than silently downgrading
to an unencrypted, credential-leaking connection.

The credential is stored in the database in plaintext (it must be
recoverable as-is to authenticate, unlike user login passwords, which are
bcrypt-hashed since they only ever need verifying) — keep database access
as tightly scoped as you would `config.yaml`'s own secrets.

## Hardening

### Inbound SPF/DKIM/DMARC verification

Every message accepted on the inbound listener (port 25 — not submission,
which is already authenticated) gets an `Authentication-Results` header
prepended, e.g.:

```
Authentication-Results: mail.example.com; spf=pass smtp.mailfrom=sender.example;
 dkim=pass header.d=sender.example; dmarc=pass header.from=sender.example
```

This is verification, not enforcement — nothing is rejected based on the
result. DMARC alignment uses `golang.org/x/net/publicsuffix` for the
organizational-domain comparison relaxed alignment requires, so it's
correct for domains under multi-label public suffixes (e.g. `.co.uk`), not
just naive suffix matching.

### Brute-force protection and reverse-proxy awareness

SMTP AUTH, IMAP LOGIN, and webmail login are each rate-limited
independently (`security.auth_rate_limit`: attempts allowed in a trailing
window before a temporary ban). The critical property, worth restating
because it's the one that caused a real production incident before: **an
address is only ever trusted to say who it's forwarding for if it's in
`security.trusted_proxies`.**

- The webmail server resolves the real client IP from `X-Forwarded-For`,
  but only when the immediate TCP peer (Traefik) is in
  `security.trusted_proxies`. A direct, untrusted client cannot forge this
  header to evade its own ban or to frame another address (like Traefik's
  own IP) for one.
- SMTP and IMAP are reached directly — Traefik can't proxy raw mail
  protocols — so the TCP peer is authoritative by default. If you do put a
  TCP-level load balancer in front of ports 587/993 (not Traefik), it can
  speak the PROXY protocol (v1/v2); the listener only parses and trusts
  that preamble from sources in `security.trusted_proxies`, and passes
  every other connection through completely untouched.

Leave `trusted_proxies` empty unless you know you need it — the default
(direct TCP peer / no forwarded-header trust) is correct for the
docker-compose topology this repo ships.

### Health check and metrics

`listen.metrics` (default `127.0.0.1:9090`, loopback-only) serves:

- `GET /healthz` — 200 if the database is reachable, 503 otherwise.
- `GET /metrics` — Prometheus exposition format: auth attempts by service
  and result, SMTP message outcomes, outbound relay attempts, queue depth,
  and inbound SPF/DKIM/DMARC results.

Both are unauthenticated by design, so keep this listener off any
publicly reachable interface — scrape it from Prometheus running on the
same host or Docker network, not through Traefik.

## Known limitations (by design, for this phase)

- Outbound relay does opportunistic STARTTLS (falls back to plaintext if
  the remote doesn't support it) and doesn't verify the remote's
  certificate — standard practice for unauthenticated MTA-to-MTA delivery,
  matching how most mail servers behave.
- Bounce notifications for permanently failed outbound mail are delivered
  to the sender's own INBOX only if the sender is a local user.
- No spam-filtering (Bayesian/ML) — SPF/DKIM/DMARC verification results are
  surfaced but nothing scores or filters on them.
- The webmail UI only shows the standard folders (INBOX/Sent/Drafts/Trash/
  Junk); it doesn't expose creating or browsing custom IMAP folders (an
  IMAP client still can, since both go through the same storage).
- Composed mail is sent as plain UTF-8 text (`Content-Transfer-Encoding:
  8bit`), not HTML — matches how the reading pane renders incoming HTML
  mail (in a sandboxed, script-disabled iframe) without needing an HTML
  editor or sanitizer in the compose path.
