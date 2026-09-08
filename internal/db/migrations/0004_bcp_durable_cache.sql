-- A durable, cross-restart, cross-user cache for BCP responses that are
-- genuinely immutable once written — an already-concluded event's info,
-- roster, pairings, and placings never change again, and league metadata
-- (gw_itc/hobby classification) is effectively permanent reference data.
-- See internal/bcp/durable.go for exactly what gets written here and
-- when: only once a caller has confirmed the underlying data can never
-- change, never speculatively.
--
-- Why this exists: internal/bcp's in-memory Cache (cache.go) only lives
-- as long as the process and only holds a value for
-- minRefetchInterval (60s) — fine for data that's genuinely live, but it
-- meant an already-decided event's info/roster/pairings/placings were
-- being re-fetched from BCP every 60 seconds by every visitor, and from
-- scratch again after every deploy. Persisting the immutable subset here
-- turns "every visitor, every 60s, forever" into "once, ever, for
-- everyone" — see CLAUDE.md's "Be respectful of BCP's API" rule.
--
-- One generic key-value table (rather than a table per BCP response
-- shape) so adding another durable-cacheable kind later doesn't need a
-- new migration — cache_key is namespaced by kind (e.g.
-- "event:AbC123", "league:XyZ789") to keep different kinds from
-- colliding.
CREATE TABLE IF NOT EXISTS bcp_durable_cache (
    cache_key  TEXT PRIMARY KEY,
    data       JSONB NOT NULL,
    cached_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
