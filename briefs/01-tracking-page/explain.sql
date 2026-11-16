-- EXPLAIN for brief 1, run with: make explain-01 LABEL=before|index|after
-- Driver 5 was on shift at 17:00 on Tue 10 Nov 2026.
\pset pager off
\echo '== table and index sizes'
SELECT c.relname AS name,
       CASE c.relkind WHEN 'i' THEN 'index' WHEN 'I' THEN 'index' ELSE 'table' END AS kind,
       pg_size_pretty(pg_total_relation_size(c.oid)) AS size,
       c.reltuples::bigint AS rows_estimate
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND c.relname ~ '^(location_pings|driver_positions)'
  AND c.relname !~ '_2026|_2025'
ORDER BY c.relname;

\echo '== tracking page: latest ping for one driver (the query in GET /v1/jobs/{id}/track before the fix)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT driver_id, at, ST_Y(location::geometry), ST_X(location::geometry), speed_kmh, battery_pct
FROM location_pings WHERE driver_id = 5 ORDER BY at DESC LIMIT 1;

\if :{?after}
\echo '== tracking page after the fix: driver_positions by primary key'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT driver_id, at, ST_Y(location::geometry), ST_X(location::geometry), speed_kmh, battery_pct
FROM driver_positions WHERE driver_id = 5;
\endif

\echo '== ops map: every driver''s latest ping in a 5-minute window (GET /v1/positions?at=)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT DISTINCT ON (driver_id) driver_id, at, ST_Y(location::geometry), ST_X(location::geometry), speed_kmh, battery_pct
FROM location_pings
WHERE at > '2026-11-10 16:55:00+00' AND at <= '2026-11-10 17:00:00+00'
ORDER BY driver_id, at DESC;
