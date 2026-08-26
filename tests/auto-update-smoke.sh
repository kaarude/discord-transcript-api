#!/bin/sh
set -eu

ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT INT TERM
BIN="$ROOT/bin"
APP="$ROOT/app"
SOURCE="$ROOT/source"
STATE="$ROOT/state"
LOG="$ROOT/commands.log"
mkdir -p "$BIN" "$APP" "$SOURCE" "$STATE"
touch "$ROOT/deploy-key" "$LOG"

cat > "$BIN/git" <<'EOF'
#!/bin/sh
set -eu
printf 'git %s\n' "$*" >> "$TEST_LOG"
case " $* " in
  *" rev-parse "*) printf '%s\n' "$TEST_SHA" ;;
esac
EOF

cat > "$BIN/rsync" <<'EOF'
#!/bin/sh
set -eu
printf 'rsync %s\n' "$*" >> "$TEST_LOG"
EOF

cat > "$BIN/docker" <<'EOF'
#!/bin/sh
set -eu
printf 'docker %s\n' "$*" >> "$TEST_LOG"
case " $* " in
  *" inspect "*".State.Running"*) printf '%s\n' "$TEST_CONTAINER_STATE" ;;
  *" inspect "*".Image"*) printf '%s\n' 'sha256:previous' ;;
  *" compose up "*)
    count=0
    if [ -f "$TEST_COMPOSE_COUNT_FILE" ]; then count="$(cat "$TEST_COMPOSE_COUNT_FILE")"; fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$TEST_COMPOSE_COUNT_FILE"
    if [ "${TEST_FAIL_FIRST_COMPOSE:-false}" = "true" ] && [ "$count" -eq 1 ]; then exit 1; fi
    ;;
esac
EOF

chmod +x "$BIN/git" "$BIN/rsync" "$BIN/docker"
export PATH="$BIN:$PATH"
export TEST_LOG="$LOG"
export TEST_SHA="0123456789abcdef0123456789abcdef01234567"
export TEST_CONTAINER_STATE="true healthy"
export TEST_COMPOSE_COUNT_FILE="$ROOT/compose-count"
export DEPLOY_APP_DIR="$APP"
export DEPLOY_SOURCE_DIR="$SOURCE"
export DEPLOY_REPO="https://github.com/example/discord-transcript-api.git"
export DEPLOY_GIT_KEY="$ROOT/deploy-key"
export DEPLOY_STATE_DIR="$STATE"

printf 'PUBLIC_BASE_URL=https://transcripts.example.com\n' > "$APP/.env"
./deploy/self-update.sh
test "$(cat "$STATE/deployed-sha")" = "$TEST_SHA"
grep -q '^rsync ' "$LOG"
grep -q '^docker build --pull ' "$LOG"
grep -q 'docker compose up -d --no-deps --force-recreate --wait --wait-timeout 90' "$LOG"
test "$(grep -c '^PUBLIC_BASE_URL=' "$APP/.env")" -eq 1

: > "$LOG"
./deploy/self-update.sh
if grep -Eq '^rsync |^docker build |^docker compose ' "$LOG"; then
  echo "Healthy deployed commit was unnecessarily rebuilt" >&2
  exit 1
fi

printf 'PUBLIC_BASE_URL=https://transcripts.example.com\nTRUST_PROXY=192.168.1.10\n' > "$APP/.env"

export TEST_CONTAINER_STATE="true unhealthy"
: > "$LOG"
./deploy/self-update.sh
grep -q '^docker build --pull ' "$LOG"
grep -q 'docker compose up -d --no-deps --force-recreate --wait --wait-timeout 90' "$LOG"
test "$(grep -c '^TRUST_PROXY=192.168.1.10$' "$APP/.env")" -eq 1

previous_sha="$TEST_SHA"
export TEST_SHA="fedcba9876543210fedcba9876543210fedcba98"
export TEST_CONTAINER_STATE="true healthy"
export TEST_FAIL_FIRST_COMPOSE="true"
rm -f "$TEST_COMPOSE_COUNT_FILE"
: > "$LOG"
if ./deploy/self-update.sh; then
  echo "Failed candidate unexpectedly reported a successful deployment" >&2
  exit 1
fi
test "$(cat "$STATE/deployed-sha")" = "$previous_sha"
test "$(cat "$TEST_COMPOSE_COUNT_FILE")" -eq 2
grep -q '^docker tag sha256:previous ghcr.io/kaarude/discord-transcript-api:latest$' "$LOG"
unset TEST_FAIL_FIRST_COMPOSE

grep -q '^OnBootSec=5min$' deploy/self-update.timer
grep -q '^RandomizedDelaySec=15s$' deploy/self-update.timer
grep -q '"${HOST_BIND_ADDRESS:-127.0.0.1}:3010:3010"' docker-compose.yml
echo "Automatic update detection, no-op, unhealthy recovery, rollback, timer, and port smoke test passed"
