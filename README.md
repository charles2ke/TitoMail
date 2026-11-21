# TitoMail

TitoMail is a self-hosted email server with a Gmail/Outlook-style webmail
client. It is a single Go binary that receives and sends mail over SMTP,
serves IMAP to desktop and phone mail apps, and embeds a React web interface.
Everything is stored in one SQLite database.

## Features

**Webmail**

- Conversation view (can be turned off), stars, archive, spam, trash, and
  colored labels.
- Search with Gmail-style operators: `from:`, `to:`, `cc:`, `bcc:`,
  `subject:`, `filename:`, `has:attachment`, `is:unread`, `is:starred`,
  `in:`, `label:`, `before:`, `after:`, `older_than:`, `newer_than:`,
  `larger:` and `smaller:`. There is also an advanced search form.
- Rich-text composer:
  - attachments and address autocomplete;
  - drafts saved automatically;
  - undo send and scheduled send;
  - signatures.
- Filters that label, star, archive, mark as read, move or delete incoming
  mail. A vacation responder and contacts are also included.
- Live updates and desktop notifications for new mail.
- Keyboard shortcuts (press `?`), light and dark themes, two densities, a
  reading pane on the right or below, and a phone layout.
- HTML email is sanitized and shown in a sandboxed frame. External images
  are blocked until you allow them.

**Mail server**

- Inbound SMTP (MX) with SPF, DKIM and DMARC verification. The results are
  recorded in an `Authentication-Results` header. Mail is rejected or moved
  to Junk according to the sender's DMARC policy.
- Authenticated submission on ports 587 (STARTTLS) and 465 (TLS).
- An outbound queue:
  - DKIM signing;
  - retries with backoff;
  - bounce messages (DSN);
  - an optional relay (smarthost).
- IMAP4rev2/IMAP4rev1 with IDLE, MOVE, UIDPLUS, SPECIAL-USE, LIST-STATUS and
  more, for desktop and phone mail apps.
- Multiple domains, aliases and catch-all addresses, and per-user quotas.
- An admin console with a DNS record checker and queue management.
- TLS certificates from Let's Encrypt (ACME), or your own certificate, which
  is reloaded automatically when renewed.
- Client autoconfiguration through Thunderbird-style `autoconfig`, Outlook
  `autodiscover` and DNS SRV records.

## Quick start (local)

You need Go 1.26 or newer, Node.js 20.19+ or 22.12+ and `make`.

```sh
make run
```

This builds everything and starts a development server:

| Service     | Address                 |
|-------------|-------------------------|
| Web         | http://localhost:8080   |
| SMTP (MX)   | localhost:2525          |
| Submission  | localhost:1587          |
| IMAP        | localhost:1143          |

The log prints a one-time setup link
(`http://localhost:8080/setup?token=...`). Open it to create the first
administrator. The mail domain is taken from the administrator's address.

In this mode:

- Passwords are accepted without TLS (`-allow-insecure-auth`).
- Mail for other domains stays in the queue (`-disable-outbound`).

To try it, send messages between two accounts on the same domain. Create
the second account under **Administration → Users**.

## Running in production

### Requirements

- A server with a public IP address. Port 25 must be open in both
  directions; many cloud providers block outbound port 25, in which case use
  a relay (see below).
- A domain name, and the ability to set reverse DNS (PTR) for the server's
  IP address.

### With Docker Compose

1. Edit `docker-compose.yml`: set `TITOMAIL_HOSTNAME` (for example
   `mail.example.com`) and `TITOMAIL_ACME_EMAIL`.
2. Point the A/AAAA records for the hostname at the server, so a
   certificate can be issued.
3. Start the server:

   ```sh
   docker compose up -d
   docker compose logs titomail   # shows the setup link
   ```

4. Open the setup link and create the administrator. Then add the
   remaining DNS records shown under
   **Administration → Domains → DNS records** (see
   [DNS records](#dns-records)).

The image runs as a non-root user and keeps its data in the `/data`
volume. Certificates come from Let's Encrypt.

There is a catch with IPv6. If the host accepts connections over IPv6 but
IPv6 isn't enabled for the container's network, Docker's userland proxy
forwards those connections. The server then sees the proxy's address
instead of the sender's, which breaks SPF checks. To avoid this, either:

- enable IPv6 for the network; or
- use `network_mode: host`, together with `user: root` so the server can
  bind ports below 1024.

### With the binary

```sh
make build
sudo setcap cap_net_bind_service=+ep bin/titomail   # bind ports < 1024 without root
bin/titomail serve \
  -hostname mail.example.com -data-dir /var/lib/titomail \
  -acme -acme-email you@example.com \
  -http-addr :80 -https-addr :443 \
  -smtp-addr :25 -submission-addr :587 -submissions-addr :465 \
  -imap-addr :143 -imaps-addr :993
```

The binary is static and has no runtime dependencies. Stop it with SIGTERM
or Ctrl-C; it shuts down gracefully.

### DNS records

For each mail domain, `titomail dns example.com` prints the records it
needs. So does **Administration → Domains → DNS records**, which also
checks what is currently published. For `example.com` served by
`mail.example.com`:

| Type   | Name                              | Value                                       |
|--------|-----------------------------------|---------------------------------------------|
| A/AAAA | `mail.example.com`                | the server's IP addresses                   |
| PTR    | the server's IP addresses         | `mail.example.com` (set at your provider)   |
| MX     | `example.com`                     | `10 mail.example.com.`                      |
| TXT    | `example.com`                     | `v=spf1 mx -all`                            |
| TXT    | `titomail._domainkey.example.com` | `v=DKIM1; k=rsa; p=...` (generated per domain) |
| TXT    | `_dmarc.example.com`              | `v=DMARC1; p=quarantine; adkim=r; aspf=r`   |
| CNAME  | `autoconfig.example.com`          | `mail.example.com.` (optional)              |
| SRV    | `_imaps._tcp.example.com`         | `0 1 993 mail.example.com.` (optional)      |
| SRV    | `_submissions._tcp.example.com`   | `0 1 465 mail.example.com.` (optional)      |

### TLS

- `-acme` obtains and renews certificates from Let's Encrypt. The HTTP
  listener must be reachable on port 80, or the HTTPS listener on port 443.
  `-acme-directory` selects another CA, such as the Let's Encrypt staging
  environment.
- `-tls-cert` and `-tls-key` use your own PEM files. The files are checked
  for changes at most every 30 seconds, so renewed certificates are used
  without a restart.

When `-https-addr` is set, plain HTTP redirects to HTTPS. Autoconfiguration
and `/healthz` are still served over plain HTTP.

### Sending through a relay

If outbound port 25 is blocked, or your IP address has a poor reputation,
send all outgoing mail through a smarthost:

```sh
-relay-host smtp.relay.example -relay-port 587 -relay-username user -relay-password secret
```

`-relay-tls` can be `starttls` (the default), `tls` or `none`.

### Behind a reverse proxy

You can put the web interface behind nginx, Caddy or another proxy. If you
do:

- Set `-public-url` to the external URL.
- Set `-trust-proxy` so client addresses are taken from the last
  `X-Forwarded-For` entry. Login throttling and logs use these addresses.
- Set `-secure-cookies` if the proxy terminates TLS.
- Don't buffer `/api/events`, which is a server-sent event stream.

SMTP and IMAP must be exposed directly.

## Mail apps

TitoMail publishes these settings for apps that support automatic setup
(see [DNS records](#dns-records)). To set up an app manually:

|          | Server             | Port | Security           |
|----------|--------------------|------|--------------------|
| Incoming | `mail.example.com` | 993  | SSL/TLS (IMAP)     |
|          |                    | 143  | STARTTLS (IMAP)    |
| Outgoing | `mail.example.com` | 465  | SSL/TLS (SMTP)     |
|          |                    | 587  | STARTTLS (SMTP)    |

The username is your full email address, and the password is your TitoMail
password.

## Configuration

Every option can be set with a flag or with an environment variable. Flags
take precedence, and invalid values stop the server at startup.

| Flag | Environment variable | Default | Description |
|------|----------------------|---------|-------------|
| `-hostname` | `TITOMAIL_HOSTNAME` | system hostname | Public name of the mail server |
| `-data-dir` | `TITOMAIL_DATA_DIR` | `./data` | Database and certificate cache |
| `-public-url` | `TITOMAIL_PUBLIC_URL` | from hostname and port | External URL of the web interface |
| `-http-addr` | `TITOMAIL_HTTP_ADDR` | `:8080` | HTTP listen address (empty disables it) |
| `-https-addr` | `TITOMAIL_HTTPS_ADDR` | | HTTPS listen address (needs TLS) |
| `-smtp-addr` | `TITOMAIL_SMTP_ADDR` | `:2525` | Inbound SMTP (MX), normally `:25` |
| `-submission-addr` | `TITOMAIL_SUBMISSION_ADDR` | `:1587` | Submission with STARTTLS, normally `:587` |
| `-submissions-addr` | `TITOMAIL_SUBMISSIONS_ADDR` | | Submission over TLS, normally `:465` (needs TLS) |
| `-imap-addr` | `TITOMAIL_IMAP_ADDR` | `:1143` | IMAP with STARTTLS, normally `:143` |
| `-imaps-addr` | `TITOMAIL_IMAPS_ADDR` | | IMAP over TLS, normally `:993` (needs TLS) |
| `-tls-cert`, `-tls-key` | `TITOMAIL_TLS_CERT`, `TITOMAIL_TLS_KEY` | | PEM certificate and key |
| `-acme` | `TITOMAIL_ACME` | `false` | Get certificates from Let's Encrypt |
| `-acme-email` | `TITOMAIL_ACME_EMAIL` | | ACME account contact |
| `-acme-directory` | `TITOMAIL_ACME_DIRECTORY` | Let's Encrypt | ACME directory URL |
| `-allow-insecure-auth` | `TITOMAIL_ALLOW_INSECURE_AUTH` | `false` | Accept passwords without TLS (development only) |
| `-max-message-bytes` | `TITOMAIL_MAX_MESSAGE_BYTES` | 50 MiB | Largest accepted message |
| `-max-upload-bytes` | `TITOMAIL_MAX_UPLOAD_BYTES` | 25 MiB | Attachment limit when composing |
| `-relay-host`, `-relay-port` | `TITOMAIL_RELAY_HOST`, `TITOMAIL_RELAY_PORT` | port 587 | Send all outbound mail through this host |
| `-relay-username`, `-relay-password` | `TITOMAIL_RELAY_USERNAME`, `TITOMAIL_RELAY_PASSWORD` | | Relay credentials |
| `-relay-tls` | `TITOMAIL_RELAY_TLS` | `starttls` | `starttls`, `tls` or `none` |
| `-outbound-port` | `TITOMAIL_OUTBOUND_PORT` | `25` | Port for direct delivery to MX hosts |
| `-disable-outbound` | `TITOMAIL_DISABLE_OUTBOUND` | `false` | Keep remote mail queued |
| `-queue-max-age` | `TITOMAIL_QUEUE_MAX_AGE` | `120h` | How long to retry before bouncing |
| `-inbound-auth-checks` | `TITOMAIL_INBOUND_AUTH_CHECKS` | `true` | Verify SPF, DKIM and DMARC |
| `-admin-email`, `-admin-password` | `TITOMAIL_ADMIN_EMAIL`, `TITOMAIL_ADMIN_PASSWORD` | | Create this administrator on first start |
| `-secure-cookies` | `TITOMAIL_SECURE_COOKIES` | `false` | Always mark cookies Secure (automatic with `-https-addr`) |
| `-trust-proxy` | `TITOMAIL_TRUST_PROXY` | `false` | Trust `X-Forwarded-*` headers |
| `-log-level` | `TITOMAIL_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

## Administration

Administrators manage domains, users, aliases (including catch-all
`@example.com` aliases) and the outbound queue in the web interface under
**Administration** in the account menu. The same basics are available from
the command line:

```sh
titomail domain add example.org                          # also prints its DNS records
titomail domain list
titomail dns example.org
titomail user add ada@example.org --admin --name "Ada"   # password from TITOMAIL_PASSWORD, stdin or a prompt
titomail user passwd ada@example.org
titomail user list
titomail version
```

Notes:

- The `domain`, `user` and `dns` commands accept `--data-dir` (or
  `TITOMAIL_DATA_DIR`). The directory must already exist.
- Add a domain before creating users in it.
- The commands work while the server is running, and changes take effect
  immediately.

## Development

```sh
make          # build the web interface and bin/titomail
make test     # go vet and Go tests (unit and integration) with the race detector
make e2e      # Playwright end-to-end tests; screenshots go to e2e/screenshots
make run      # local development server
make docker   # build the container image
```

Notes:

- For `make e2e`, install a browser first with
  `cd e2e && npm ci && npx playwright install chromium`. Or set
  `PW_CHANNEL=chrome` to use an installed Google Chrome.
- For live reloading of the web interface, run `npm run dev` in `web/`
  while a server is running on port 8080. Vite proxies API requests to
  `TITOMAIL_BACKEND` (default `http://localhost:8080`).

Code layout:

| Path | Contents |
|------|----------|
| `cmd/titomail` | Command-line entry point and administration commands |
| `internal/server` | Wires the services together, TLS and ACME |
| `internal/config` | Flags, environment variables and validation |
| `internal/auth` | Password checks with brute-force throttling |
| `internal/smtpd` | SMTP for MX and submission, and SPF/DKIM/DMARC checks |
| `internal/delivery` | Local delivery, filters, vacation replies, and the outbound queue with DKIM, retries and bounces |
| `internal/imapd` | IMAP server |
| `internal/api` | JSON API, live events, autoconfiguration and static files |
| `internal/events` | Sends mailbox changes to connected web clients |
| `internal/store` | SQLite storage, search and threading |
| `internal/email` | MIME parsing, message building and HTML sanitizing |
| `web` | React and TypeScript web interface |
| `e2e` | Playwright tests |

## Project status

TitoMail is new and has not been used in production yet. What has been
tested:

- Go unit tests, and integration tests that run the whole server.
  - The integration tests deliver mail over SMTP, submission and the
    outbound queue to a fake remote server, including DKIM signing and
    verification.
  - They read mail over IMAP and the web API, and use TLS with test
    certificates.
- Playwright end-to-end tests of the web interface in Chrome, on desktop
  and phone sizes.
- The Docker image: building, starting, and basic SMTP, IMAP and HTTP
  responses.

Not tested yet:

- Exchanging mail with real providers such as Gmail and Outlook.com.
- Obtaining certificates from Let's Encrypt.
- Using real mail apps such as Thunderbird, Apple Mail and Outlook.

Reports and fixes are welcome.

## Limitations

- Junk detection relies on SPF, DKIM and DMARC results and your filters.
  There is no content or reputation scoring, and no virus scanning.
- There is no POP3, JMAP, CalDAV/CardDAV, Exchange ActiveSync or two-factor
  authentication.
- It runs on a single server with SQLite. It is meant for individuals,
  families and small organizations.
- Deliverability to large providers depends on your IP address's
  reputation, reverse DNS and correct SPF, DKIM and DMARC records.

## License

[GNU Affero General Public License v3.0](LICENSE)
