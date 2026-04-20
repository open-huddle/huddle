#!/usr/bin/env bash
# Generate an Atlas-format versioned migration from the Ent schema.
#
# Ent's NamedDiff needs a reachable, empty Postgres ("dev DB") to replay the
# existing migrations and compute the delta against the schema in code. We
# spin one up in Docker for the lifetime of this command so the result does
# not depend on whatever happens to be in the developer's local DB.
#
# Usage: migrate-diff.sh <migration-name>
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <migration-name>" >&2
  exit 2
fi
NAME="$1"

CONTAINER="huddle-atlas-dev-$$"
# POSTGRES_IMAGE is exported by the Makefile so compose and this script share
# the same major version. The fallback exists only for `bash scripts/migrate-diff.sh`
# invocations outside `make`.
IMAGE="${POSTGRES_IMAGE:-postgres:18.3-alpine}"

# Use -P to let Docker pick a free host port — avoids collisions with the
# compose Postgres on 5432 and with any stale container from a prior run.
PORT="$(docker run --rm -d \
  --name "$CONTAINER" \
  -e POSTGRES_PASSWORD=postgres \
  -P \
  "$IMAGE" >/dev/null && \
  docker port "$CONTAINER" 5432/tcp | awk -F: 'NR==1{print $NF}')"

cleanup() { docker stop "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Wait until the container accepts connections.
for _ in $(seq 1 30); do
  if docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

export HUDDLE_DEV_DB_URL="postgres://postgres:postgres@localhost:${PORT}/postgres?sslmode=disable"
go run ./cmd/migrate "$NAME"
atlas migrate hash --dir file://migrations
