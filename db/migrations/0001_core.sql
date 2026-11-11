-- GekGo core schema: places, customers, fleet and jobs.
-- Money is always integer pence. Times are timestamptz (UTC in the database).

CREATE EXTENSION IF NOT EXISTS postgis;

CREATE TABLE depots (
    id        bigserial PRIMARY KEY,
    name      text NOT NULL UNIQUE,
    city      text NOT NULL CHECK (city IN ('glasgow', 'edinburgh')),
    location  geography(Point, 4326) NOT NULL
);

-- Pricing and dispatch boundaries. Rings around each city centre;
-- a point belongs to the zone whose area covers it.
CREATE TABLE zones (
    id               bigserial PRIMARY KEY,
    depot_id         bigint NOT NULL REFERENCES depots(id),
    name             text NOT NULL UNIQUE,
    area             geography(Polygon, 4326) NOT NULL,
    surcharge_pence  integer NOT NULL CHECK (surcharge_pence >= 0)
);
CREATE INDEX zones_area_gix ON zones USING gist (area);

CREATE TABLE addresses (
    id        bigserial PRIMARY KEY,
    line1     text NOT NULL,
    line2     text,
    city      text NOT NULL,
    postcode  text NOT NULL,
    location  geography(Point, 4326) NOT NULL
);
CREATE INDEX addresses_location_gix ON addresses USING gist (location);

CREATE TABLE accounts (
    id                  bigserial PRIMARY KEY,
    name                text NOT NULL,
    sector              text NOT NULL,
    tier                text NOT NULL CHECK (tier IN ('standard', 'plus', 'enterprise')),
    billing_terms       text NOT NULL CHECK (billing_terms IN ('prepay', 'net14', 'net30')),
    home_address_id     bigint REFERENCES addresses(id),
    api_key_hash        bytea UNIQUE,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- People who receive parcels, or book through the consumer app.
CREATE TABLE customers (
    id          bigserial PRIMARY KEY,
    name        text NOT NULL,
    phone       text NOT NULL,
    email       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE drivers (
    id               bigserial PRIMARY KEY,
    name             text NOT NULL,
    employment_type  text NOT NULL CHECK (employment_type IN ('employed', 'self_employed')),
    home_depot_id    bigint NOT NULL REFERENCES depots(id)
);

CREATE TABLE vehicles (
    id             bigserial PRIMARY KEY,
    registration   text NOT NULL UNIQUE,
    type           text NOT NULL CHECK (type IN ('van', 'ebike')),
    is_electric    boolean NOT NULL,
    range_km       integer,
    battery_kwh    numeric(5,1),
    depot_id       bigint NOT NULL REFERENCES depots(id)
);

CREATE TABLE shifts (
    id          bigserial PRIMARY KEY,
    driver_id   bigint NOT NULL REFERENCES drivers(id),
    vehicle_id  bigint NOT NULL REFERENCES vehicles(id),
    starts_at   timestamptz NOT NULL,
    ends_at     timestamptz NOT NULL,
    CHECK (ends_at > starts_at)
);
CREATE INDEX shifts_driver_idx ON shifts (driver_id, starts_at);

CREATE TABLE jobs (
    id            bigserial PRIMARY KEY,
    account_id    bigint REFERENCES accounts(id),          -- NULL for consumer bookings
    customer_id   bigint NOT NULL REFERENCES customers(id), -- the recipient
    pickup_id     bigint NOT NULL REFERENCES addresses(id),
    dropoff_id    bigint NOT NULL REFERENCES addresses(id),
    service       text NOT NULL CHECK (service IN ('two_hour', 'same_day', 'scheduled')),
    window_start  timestamptz NOT NULL,
    window_end    timestamptz NOT NULL,
    status        text NOT NULL CHECK (status IN (
                    'created', 'collected', 'at_depot', 'out_for_delivery',
                    'delivered', 'failed', 'returned', 'cancelled')),
    driver_id     bigint REFERENCES drivers(id),            -- current assignee, if any
    price_pence   integer NOT NULL CHECK (price_pence >= 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (window_end > window_start)
);
CREATE INDEX jobs_account_idx ON jobs (account_id, created_at);
CREATE INDEX jobs_status_idx  ON jobs (status) WHERE status NOT IN ('delivered', 'returned', 'cancelled');

-- Append-only status history. Never UPDATE or DELETE rows here.
CREATE TABLE job_events (
    id       bigserial PRIMARY KEY,
    job_id   bigint NOT NULL REFERENCES jobs(id),
    type     text NOT NULL,
    at       timestamptz NOT NULL,
    actor    text NOT NULL,          -- 'customer', 'system', 'driver:<id>', 'ops:<name>'
    payload  jsonb
);
CREATE INDEX job_events_job_idx ON job_events (job_id, at);

-- Routes are filled in by dispatch (weeks 3–4); created now so the shape is fixed.
CREATE TABLE routes (
    id          bigserial PRIMARY KEY,
    shift_id    bigint NOT NULL REFERENCES shifts(id),
    planned_km  numeric(7,2),
    planned_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE route_stops (
    route_id    bigint NOT NULL REFERENCES routes(id),
    job_id      bigint NOT NULL REFERENCES jobs(id),
    seq         integer NOT NULL,
    eta         timestamptz,
    arrived_at  timestamptz,
    PRIMARY KEY (route_id, seq)
);
