#!/usr/bin/env bash
# Measures response time for each of this API's endpoints and prints a
# table: min / median / mean / max, plus the status code each returned.
#
# Point it at anything — it takes a base URL, so the same script gives
# the local docker-compose stack and the real deployment:
#
#   ./scripts/latency-map.sh                                  # localhost:8080
#   ./scripts/latency-map.sh --base https://api.brass-ledger.app
#   ./scripts/latency-map.sh --session "$TOKEN" --runs 10
#
# --session takes a `session` cookie value. Without one, the gated
# endpoints are still measured, but what you're timing is the 401 path —
# routing plus the session lookup that fails — not the real work. That's
# worth knowing on its own (it's this service's floor), just don't read
# it as the cost of the endpoint.
#
# WHAT LOCAL NUMBERS DO AND DON'T TELL YOU
#
# The endpoints that actually make people wait (/api/me/events,
# /api/me/stats, and the /api/events/* proxy) spend nearly all their
# time in Best Coast Pairings' API, not here — a cold
# /api/me/events crawls two paginated BCP feeds. A local run against an
# account with no linked BCP profile returns the empty-response path
# instead, so it measures this service's own overhead (routing, session
# lookup, Postgres) with the third-party time removed. Useful as a
# floor, and useful for spotting a regression in our own code. It is not
# the number a user experiences — for that, run this against production
# with a session for a real linked account.
#
# Be aware that a production run with a real session does hit BCP
# through our cache. Keep --runs small there; see CLAUDE.md's
# "Be respectful of BCP's API".
#
# To get a --session locally without signing in through Google, mint one
# straight into the dev database (local only — never do this anywhere
# real):
#
#   docker exec brass-ledger-db-1 psql -U brassledger -d brass_ledger -c \
#     "INSERT INTO users (google_sub, email, name, role, status)
#      VALUES ('latency-probe-sub','latency-probe@example.com','Latency Probe','admin','approved')
#      ON CONFLICT (google_sub) DO UPDATE SET status='approved', role='admin';
#      INSERT INTO sessions (token, user_id, expires_at)
#      SELECT 'latency-probe-token', id, now() + interval '2 hours'
#      FROM users WHERE google_sub='latency-probe-sub'
#      ON CONFLICT (token) DO UPDATE SET expires_at = now() + interval '2 hours';"
#
# That account has no linked BCP profile, which is the point: the gated
# endpoints take their empty-response path, so you measure this
# service's own overhead rather than BCP's.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

BASE="${BASE_URL:-http://localhost:8080}"
SESSION=""
RUNS=5
# With --gate, exit non-zero if any page's critical path exceeds
# CRITICAL_MS — so this can be used as a blocking check rather than
# something a human has to read.
GATE=0

while [ $# -gt 0 ]; do
	case "$1" in
		--base)    BASE="$2"; shift 2 ;;
		--session) SESSION="$2"; shift 2 ;;
		--runs)    RUNS="$2"; shift 2 ;;
		--gate)    GATE=1; shift ;;
		-h|--help) sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

# Microseconds -> a millisecond string. Sub-millisecond values keep a
# decimal so they stay legible rather than collapsing to "0ms".
as_ms() {
	awk -v us="$1" 'BEGIN { ms = us/1000; if (ms < 10) printf "%.2fms", ms; else printf "%dms", ms }'
}

cookie_args=()
[ -n "$SESSION" ] && cookie_args=(-H "Cookie: session=$SESSION")

# Measured means, as newline-separated "path<TAB>ms" records. A flat
# string rather than an associative array on purpose: macOS ships bash
# 3.2, where `declare -A` doesn't exist, and this script is meant to run
# on a contributor's Mac as-is.
MEANS=""

# Anything slower than this is called out as critically slow. Two
# seconds is the line past which a load stops reading as "loading" and
# starts reading as "broken" — and it's the bar this project already
# uses on the client (SLOW_LOAD_MS in brass-ledger-web's
# app/lib/useDelayedFlag.ts is 3s for showing a "taking longer than
# usual" hint, so 2s is a deliberately tighter target than the point at
# which we apologise to the user).
CRITICAL_MS="${CRITICAL_MS:-2000}"

# name | method | path — grouped by what they cost, not by file.
ENDPOINTS=(
	"health           |GET |/healthz"
	"ready (db ping)  |GET |/readyz"
	"me               |GET |/api/me"
	"my events        |GET |/api/me/events"
	"my stats         |GET |/api/me/stats"
	"recent events    |GET |/api/me/recent-events"
	"friends          |GET |/api/friends"
	"friend requests  |GET |/api/friends/requests"
	"admin: accounts  |GET |/api/admin/users"
	"admin: feedback  |GET |/api/admin/feedback/open-count"
	"bcp: event info  |GET |/api/events/latency-probe"
	"bcp: roster      |GET |/api/events/latency-probe/players"
)

printf 'Latency map — %s\n' "$BASE"
if [ -n "$SESSION" ]; then
	printf 'runs: %s per endpoint   session: provided\n\n' "$RUNS"
else
	printf 'runs: %s per endpoint   session: none (gated endpoints measure the 401 path)\n\n' "$RUNS"
fi
printf '%-18s %-5s %-34s %8s %8s %8s %8s  %s\n' \
	"endpoint" "verb" "path" "min" "p50" "mean" "max" "status"
printf '%s\n' "------------------------------------------------------------------------------------------------------------"

for entry in "${ENDPOINTS[@]}"; do
	IFS='|' read -r name method path <<< "$entry"
	name="$(echo "$name" | xargs)"; method="$(echo "$method" | xargs)"; path="$(echo "$path" | xargs)"

	times=()
	status=""
	for _ in $(seq 1 "$RUNS"); do
		out="$(curl -s -o /dev/null -w '%{time_total} %{http_code}' \
			-X "$method" --max-time 60 "${cookie_args[@]}" "$BASE$path" 2>/dev/null)"
		t="${out%% *}"; status="${out##* }"
		# Seconds -> integer MICROseconds, without relying on bc.
		# Microseconds rather than milliseconds because this repo's own
		# endpoints answer in under a millisecond locally, and rounding
		# those to "0ms" would hide exactly the regressions a local run
		# is there to catch. Displayed as ms below.
		times+=("$(awk -v t="$t" 'BEGIN{printf "%d", t*1000000}')")
	done

	sorted="$(printf '%s\n' "${times[@]}" | sort -n)"
	min="$(echo "$sorted" | head -1)"
	max="$(echo "$sorted" | tail -1)"
	p50="$(echo "$sorted" | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}')"
	mean="$(printf '%s\n' "${times[@]}" | awk '{s+=$1} END{printf "%d", s/NR}')"

	MEANS="${MEANS}${path}	${mean}
"
	printf '%-18s %-5s %-34s %7s %7s %7s %7s  %s\n' \
		"$name" "$method" "$path" "$(as_ms "$min")" "$(as_ms "$p50")" "$(as_ms "$mean")" "$(as_ms "$max")" "$status"
done

# --- Page critical paths --------------------------------------------
#
# A page's cost isn't one endpoint, it's the waterfall it blocks on.
# Wave 1 fires on mount, in parallel. Wave 2 can't start until the
# sign-in check in wave 1 resolves, so a page's floor is
# max(wave 1) + max(wave 2). Derived from the measurements above rather
# than timed in a browser, so it's the server-side floor: real page time
# adds the JS bundle and render on top.
#
# name | route | wave-1 paths (comma-separated) | wave-2 paths
PAGES=(
	# Home fires everything on mount — nothing here waits for the
	# sign-in check, because none of these endpoints needs anything the
	# check returns.
	"home       |/          |/api/me,/api/friends,/api/friends/requests,/api/me/recent-events,/api/me/events,/api/me/stats|"
	# These three genuinely are two-wave: they skip the fetch entirely
	# for an account that is linked but not yet approved, which they
	# can't know until /api/me answers. That's a deliberate choice (it
	# avoids 403-ing a pending account) — so the fix for them is making
	# the endpoint fast, not removing the wave.
	"my events  |/my-events |/api/me|/api/me/events"
	"calendar   |/calendar  |/api/me|/api/me/events"
	"player stats|/stats    |/api/me|/api/me/stats"
	"friends    |/friends   |/api/me,/api/friends,/api/friends/requests|"
	"login      |/login     |/api/me|"
)

mean_for() {
	printf '%s' "$MEANS" | awk -F'\t' -v p="$1" '$1 == p { print $2; found=1; exit } END { if (!found) print 0 }'
}

# Largest measured mean across a comma-separated list of paths — a wave
# runs in parallel, so its cost is its slowest member, not their sum.
max_of() {
	[ -z "$1" ] && { echo 0; return; }
	biggest=0
	for path in $(printf '%s' "$1" | tr ',' ' '); do
		v="$(mean_for "$path")"
		[ "$v" -gt "$biggest" ] && biggest="$v"
	done
	echo "$biggest"
}

over=0
printf '\n\nPage critical paths (server-side floor: max of wave 1 + max of wave 2)\n'
printf '%-13s %-12s %9s %9s %9s  %s\n' "page" "route" "wave 1" "wave 2" "total" "verdict"
printf '%s\n' "------------------------------------------------------------------------------"
for entry in "${PAGES[@]}"; do
	IFS='|' read -r name route w1 w2 <<< "$entry"
	name="$(echo "$name" | xargs)"; route="$(echo "$route" | xargs)"
	a="$(max_of "$w1")"; b="$(max_of "$w2")"
	total=$((a + b))
	if [ "$total" -gt "$((CRITICAL_MS * 1000))" ]; then
		verdict="CRITICALLY SLOW (>${CRITICAL_MS}ms)"
		over=$((over + 1))
	else
		verdict="ok"
	fi
	printf '%-13s %-12s %8s %8s %8s  %s\n' "$name" "$route" "$(as_ms "$a")" "$(as_ms "$b")" "$(as_ms "$total")" "$verdict"
done

printf '\n'
printf 'Reading the status column:\n'
printf '  200  answered normally\n'
printf '  401  not signed in — the time is routing + a failed session lookup, not the endpoint\n'
printf '  403  signed in but not approved\n'
printf '  404  no such event/record (the bcp:* probes use a deliberately absent id)\n'

if [ "$GATE" -eq 1 ] && [ "$over" -gt 0 ]; then
	printf '\n%s page(s) over %sms — see CLAUDE.md: a page over two seconds is a bug.\n' "$over" "$CRITICAL_MS" >&2
	exit 1
fi
