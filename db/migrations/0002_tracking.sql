-- GPS pings from drivers, one every 10 seconds per vehicle.
--
-- DELIBERATELY NAIVE (brief 1 "The tracking page crawls at 5pm"):
--   * one unpartitioned table that grows by ~400k rows a day, forever
--   * the only index is on driver_id, so "latest ping for a driver" has to
--     read and sort every ping that driver has ever sent
--   * no latest-position table
-- Brief 1 replaces this with daily partitions, a (driver_id, at DESC) index
-- and a latest-position table. Do not "fix" it before then.

CREATE TABLE location_pings (
    driver_id    bigint NOT NULL REFERENCES drivers(id),
    at           timestamptz NOT NULL,
    location     geography(Point, 4326) NOT NULL,
    speed_kmh    real,
    battery_pct  real
);
CREATE INDEX location_pings_driver_idx ON location_pings (driver_id);

-- Proof of delivery: photo or signature captured by the driver app.
CREATE TABLE proofs_of_delivery (
    id           bigserial PRIMARY KEY,
    job_id       bigint NOT NULL REFERENCES jobs(id),
    driver_id    bigint NOT NULL REFERENCES drivers(id),
    kind         text NOT NULL CHECK (kind IN ('photo', 'signature', 'safe_place')),
    object_url   text,
    recipient    text,
    captured_at  timestamptz NOT NULL
);
CREATE INDEX proofs_job_idx ON proofs_of_delivery (job_id);
