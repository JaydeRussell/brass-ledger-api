#!/usr/bin/env bash
# Quick post-rebuild sanity check for the whole local stack (Postgres +
# backend + frontend, started via ./run.sh) — the same handful of curl
# checks that would otherwise get re-typed by hand after every rebuild.
#
# Deliberately doesn't touch BCP's real API (see CLAUDE.md's
# "be respectful of third-party APIs" rule) — only checks this stack's
# own routes respond the way they should. It's not a substitute for
# `go test ./...`/the frontend's `npm test`, just a fast "did the thing
# I just rebuilt actually come up" check.
#
# Usage: ./scripts/smoke-test.sh (or `make smoke`)
# Override BACKEND_URL/FRONTEND_URL if the stack isn't on the usual
# localhost ports.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

BACKEND="${BACKEND_URL:-http://localhost:8080}"
FRONTEND="${FRONTEND_URL:-http://localhost:3000}"
fail=0

check() {
	local name="$1" url="$2" want="$3"
	local got
	got="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url")"
	if [ "$got" = "$want" ]; then
		echo "ok    $name ($url -> $got)"
	else
		echo "FAIL  $name ($url -> $got, want $want)"
		fail=1
	fi
}

check "backend healthz"              "$BACKEND/healthz"  200
check "backend readyz (db reachable)" "$BACKEND/readyz"  200
check "backend auth wired (signed out -> 401)" "$BACKEND/api/me" 401
check "frontend root"                "$FRONTEND/"        200

echo
if [ "$fail" -eq 0 ]; then
	echo "All checks passed."
else
	echo "One or more checks failed — see above." >&2
fi
exit $fail
