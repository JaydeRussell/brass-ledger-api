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
  docker compose up --build "$@"
fi
