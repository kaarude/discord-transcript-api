# Discord Transcript API

A self-hosted Go service that turns Discord channels into durable, self-contained transcript archives: cached CDN media, pixel-faithful HTML rendering, JSON/HTML/text exports, private links, an admin dashboard, and runtime health telemetry.

- **One static binary** in a `scratch` container — no runtime dependencies.
- **You own the auth**: sign in with passkeys and/or a dashboard password stored by the service itself. No external identity provider, no shared admin secret.
- **Faithful transcripts**: exports read like the source channel, preserving authorship, colors, embeds, reactions, and attachments.
- **Storage-bounded by design**: per-file and per-transcript caps, a global storage ceiling, and size-bounded rendering keep worst-case memory finite.

## How it works

Your bot (or any client holding an API token) calls `GET /transcript` with the channel ID. The service fetches messages and metadata from the Discord API, caches every attachment on its own CDN host, renders three export formats into a portable directory, and returns a shareable link. Links can be public or protected with a rotating access key; admins manage everything from `/admin`.

## Quick start with Docker

```sh
git clone https://github.com/kaarude/discord-transcript-api.git
cd discord-transcript-api
cp .env.example .env
# Set PUBLIC_BASE_URL (HTTPS) and TRUST_PROXY in .env
mkdir -p data transcripts
sudo chown -R "${PUID:-1000}:${PGID:-1000}" data transcripts
chmod 750 data transcripts
docker compose up -d --build
```

- Admin dashboard: `http://localhost:3010/admin`
- Health dashboard: `http://localhost:3010/health`
- Health JSON: `http://localhost:3010/health?format=json`

### First-time setup

On first boot there is no sign-in method yet. Read the one-time setup code on the host:

```sh
cat data/setup-code
```

Then open `/admin`, enter the code, and choose how you want to sign in:

| Option | Notes |
| --- | --- |
| **Passkey** | Phishing-resistant; stored on your device or a hardware key. Requires HTTPS (except localhost). |
| **Password** | Simple and portable. Minimum 12 characters; stored as an Argon2id hash. |

The setup code file is deleted as soon as the first method is saved. You can always add the other method later from Administration — passkeys and passwords coexist freely. The service refuses to remove the last remaining sign-in method so you cannot lock yourself out.

Sign-in creates a 24-hour HttpOnly, SameSite=Strict session that is cleared when the server restarts or when you change/remove the dashboard password.

## Prebuilt image

Every commit to `main` publishes `ghcr.io/kaarude/discord-transcript-api:latest`; semver tags publish versioned images:

```sh
docker compose pull && docker compose up -d
```

## API

### Create a transcript

```http
GET /transcript
Authorization: <api-token>
Discord-Bot-Token: <discord-bot-token>
Channel-Id: <discord-channel-id>
```

The response contains `url`, `expiresAt`, and `expiresInDays`. Tokens may carry the `Bot ` prefix; Discord bot tokens are used transiently and never persisted. Create and revoke API tokens from the admin dashboard (they are stored hashed).

### Transcripts

- `GET /transcripts/:uuid` — rendered HTML transcript
- `GET /transcripts/:uuid/download/html|json|txt`
- `GET /transcripts/:uuid/assets/:filename` — cached media

Private links exchange their query-string access key for an HTTP-only cookie and redirect to a clean URL.

### Admin API

Admin endpoints accept only the HttpOnly browser session created at sign-in. Bearer headers never grant dashboard access.

- `GET /api/admin/settings`
- `POST /api/admin/tokens` · `DELETE /api/admin/tokens/:tokenId`
- `PUT /api/admin/rate-limit` · `PUT /api/admin/transcript-limit`
- `GET /api/admin/transcripts?user=&page=1&limit=50`
- `POST /api/admin/transcripts/:uuid/renew`
- `PATCH /api/admin/transcripts/:uuid/visibility`
- `DELETE /api/admin/transcripts/:uuid`
- `GET /api/admin/passkeys` · `POST /api/admin/passkeys/register/start|finish` · `DELETE /api/admin/passkeys/:passkeyId`
- `PUT /api/admin/password` · `DELETE /api/admin/password`

### Health

- `GET /health` — HTML for browsers, JSON for API clients
- `GET /health/data` · `GET /health/ping` · `GET /health/probe?bytes=131072` (≤256 KiB)

All health routes require a signed-in session except `/access/status`, which stays public for orchestrators and returns state only — never secrets.

## Storage design

The append-only JSONL registry (`data/transcripts.jsonl`) loads once into a concurrency-safe in-memory index. Updates append new record versions; deletes append tombstones and are compacted at startup and hourly. Transcript bundles are ordinary portable directories under `transcripts/<id>/` containing `index.html`, `transcript.json`, `transcript.txt`, `assets-manifest.json`, and `assets/`.

Media downloads are hardened:

- only HTTPS URLs on `cdn.discordapp.com` / `media.discordapp.net`;
- every redirect revalidated, at most three hops;
- streamed to temporary files — whole attachments are never buffered;
- 25 MiB per file, 250 MiB per transcript cap;
- atomic temporary directories so crashes never leave half-written bundles visible.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `PUBLIC_BASE_URL` | required | Absolute origin used in returned links, cookies, and WebAuthn |
| Port | `3010` (fixed) | Host and container port |
| `TRUST_PROXY` | unset | Enables validated `X-Forwarded-For` use for rate limiting; accepts hop count, IPs/CIDRs, or `true` |
| `HOST_BIND_ADDRESS` | `127.0.0.1` | Host interface for the cleartext port; use a private interface only when the proxy runs elsewhere |
| `PASSKEY_RP_ID` | hostname of `PUBLIC_BASE_URL` | Optional WebAuthn relying party override |
| `PASSKEY_ORIGINS` | origin of `PUBLIC_BASE_URL` | Optional comma-separated allowed WebAuthn origins |
| `AUTH_PATH` | `data/auth.json` | Sign-in credential store (0600) |
| `TRANSCRIPT_STORAGE_LIMIT_BYTES` | `10737418240` | Global storage ceiling (10 GiB) |
| `API_TOKENS` | empty | Comma-separated bootstrap tokens merged into settings on startup |
| `RATE_LIMIT_MAX` / `RATE_LIMIT_WINDOW_MS` | `25` / `60000` | Per-token request budget |
| `TRANSCRIPT_LIMIT` | `1000` | Messages fetched per transcript (1–50000) |
| `PUID` / `PGID` | `1000` | Compose process UID/GID for bind mounts |

Notes:

- An API token present in `API_TOKENS` is restored if removed via the UI — delete it from the environment to revoke it permanently.
- Keep `PUBLIC_BASE_URL` and `PASSKEY_RP_ID` stable after enrolling passkeys; the service refuses to start with a changed relying party ID over existing passkeys rather than lock you out silently.
- Rate-limit counters live in memory and reset on restart.

## Reverse proxy example

With Caddy on the same host:

```caddyfile
transcripts.example.com {
  reverse_proxy 127.0.0.1:3010
}
```

Set `PUBLIC_BASE_URL=https://transcripts.example.com` and point `TRUST_PROXY` at the proxy's connecting address. TLS must terminate at the proxy so private-link cookies stay secure. The Compose port binds to loopback by default; see DEPLOY.md for split-host setups and nginx equivalents.

## Automatic updates (optional)

`deploy/self-update.sh` plus its systemd unit/timer implement a self-hosting GitOps loop: poll your repository, build and test a candidate image, swap containers only when the health check passes, roll back automatically otherwise, while leaving `.env`, `data/`, and `transcripts/` untouched. Full install steps are in DEPLOY.md.

## Native build

Go 1.26.6 or newer (matches the standard-library security fixes used by passkey verification):

```sh
go test ./...
go build -trimpath -ldflags="-s -w" -o transcript-api ./cmd/transcript-api
./transcript-api
```

The executable reads `.env` automatically when present and handles graceful SIGINT/SIGTERM shutdown.

Make targets: `make test`, `make vet`, `make race`, `make docker-up`, `make updater-smoke`, `make docker-smoke`.

## Operational notes

- Expired transcripts remain stored so admins can renew them; explicit deletion removes their bytes. New exports stop safely at the global storage ceiling.
- Transcript creation is serialized (one concurrent export) to bound worst-case resource use.
- The registry and settings files are safe for exactly one service process. Do not mount one writable storage into multiple replicas.
- Back up `data/auth.json` with the rest of `data/`. Passkeys are not portable across domains.
- `data/` and `transcripts/` contain all state; everything else is disposable.

## License

[MIT](LICENSE)
