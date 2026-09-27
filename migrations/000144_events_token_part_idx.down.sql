-- Drops events_token_part_idx without blocking inserts into events. A
-- single statement, so golang-migrate sends it on its own, outside any
-- transaction block, which CONCURRENTLY requires. Unlike a concurrent
-- build (see the up migration), a concurrent drop waits only for
-- transactions holding a lock on events, never for older snapshots, so a
-- second migrator waiting on golang-migrate's advisory lock cannot
-- deadlock it.
DROP INDEX CONCURRENTLY IF EXISTS events_token_part_idx;
