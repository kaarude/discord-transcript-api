#!/bin/sh
# Pull-and-deploy updater for self-hosted servers.
#
# Clones/pulls a git source, rsyncs it into the app directory while leaving
# .env, data/, and transcripts/ untouched, builds a candidate Docker image,
# runs its health check, then swaps containers. Rolls back automatically if
# the candidate is unhealthy. Pair it with the included systemd service/timer.
set -eu

APP_DIR="${DEPLOY_APP_DIR:-/opt/transcript-api}"
SOURCE_DIR="${DEPLOY_SOURCE_DIR:-/opt/transcript-api-source}"
BRANCH="${DEPLOY_BRANCH:-main}"
IMAGE="${DEPLOY_IMAGE:-ghcr.io/kaarude/discord-transcript-api}"
CONTAINER="${DEPLOY_CONTAINER:-transcript-api}"
STATE_DIR="${DEPLOY_STATE_DIR:-/var/lib/transcript-api-update}"
STATE_FILE="$STATE_DIR/deployed-sha"
# Optional: set to an SSH key path when cloning over SSH.
GIT_KEY="${DEPLOY_GIT_KEY:-}"

if [ -n "$GIT_KEY" ]; then
  export GIT_SSH_COMMAND="ssh -i $GIT_KEY -o IdentitiesOnly=yes"
fi

if [ ! -d "$SOURCE_DIR/.git" ]; then
  git clone --branch "$BRANCH" --single-branch "${DEPLOY_REPO:?Set DEPLOY_REPO to your repository URL}" "$SOURCE_DIR"
fi

git -C "$SOURCE_DIR" fetch --quiet origin "$BRANCH"
latest_sha="$(git -C "$SOURCE_DIR" rev-parse "origin/$BRANCH")"
deployed_sha="$(cat "$STATE_FILE" 2>/dev/null || true)"
container_state="$(docker inspect --format '{{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$CONTAINER" 2>/dev/null || true)"
[ "$latest_sha" != "$deployed_sha" ] || [ "$container_state" != "true healthy" ] || exit 0

git -C "$SOURCE_DIR" reset --quiet --hard "origin/$BRANCH"
rsync -a --delete --exclude '.git/' --exclude '.env' --exclude 'data/' --exclude 'transcripts/' "$SOURCE_DIR/" "$APP_DIR/"

candidate="$IMAGE:candidate-$latest_sha"
old_image="$(docker inspect --format '{{.Image}}' "$CONTAINER" 2>/dev/null || true)"
docker build --pull --build-arg BUILD_DATE="$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --tag "$candidate" "$APP_DIR"
docker tag "$candidate" "$IMAGE:latest"
cd "$APP_DIR"
service_name="$(basename "$CONTAINER")"
if ! docker compose up -d --no-deps --force-recreate --wait --wait-timeout 90; then
  echo "New container failed its health check; rolling back" >&2
  if [ -n "$old_image" ]; then
    docker tag "$old_image" "$IMAGE:latest"
    docker compose up -d --no-deps --force-recreate --wait --wait-timeout 90
  fi
  exit 1
fi
mkdir -p "$STATE_DIR"
printf '%s\n' "$latest_sha" > "$STATE_FILE"
docker image rm "$candidate" >/dev/null 2>&1 || true
docker image prune -f --filter "until=168h" >/dev/null
