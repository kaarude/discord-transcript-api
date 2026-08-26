#!/bin/sh
set -eu

IMAGE="transcript-api-storage-smoke"
CONTAINER="transcript-api-storage-smoke-$$"
ROOT="$(mktemp -d)"
HOST_PORT="${SMOKE_PORT:-13010}"

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker image rm "$IMAGE" >/dev/null 2>&1 || true
  rm -rf "$ROOT"
}
trap cleanup EXIT INT TERM

file_mode() {
  stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1"
}

mkdir -p "$ROOT/data" "$ROOT/transcripts"
chmod 777 "$ROOT/data" "$ROOT/transcripts"
docker build --build-arg BUILD_DATE="$(date -u '+%Y-%m-%dT%H:%M:%SZ')" -t "$IMAGE" . >/dev/null
docker run -d --name "$CONTAINER" -p "127.0.0.1:$HOST_PORT:3010" \
  -e APP_ENV=production -e PUBLIC_BASE_URL=http://127.0.0.1:"$HOST_PORT" \
  -v "$ROOT/data:/app/data" -v "$ROOT/transcripts:/app/transcripts" "$IMAGE" >/dev/null

attempt=0
until curl -fsS "http://127.0.0.1:$HOST_PORT/access/status" >/dev/null; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then docker logs "$CONTAINER"; exit 1; fi
  sleep 1
done

attempt=0
until [ "$(docker inspect --format '{{.State.Health.Status}}' "$CONTAINER")" = "healthy" ]; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 35 ]; then docker inspect "$CONTAINER"; exit 1; fi
  sleep 1
done

test "$(docker inspect --format '{{.Config.User}}' "$CONTAINER")" = "1000:1000"
test -s "$ROOT/data/settings.json"
test -s "$ROOT/data/auth.json"
test -s "$ROOT/data/setup-code"
test "$(file_mode "$ROOT/data/settings.json")" = "600"
test "$(file_mode "$ROOT/data/auth.json")" = "600"
test "$(file_mode "$ROOT/data/setup-code")" = "600"
curl -fsS "http://127.0.0.1:$HOST_PORT/access/status" | grep -q '"setupRequired":true'
echo "Docker port-3010, first-time setup, and mounted-storage smoke test passed"
