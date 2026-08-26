# Deploy to a server

Discord Transcript API is a single static Go binary in a minimal container. The container listens on port `3010`; Compose binds that port to host loopback by default.

## Initial setup

```sh
mkdir -p /opt/transcript-api
cd /opt/transcript-api
git clone https://github.com/kaarude/discord-transcript-api.git .
cp .env.example .env
mkdir -p data transcripts
chown -R 1000:1000 data transcripts
chmod 750 data transcripts
docker compose pull   # or: docker compose up -d --build
docker compose up -d --wait
```

Set these values in `.env` before exposing the service:

```env
APP_ENV=production
PUBLIC_BASE_URL=https://transcripts.example.com
PASSKEY_RP_ID=
PASSKEY_ORIGINS=
TRANSCRIPT_STORAGE_LIMIT_BYTES=10737418240
TRUST_PROXY=<REVERSE-PROXY-IP>
HOST_BIND_ADDRESS=127.0.0.1
```

`PUBLIC_BASE_URL` must be the HTTPS origin used in the browser. Leave the passkey overrides empty unless the service answers on more than one origin of the same site. Do not change `PASSKEY_RP_ID` after enrolling passkeys.

The unprotected liveness endpoint is `http://127.0.0.1:3010/access/status`. Health and admin routes require a signed-in session.

## First-time setup

First boot contains no sign-in method. Retrieve the locally generated setup code, visit the public `/admin` URL, and pick a passkey or a password:

```sh
cat data/setup-code
```

The application deletes this file as soon as the first method is enrolled. Add a second method (passkey and/or password) from Administration before relying on either as the only recovery path.

## Reverse proxy

nginx example:

```nginx
location / {
    proxy_pass http://127.0.0.1:3010;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

Set `TRUST_PROXY` to the exact connecting proxy IP or CIDR so rate limiting sees real client addresses. If the proxy runs on another host, set `HOST_BIND_ADDRESS` to the service's private interface address and firewall port 3010 to the proxy only. Never bind the cleartext backend directly to a public interface.

Caddy alternative:

```caddyfile
transcripts.example.com {
  reverse_proxy 127.0.0.1:3010
}
```

## Persistence

The `data/` and `transcripts/` mounts hold all state — settings, the JSONL registry, transcript bundles, cached media, and the auth store. They survive image replacement. Everything else is disposable.

Never run two service processes against the same writable storage.

## Automatic updates (optional)

Install the updater once; it follows your repository's default branch:

```sh
# Clone to the source dir and install the script + units
git clone https://github.com/kaarude/discord-transcript-api.git /opt/transcript-api-source
install -m 755 /opt/transcript-api-source/deploy/self-update.sh /usr/local/sbin/transcript-api-update
install -m 644 /opt/transcript-api-source/deploy/self-update.service /etc/systemd/system/self-update.service
install -m 644 /opt/transcript-api-source/deploy/self-update.timer /etc/systemd/system/self-update.timer

# Edit /etc/systemd/system/self-update.service first:
#   DEPLOY_APP_DIR      where compose runs        (/opt/transcript-api)
#   DEPLOY_SOURCE_DIR   git clone location        (/opt/transcript-api-source)
#   DEPLOY_REPO         your repo URL
#   DEPLOY_IMAGE        ghcr.io/<you>/discord-transcript-api
# For private repos add: Environment=DEPLOY_GIT_KEY=/root/.ssh/deploy_key

systemctl daemon-reload
systemctl enable --now self-update.timer
systemctl start self-update.service
```

Verify before merging a production change:

```sh
systemctl is-enabled self-update.timer
systemctl status self-update.service --no-pager
docker inspect --format '{{.State.Running}} {{.State.Health.Status}}' transcript-api
```

The timer fetches `main`, rsyncs source into the app directory while preserving `.env`, `data/`, and `transcripts/`, builds and tests a candidate image, waits for its port-3010 health check, then swaps containers. An unhealthy candidate rolls back automatically and retries on the next run because the deployed commit is recorded only after a healthy replacement.
