#!/usr/bin/env bash
# Builds and starts the whole stack — Postgres, the backend, and the
# frontend — with one command. Requires Docker Desktop (or another local
# Docker engine) running, and this repo to sit next to
# ../brass-ledger-web on disk (see the comment in docker-compose.yml).
#
# Usage:
#   ./run.sh          start everything, rebuilding images that changed
#   ./run.sh -d       same, but detached (returns your terminal)
#   ./run.sh down     stop and remove the containers (add -v to also wipe
#                     the Postgres data volume)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

if [[ "${1:-}" == "down" ]]; then
  shift
  docker compose down "$@"
else
  # Both containers bind-mount a logs/ directory (see docker-compose.yml's
  # LOG_FILE/volumes) so their log files land on the host, readable
  # without `docker compose exec`/`docker cp`. Pre-create them here,
  # world-writable: the backend container runs as a non-root distroless
  # user, and a directory Docker auto-creates for a bind mount would
  # otherwise be owned by root with no write access for that user,
  # causing the server to fail to open its log file at startup.
  mkdir -p logs ../brass-ledger-web/logs
  chmod 777 logs ../brass-ledger-web/logs
  docker compose up --build "$@"
fi
