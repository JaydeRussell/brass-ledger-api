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
# localhost ports — also used as-is in .github/workflows/ci.yml, pointed
# at the real production URLs right after a deploy, so a bad deploy
# gets caught immediately instead of silently.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

BACKEND="${BACKEND_URL:-http://localhost:8080}"
FRONTEND="${FRONTEND_URL:-http://localhost:3000}"
fail=0

# A Cloudflare Container that was asleep (see wrangler.jsonc's sleepAfter,
# and a fresh deploy is effectively the same as "asleep") can take longer
# than a single request's timeout to spin up and start responding — its
# very first request after cold-starting is the slow one, not steady
# state. A flat one-shot check was seeing real "000, connection not made
# yet" failures here that resolved on their own moments later, so each
# check now retries with a short backoff instead of failing immediately
# on the first miss. Local runs against an already-running dev stack
# still pass on the first attempt, same as before — this only changes
# behavior when the target genuinely isn't answering yet.
check() {
	local name="$1" url="$2" want="$3"
	local got attempt
	for attempt in 1 2 3 4 5 6; do
		got="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url")"
		if [ "$got" = "$want" ]; then
			echo "ok    $name ($url -> $got)"
			return
		fi
		if [ "$attempt" -lt 6 ]; then
			sleep 5
		fi
	done
	echo "FAIL  $name ($url -> $got, want $want)"
	fail=1
}

check "backend healthz"              "$BACKEND/healthz"  200
check "backend readyz (db reachable)" "$BACKEND/readyz"  200
check "backend auth wired (signed out -> 401)" "$BACKEND/api/me" 401
check "backend BCP routes gated (signed out -> 401)" "$BACKEND/api/events/smoke-test" 401
check "frontend root"                "$FRONTEND/"        200

echo
if [ "$fail" -eq 0 ]; then
	echo "All checks passed."
else
	echo "One or more checks failed — see above." >&2
fi
exit $fail
