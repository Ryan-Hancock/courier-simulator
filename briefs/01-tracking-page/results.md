# Brief 1 results: "The tracking page crawls at 5pm."

Measured on Mon 16 Nov 2026 on one laptop (16 cores, 15 GB RAM, Docker, Postgres 16 + PostGIS
with `shared_buffers=512MB`), against seed 42 with history to 10 Nov 2026 and 18 months of GPS
pings (123,081,396 rows). Raw data: `articles/obsidian/01-tracking-page/data/`.

## Summary

| At 17:00 | Before | Index only (A) | After (B) |
| --- | ---: | ---: | ---: |
| Tracking requests answered within 10 s | 0.4% | 100% | 100% |
| Tracking p95 | >10 s (timed out) | 2.4 ms | 2.5 ms |
| Driver ping p95 | 8.6 s | 3.1 ms | 3.2 ms |
| Ops map query, one 5-minute window (EXPLAIN) | 2,248 ms | 2,025 ms | 3.0 ms |
| Replay range (`GET /v1/pings/range`) | full table scan | full table scan | 36 ms |

- **The tracking page was down, not slow.** At every hour from 09:00 to 19:00, only 0.1–2.6% of
  requests were answered within 10 s, even at 7 requests a second.
- **The brief's timing was off.** Parcels on the road peak at 12:00 (2,230), not 17:00 (543). At
  5pm people get home, look, and phone.
- **One index fixed the page.** `(driver_id, at DESC)` alone took the tracking page from
  timing out to 2.4 ms at p95 at 17:00. Most of the article's headline comes from this one line.
- **The rest of the fix is about the next 18 months.** Partitions, BRIN and `driver_positions`
  fixed the ops map and replay (no time index before), made retention a `DETACH` instead of a
  giant `DELETE`, and took the tracking page off the ping history entirely.

## Method

`geksim bench` replays one moment of Tue 10 Nov 2026 for 60 s, once for every hour from 09:00
to 19:00:

1. **Freeze.** Every job in progress at that moment is put back into its status at the time,
   using `job_events`.
2. **Load.** It polls `GET /v1/jobs/{id}/track` at a fixed rate: open loop, so a slow server
   can't slow the load down. Latency is counted from when each request was due. Meanwhile,
   every driver on shift posts a ping every 10 s.
3. **Restore.** Statuses go back to normal, and the bench's own pings are deleted.

**Traffic model (an assumption).** The tracking link goes out by SMS when a parcel leaves the
depot. Half of recipients have the page open, and it refreshes every 15 s. That gives
4.5 requests a second at 19:00 and 74.3 at 12:00. A request slower than 10 s counts as failed.

## Before

- `location_pings`: 123,081,396 rows, 10 GB, with one index on `driver_id` (758 MB).
- Finding driver 5's latest ping, idle database: **1,966 ms**. The planner chose a parallel
  sequential scan: 1,269,087 pages (9.7 GB), 95% read from disk, to sort 1,295,538 pings and
  keep 1. Pings arrive in time order, so one driver's rows sit on almost every page, and the
  `driver_id` index doesn't help.
- Under load, requests queue for the connection pool and almost none finish. Driver pings wait
  in the same queue, with a p95 of 7.2–10 s. That's Morag's "the drivers' app seems to get
  slower too".

| Hour | Out for delivery | Requests/s | Answered ≤10 s | Ping p95 |
| --- | ---: | ---: | ---: | ---: |
| 09:00 | 209 | 7.0 | 1.7% | 7.2 s |
| 12:00 | 2,230 | 74.3 | 0.1% | 10.0 s |
| 17:00 | 543 | 18.1 | 0.4% | 8.6 s |
| 19:00 | 134 | 4.5 | 2.6% | (no drivers on shift) |

## Step A: one index

```sql
CREATE INDEX CONCURRENTLY location_pings_driver_at_idx ON location_pings (driver_id, at DESC);
```

- Took 2 min 6 s, without blocking writes. Size: 3.7 GB.
- Latest ping for one driver: one index entry and one row (index scan 0.9 ms; 8 buffers).
- Tracking p95: 2.4–3.3 ms at every hour, and 0 failed. Ping p95: 2.6–4.1 ms.
- Not fixed: the ops map's 5-minute window is still a 10 GB scan (2,025 ms), and the
  table still grows forever.

## Step B: partitions, BRIN, `driver_positions`

Migration `db/migrations/0004_ping_partitions.sql`:

- Renames the old table, attaches it as the historical partition of a new `PARTITION BY RANGE (at)`
  table, and creates daily partitions from tomorrow onwards. **0 rows copied, 0 deleted.**
- `(driver_id, at DESC)` on every partition. On the historical partition, it reuses step A's index.
- BRIN on `at`: **376 kB** for 18 months of pings.
- `driver_positions`: one row per driver. Ingest writes the pings and moves the position forward
  in one statement. The tracking page reads one row by key (0.022 ms in EXPLAIN).
- `ensure_ping_partitions()` creates the next 7 days. tracking-ingest calls it at startup and every
  hour.
- Took **20.6 s** on the 10 GB table, with step A's index already built. This includes
  validating the `CHECK` constraint and building the BRIN index, and the ping table is locked
  throughout. Run it in a quiet hour.

| Hour | Answered ≤10 s | Tracking p95 | Ping p95 |
| --- | ---: | ---: | ---: |
| 09:00 | 100% | 2.6 ms | 3.3 ms |
| 12:00 | 100% | 2.4 ms | 90 ms |
| 17:00 | 100% | 2.5 ms | 3.2 ms |
| 19:00 | 100% | 2.5 ms | (no drivers on shift) |

Ping p95 jumped to 90 ms at 12:00 and 44 ms at 14:00, and stayed between 2.9 and 3.3 ms at every other hour. The
10:00 run also had one slow tail (p99 487 ms). This is probably a checkpoint or autovacuum during
those minutes, and I haven't investigated it.

## Costs and trade-offs

- **Storage went up.** The new index is 3.7 GB and replaced the 758 MB one: +2.9 GB on
  18 months of pings. The historical partition is now 13 GB including indexes.
- **History before the cutover is still one big partition.** Splitting it into days, or
  exporting and detaching old months, is a separate job. Brief 11 needs that history.
- **The migration locks the ping table for ~20 s.** Build step A's index concurrently first,
  as above, or the lock lasts as long as the index build.

## Not yet measured

- How the page degraded as history grew (bench at 1–26 weeks of pings).
- Simulator quirk: 134 parcels are still out for delivery at 19:00 with no driver on shift.
  Deliveries can run past a shift's end.

## Reproduce

```sh
git checkout brief-01-before
make up && make seed-brief-01            # ~21 min
make bench-01 RUN=before && make explain-01 LABEL=before
docker compose exec postgres psql -U gekgo -c \
  "CREATE INDEX CONCURRENTLY location_pings_driver_at_idx ON location_pings (driver_id, at DESC)"
make bench-01 RUN=index && make explain-01 LABEL=index
git checkout brief-01-after && make up   # runs migration 0004
make bench-01 RUN=after && make explain-01 LABEL=after
```
