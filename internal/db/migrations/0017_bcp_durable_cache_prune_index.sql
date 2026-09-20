-- An index for pruning bcp_durable_cache, which nothing ever removed
-- from.
--
-- Most of what the table holds is genuinely permanent (an already-
-- concluded event's info, roster, pairings and placings; a league's
-- classification) and is meant to stay for ever — that is the whole
-- point of it. But three kinds are age-bounded and simply become dead
-- weight once they expire: the two per-user history feeds, and ITC
-- rankings. An event that never ended is the fourth: it was stored with
-- a lifetime, not permanently.
--
-- Without this index the prune is a sequential scan of a table that
-- only ever grew. cached_at leads because the prune is always
-- "everything of these kinds older than X"; cache_key follows so the
-- prefix match can be satisfied from the index too.
CREATE INDEX IF NOT EXISTS bcp_durable_cache_prune_idx
    ON bcp_durable_cache (cached_at, cache_key);
