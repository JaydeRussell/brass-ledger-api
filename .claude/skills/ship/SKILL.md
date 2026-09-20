---
name: ship
description: Commit the current working-tree changes, push a feature branch, open a PR following this project's conventions, wait for CI, and squash-merge it — the branch-to-merge choreography used throughout this project. Optionally cut a release afterward per the sibling brass-ledger-web repo's CLAUDE.md "Releases" section (this repo has no version of its own). Use when the user says "ship this," "let's push them up," "get it merged," "open a PR," or otherwise asks to land the current changes.
---

# Shipping a change in this project

This project always lands work on `main` the same way: feature branch →
commit → push → PR → wait for CI → squash-merge with `--delete-branch`.
Never push directly to `main`. This is an explicit action, not a
default — only run this when the user actually asks for it (same rule
as committing at all; see the top-level git-safety instructions).

## Before starting

- Run this repo's verification commands first — `go build ./...`, `go
  vet ./...`, `go test ./...` — and don't open a PR on code that hasn't
  passed all of them.
- `go vet` is not what CI runs. CI runs golangci-lint, which catches a
  strictly larger set (`staticcheck`, `unused`, `errcheck`), and it is
  not installed by default — so a clean `go vet` locally still fails CI
  on things like an unused helper or `Write([]byte(fmt.Sprintf(...)))`.
  Run `make lint-ci` before pushing; it fetches the exact pinned version
  CI uses.
- If the change touches request handling, caching, or anything either
  history feed reaches, also run `make latency` against the local stack
  and read the page table at the bottom. Anything flagged over
  `CRITICAL_MS` is a bug by this project's own rule — see CLAUDE.md's
  "A page over two seconds is a bug". Local numbers are a floor, not a
  user experience (see the script's header for why), so treat a
  regression *relative to the last run* as the signal, not the absolute
  value.
- `git status`/`git diff` to see exactly what's changing. Stage only the
  files that belong to this change — never a blanket `git add -A`
  without reviewing what it actually picked up (check for anything that
  looks like it could hold a secret before pushing).
- If the change touches both this repo and the sibling
  `brass-ledger-web`, ship each repo as its own PR — this project has
  never used a single cross-repo PR, and each repo's CI/branch
  protection is independent (this repo's CI also runs a real Postgres
  `integration` check, in addition to `test`).

## The sequence

1. `git checkout -b <descriptive-branch-name>` — a short kebab-case name
   describing the change (e.g. `feat/feedback-widget-backend`), not a
   generic `work` or `fix`.
2. Stage the specific files and commit. Message describes *why*, not a
   diff narration, and ends with:
   ```
   Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
   Claude-Session: <this session's URL>
   ```
3. `git push -u origin <branch>`.
4. `gh pr create` — title under ~70 characters. Body has `## Summary`
   (why this change, referencing a companion PR in the other repo if
   there is one) and `## Test plan` (checked boxes for what was actually
   verified — including any live click-through, not just automated
   checks). Ends with the `🤖 Generated with [Claude Code]` footer and
   the session link, same as the commit.
5. Poll `gh pr checks <number>` until nothing is `pending` — both `test`
   and `integration` need to report before this is done. A short sleep
   loop between checks is fine; don't guess at timing.
6. If every check passed: `gh pr merge <number> --squash
   --delete-branch`. If anything failed, stop and report it — never
   merge a red PR, and never use `--admin` to bypass a failing or
   still-pending required check.
7. Confirm the local `main` fast-forwarded cleanly (`git status`, `git
   log -1`) — `gh pr merge` updates the local branch automatically when
   you're on the branch that just got merged.

## Releasing afterward (only if asked)

This repo carries no version of its own — Brass Ledger's version lives
in `brass-ledger-web`'s `package.json`, covering whatever changed in
either or both repos since the last release. Read the sibling repo's
`CLAUDE.md` "Releases" section fresh each time — it's the canonical
policy (when to bump, how to size minor vs. patch, the changelog entry
format, and the tag-after-merge rule, including tagging *this* repo's
commit too when it has one for the release) — rather than relying on
this skill's memory of it, since the two can drift apart over time.
