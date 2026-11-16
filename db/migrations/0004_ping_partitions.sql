-- Brief 1 "The tracking page crawls at 5pm": the fix.
--
--   * location_pings becomes PARTITION BY RANGE (at), one partition per day
--     (Europe/London days). The old table is attached as one historical
--     partition, so no rows are copied and no history is lost.
--   * every partition has (driver_id, at DESC) for "a driver's latest pings"
--     and a BRIN index on at for time windows (pings arrive in time order,
--     so BRIN is tiny).
--   * driver_positions keeps one row per driver: the tracking page reads it
--     by primary key and never touches the ping history.
--
-- This runs in one transaction and locks the ping table while the constraint
-- is checked and the BRIN index is built. On a live system, build
-- (driver_id, at DESC) first with CREATE INDEX CONCURRENTLY (option A; this
-- migration then reuses it) and run the rest in a quiet hour.

ALTER TABLE location_pings RENAME TO location_pings_legacy;
-- Option A's index, if it was built ahead of time: renamed out of the way so
-- the parent index below can attach it instead of building a new one.
ALTER INDEX IF EXISTS location_pings_driver_at_idx RENAME TO location_pings_legacy_driver_at_idx;

CREATE TABLE location_pings (
    driver_id    bigint NOT NULL REFERENCES drivers(id),
    at           timestamptz NOT NULL,
    location     geography(Point, 4326) NOT NULL,
    speed_kmh    real,
    battery_pct  real
) PARTITION BY RANGE (at);

-- The old table holds everything before tomorrow (London time).
DO $$
DECLARE
    cutover timestamptz := (date_trunc('day', now() AT TIME ZONE 'Europe/London') + interval '1 day') AT TIME ZONE 'Europe/London';
BEGIN
    EXECUTE format('ALTER TABLE location_pings_legacy ADD CONSTRAINT location_pings_legacy_at_check CHECK (at < %L)', cutover);
    EXECUTE format('ALTER TABLE location_pings ATTACH PARTITION location_pings_legacy FOR VALUES FROM (MINVALUE) TO (%L)', cutover);
END $$;

-- Indexes on the parent cascade to every partition, now and future.
CREATE INDEX location_pings_driver_at_idx ON location_pings (driver_id, at DESC);
CREATE INDEX location_pings_at_brin ON location_pings USING brin (at);
DROP INDEX location_pings_driver_idx; -- driver_id alone: now redundant

-- ensure_ping_partitions creates a daily partition for every day from
-- from_day to today + days_ahead, skipping days that already have one or
-- that the historical partition covers. tracking-ingest calls it at startup
-- and every hour; geksim calls it before seeding pings.
CREATE FUNCTION ensure_ping_partitions(from_day date, days_ahead int) RETURNS int
LANGUAGE plpgsql AS $$
DECLARE
    d       date;
    created int := 0;
BEGIN
    FOR d IN SELECT generate_series(from_day, (now() AT TIME ZONE 'Europe/London')::date + days_ahead, interval '1 day')::date LOOP
        CONTINUE WHEN to_regclass(format('location_pings_%s', to_char(d, 'YYYYMMDD'))) IS NOT NULL;
        BEGIN
            EXECUTE format('CREATE TABLE location_pings_%s PARTITION OF location_pings FOR VALUES FROM (%L) TO (%L)',
                to_char(d, 'YYYYMMDD'),
                d::timestamp AT TIME ZONE 'Europe/London',
                (d + 1)::timestamp AT TIME ZONE 'Europe/London');
            created := created + 1;
        EXCEPTION WHEN invalid_object_definition THEN
            NULL; -- inside the historical partition's range
        END;
    END LOOP;
    RETURN created;
END $$;

SELECT ensure_ping_partitions((now() AT TIME ZONE 'Europe/London')::date, 7);

-- One row per driver: their latest ping.
CREATE TABLE driver_positions (
    driver_id    bigint PRIMARY KEY REFERENCES drivers(id),
    at           timestamptz NOT NULL,
    location     geography(Point, 4326) NOT NULL,
    speed_kmh    real,
    battery_pct  real
);

INSERT INTO driver_positions
SELECT p.*
FROM drivers d
CROSS JOIN LATERAL (
    SELECT driver_id, at, location, speed_kmh, battery_pct
    FROM location_pings WHERE driver_id = d.id ORDER BY at DESC LIMIT 1
) p;
